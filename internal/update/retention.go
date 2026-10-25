package update

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Image retention
//
// A generation runs its images by repository@digest (the compose override
// pins them). Whether the engine can still resolve such a reference depends on
// the names the image carries in that repository: when `docker build -t
// repo:dev` is repeated, :dev moves to the new image and the old image keeps
// no name in the repository, so repo@digest stops resolving even though the
// content is still stored. A tag in another repository does not help.
//
// Every image a restore or rollback may start is therefore kept with a stable
// tag inside its own repository that is unique to its full digest:
//
//	repository:lensdock-keep-<64 hex digits>
//
// This happens before anything is stopped, so a restore can never discover a
// missing image late.

// keepTagPrefix starts the name of a keep tag; the full digest follows.
const keepTagPrefix = "lensdock-keep-"

// splitPinnedRef splits repository@sha256:<64 hex> and returns the repository
// and the bare hex digest. The repository carries neither a tag nor a digest.
func splitPinnedRef(ref string) (repo, hex string, err error) {
	repo, digest, ok := strings.Cut(ref, "@")
	if !ok || repo == "" || repo != repoOf(repo) || !digestPattern.MatchString(digest) {
		return "", "", fmt.Errorf("image %q is not a repository@sha256:<digest> reference", ref)
	}
	return repo, strings.TrimPrefix(digest, "sha256:"), nil
}

// keepTag is the local tag, inside the image's own repository, that keeps
// ref (repository@digest) resolvable. It is derived from the full digest, so
// two images never share one.
func keepTag(ref string) (string, error) {
	repo, hex, err := splitPinnedRef(ref)
	if err != nil {
		return "", err
	}
	return repo + ":" + keepTagPrefix + hex, nil
}

// legacyKeepTag is the tag earlier versions wrote: another repository
// (lensdock-keep/<name>) and only twelve digest digits. It keeps the content
// but never makes repository@digest resolvable. It is read only to recover a
// previous image that has already lost its name; nothing writes it any more.
func legacyKeepTag(ref string) (string, error) {
	repo, hex, err := splitPinnedRef(ref)
	if err != nil {
		return "", err
	}
	return "lensdock-keep/" + repo[strings.LastIndex(repo, "/")+1:] + ":" + hex[:12], nil
}

// retainTarget is one image to keep.
type retainTarget struct {
	// Name identifies the owner in messages (a component id).
	Name string
	// Ref is repository@digest and Digest the digest it must resolve to.
	Ref, Digest string
	// Recover admits a local copy of an image whose Ref no longer resolves.
	// Only images that really ran before (the previous generation) qualify;
	// a release image that is missing is an operator problem to fix by
	// pulling it.
	Recover bool
}

// componentTargets lists the images of comps in a stable order.
func componentTargets(comps map[string]ComponentState, recoverMissing bool) []retainTarget {
	out := make([]retainTarget, 0, len(comps))
	for _, id := range sortedKeys(comps) {
		c := comps[id]
		out = append(out, retainTarget{Name: id, Ref: c.Image, Digest: c.ImageID, Recover: recoverMissing})
	}
	return out
}

// retention is the outcome of inspecting one target; nothing was changed.
type retention struct {
	retainTarget
	// Keep is the in-repository keep tag.
	Keep string
	// From is the local reference that holds exactly Digest today: Ref
	// itself, or (when Ref no longer resolves) a verified local copy.
	From string
	// HaveKeep reports that Keep already names exactly Digest.
	HaveKeep bool
}

// recovers reports that Ref does not resolve today.
func (r retention) recovers() bool { return r.From != r.Ref }

// planRetention inspects, without changing anything, how t can be kept. It
// fails when no local image with exactly t.Digest exists, when the image is
// for another platform, or when its keep tag already names another image.
func (e *Engine) planRetention(ctx context.Context, t retainTarget, arch string) (retention, error) {
	_, hex, err := splitPinnedRef(t.Ref)
	if err != nil {
		return retention{}, err
	}
	if "sha256:"+hex != t.Digest {
		return retention{}, fmt.Errorf("the state records image %s with digest %s, which is not the digest in the reference", t.Ref, t.Digest)
	}
	keep, err := keepTag(t.Ref)
	if err != nil {
		return retention{}, err
	}
	p := retention{retainTarget: t, Keep: keep}

	info, err := e.Docker.ImageInspect(ctx, t.Ref)
	switch {
	case err == nil:
		if err := checkImageIdentity(info, t.Ref, t.Digest, arch); err != nil {
			return retention{}, err
		}
		p.From = t.Ref
	case errors.Is(err, ErrNotFound):
		if !t.Recover {
			return retention{}, fmt.Errorf("image %s is not present locally; pull this exact digest first: docker pull %s", t.Ref, t.Ref)
		}
		if p.From, err = e.recoverySource(ctx, t, arch); err != nil {
			return retention{}, err
		}
	default:
		return retention{}, err
	}

	// A keep tag is named after the full digest. If it names something else
	// it was moved by hand; tagging would silently move it again.
	kinfo, err := e.Docker.ImageInspect(ctx, keep)
	switch {
	case err == nil:
		if !kinfo.HasDigest(t.Digest) {
			return retention{}, fmt.Errorf("keep tag %s names another image (%s) than %s: refusing to move it", keep, kinfo.ID, t.Digest)
		}
		p.HaveKeep = true
	case !errors.Is(err, ErrNotFound):
		return retention{}, err
	}
	return p, nil
}

