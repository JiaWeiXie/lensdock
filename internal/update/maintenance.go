package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// InitDeployment prepares the directories and backup key of dep. It refuses
// to replace an existing key. Deployment.Save is the caller's responsibility
// when creating a new deployment file.
func InitDeployment(dep *Deployment) (keyID string, created bool, err error) {
	if err := dep.ensureDirs(); err != nil {
		return "", false, err
	}
	key, err := LoadBackupKey(dep.BackupKeyFile)
	if err == nil {
		return KeyID(key), false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	id, err := GenerateBackupKey(dep.BackupKeyFile)
	return id, err == nil, err
}

// VerifyCheckpoint deep-verifies a checkpoint and marks its verification time.
func (e *Engine) VerifyCheckpoint(id string) (Checkpoint, error) {
	key, err := LoadBackupKey(e.Dep.BackupKeyFile)
	if err != nil {
		return Checkpoint{}, err
	}
	store := CheckpointStore{Dir: e.Dep.BackupDir}
	cp, err := store.Verify(id, key, true)
	if err != nil {
		return cp, err
	}
	if err := store.MarkVerified(id, e.Now()); err != nil {
		return cp, err
	}
	return cp, nil
}

// PruneCheckpoints applies the configured retention policy. It protects the
// same checkpoints age expiry does (unfinished operations and the one paired
// with the running generation) and, like expiry, refuses to delete anything
// when state or journals are unreadable or the running generation's producer
// journal is missing or inconsistent: the count policy must not discard the
// rollback checkpoint just because its record is gone.
func (e *Engine) PruneCheckpoints() ([]string, error) {
	unlock, err := lockFile(e.Dep.lockPath())
	if err != nil {
		return nil, err
	}
	defer unlock()
	w, err := loadExpiryWorld(e.Dep, e.Now)
	if err != nil {
		return nil, err
	}
	return (CheckpointStore{Dir: e.Dep.BackupDir}).Prune(e.Dep.RetainCheckpoints, w.protectedCheckpoints())
}

// DeleteBranch explicitly deletes a managed branch volume. It refuses a volume
// used by the current generation or referenced by an unfinished operation.
func (e *Engine) DeleteBranch(ctx context.Context, volume string) error {
	if err := checkDockerName("volume", volume); err != nil {
		return err
	}
	unlock, err := lockFile(e.Dep.lockPath())
	if err != nil {
		return err
	}
	defer unlock()
	st, _, _, err := e.resolveState(ctx)
	if err != nil {
		return err
	}
	for _, c := range st.Components {
		for _, v := range c.Volumes {
			if v == volume {
				return fmt.Errorf("volume %s is used by the running generation", volume)
			}
		}
	}
	js, _ := listJournals(e.Dep, e.Now)
	knownRetired := false
	for _, j := range js {
		if contains(j.RetiredVolumes, volume) {
			knownRetired = true
		}
		if j.Status != JournalRunning && j.Status != JournalAwaitingApproval && j.Status != JournalHeld {
			continue
		}
		if j.Candidate != nil {
			for _, v := range j.Candidate.Volumes {
				if v == volume {
					return fmt.Errorf("volume %s is referenced by unfinished %s %s", volume, j.Kind, j.ID)
				}
			}
			if contains(j.Candidate.Scratch, volume) {
				return fmt.Errorf("volume %s is referenced by unfinished %s %s", volume, j.Kind, j.ID)
			}
		}
	}
	info, err := e.Docker.VolumeInspect(ctx, volume)
	if err != nil {
		return err
	}
	if info.Labels[LabelManaged] != "true" && !knownRetired {
		return fmt.Errorf("refusing to remove volume %s: it is neither managed by lensdock update nor recorded as a retired branch", volume)
	}
	if !knownRetired && !volumeOwned(volume, info.Labels, js) {
		return fmt.Errorf("refusing to remove volume %s: no operation of this deployment created or retired it", volume)
	}
	if knownRetired {
		return e.Docker.do(ctx, "volume", "rm", volume)
	}
	return e.Docker.VolumeRemove(ctx, volume)
}

var allowedComposeCommands = map[string]bool{
	"up": true, "down": true, "stop": true, "start": true, "restart": true,
	"ps": true, "logs": true, "config": true,
}

// ComposeCommand returns the exact docker argv that operates the current
// generation: base files + persisted env files + generated generation
// override. args must begin with a compose subcommand; global flags are not
// admitted because the deployment owns its project/files/env-files.
func (e *Engine) ComposeCommand(args []string) (string, []string, error) {
	if len(args) == 0 || !allowedComposeCommands[args[0]] {
		return "", nil, fmt.Errorf("compose command must start with one of: up, down, stop, start, restart, ps, logs, config")
	}
	for _, a := range args[1:] {
		if args[0] == "down" && (a == "-v" || a == "--volumes" || a == "--rmi" || strings.HasPrefix(a, "--rmi=")) {
			return "", nil, fmt.Errorf("%s is refused: lensdock update never deletes volumes or release images implicitly", a)
		}
		if args[0] == "up" && (a == "--build" || a == "--pull" || strings.HasPrefix(a, "--pull=") && a != "--pull=never") {
			return "", nil, fmt.Errorf("%s is refused: the generation uses pre-verified digest-pinned images (use --pull=never)", a)
		}
	}
	for _, a := range args {
		if strings.ContainsRune(a, 0) || strings.ContainsAny(a, "\r\n") {
			return "", nil, errors.New("compose argument contains a control character")
		}
	}
	st, tracked, err := LoadState(e.Dep)
	if err != nil {
		return "", nil, err
	}
	c := e.Dep.composeFor(e.Docker, st, tracked)
	return e.Docker.bin(), c.Args(args...), nil
}

// PublishCandidates writes current status including a fresh update candidate
// scan. Invalid releases are present with issues; no candidate changes a
// running service.
func (e *Engine) PublishCandidates(ctx context.Context, workspace string) (PublicStatus, error) {
	candidates, err := e.Check(ctx, workspace)
	if err != nil {
		return PublicStatus{}, err
	}
	status, err := e.Status(ctx)
	if err != nil {
		return status, err
	}
	status.Update.Candidates = candidates
	if len(candidates) > 0 && status.Update.Phase == "idle" {
		status.Update.Phase = "candidate-available"
	}
	if e.Dep.PublicStatusFile != "" {
		if err := os.MkdirAll(filepath.Dir(e.Dep.PublicStatusFile), 0o755); err != nil {
			return status, err
		}
		if err := writeJSONAtomic(e.Dep.PublicStatusFile, status, 0o644); err != nil {
			return status, err
		}
	}
	return status, nil
}