// recoverySource finds a local image holding exactly t.Digest for a Ref that
// no longer resolves. Only content addressed sources count: the tag earlier
// versions kept in the lensdock-keep repository and the digest itself (which
// engines with the containerd image store resolve to the stored image). Each
// candidate must carry exactly the pinned digest on the engine's platform;
// nothing is pulled and no mutable tag is consulted.
func (e *Engine) recoverySource(ctx context.Context, t retainTarget, arch string) (string, error) {
	legacy, err := legacyKeepTag(t.Ref)
	if err != nil {
		return "", err
	}
	var notes []string
	for _, src := range []string{legacy, t.Digest} {
		info, err := e.Docker.ImageInspect(ctx, src)
		switch {
		case errors.Is(err, ErrNotFound):
			notes = append(notes, src+" is absent")
			continue
		case err != nil:
			return "", err
		}
		if err := checkImageIdentity(info, src, t.Digest, arch); err != nil {
			notes = append(notes, err.Error())
			continue
		}
		return src, nil
	}
	return "", fmt.Errorf("image %s is not present locally and no local copy with that exact digest exists (%s); make exactly this image available again (docker pull %s for a published repository) before continuing", t.Ref, strings.Join(notes, "; "), t.Ref)
}

// planRetentions plans every target (duplicates by reference once) and
// reports all problems together, so nothing is changed unless every image can
// be kept.
func (e *Engine) planRetentions(ctx context.Context, arch string, targets []retainTarget) ([]retention, error) {
	var plans []retention
	var errs []error
	seen := map[string]bool{}
	for _, t := range targets {
		if seen[t.Ref] {
			continue
		}
		seen[t.Ref] = true
		p, err := e.planRetention(ctx, t, arch)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.Name, err))
			continue
		}
		plans = append(plans, p)
	}
	return plans, errors.Join(errs...)
}

// keep carries out a plan: it tags the verified source with the keep tag and
// then proves that both Ref and Keep resolve to exactly the pinned image.
func (e *Engine) keep(ctx context.Context, p retention, arch string) error {
	if err := e.Docker.Tag(ctx, p.From, p.Keep); err != nil {
		return fmt.Errorf("tag %s as %s: %w", p.From, p.Keep, err)
	}
	for _, name := range []string{p.Ref, p.Keep} {
		info, err := e.Docker.ImageInspect(ctx, name)
		if err != nil {
			return fmt.Errorf("verify %s after tagging: %w", name, err)
		}
		if err := checkImageIdentity(info, name, p.Digest, arch); err != nil {
			return err
		}
	}
	return nil
}

// retainImages makes every target resolvable by repository@digest for as
// long as its keep tag exists. All targets are inspected first; a target that
// cannot be kept (missing, wrong digest, wrong platform) fails the call
// before any tag is written. It returns a one-line summary for the journal.
func (e *Engine) retainImages(ctx context.Context, arch string, targets []retainTarget) (string, error) {
	plans, err := e.planRetentions(ctx, arch, targets)
	if err != nil {
		return "", err
	}
	var tagged, recovered int
	for _, p := range plans {
		if p.HaveKeep && !p.recovers() {
			continue
		}
		if p.recovers() {
			recovered++
			e.logf("image %s no longer resolves; re-tagging the local copy %s (exact digest verified) as %s", p.Ref, p.From, p.Keep)
		}
		if err := e.keep(ctx, p, arch); err != nil {
			return "", fmt.Errorf("%s: %w", p.Name, err)
		}
		tagged++
	}
	return fmt.Sprintf("%d images kept under their own repositories (%d newly tagged, %d recovered from a verified local copy)", len(plans), tagged, recovered), nil
}
