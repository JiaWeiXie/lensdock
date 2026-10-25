package update

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ProjectStopOptions makes loss of in-memory browser state explicit. Force is
// reserved for an authenticated human and still requires the observed generation.
type ProjectStopOptions struct {
	DiscardPageState   bool
	Force              bool
	ExpectedGeneration string
}

// CheckProjectStart refuses to restart services beneath an unfinished update.
// The host worker holds its project lock through this check and the subsequent
// start. The updater lock protects journal reads from standalone Engine calls.
func (e *Engine) CheckProjectStart() error {
	if err := e.Dep.ensureDirs(); err != nil {
		return err
	}
	unlock, err := lockFile(e.Dep.lockPath())
	if err != nil {
		return err
	}
	defer unlock()
	return e.checkProjectLifecycleJournals()
}

func (e *Engine) checkProjectLifecycleJournals() error {
	journals, issues := listJournals(e.Dep, e.Now)
	if len(issues) > 0 {
		return fmt.Errorf("cannot safely inspect update journals: %w", errors.Join(issues...))
	}
	for _, j := range journals {
		if j.Status == JournalRunning || j.Status == JournalAwaitingApproval || j.Status == JournalHeld {
			return fmt.Errorf("unfinished update %s must be resolved before starting or stopping the project", j.ID)
		}
	}
	return nil
}

// StopProject fences control before stopping both services, retaining every
// volume and the current image/generation overrides. The supervisor's SIGTERM
// path closes Chromium through Browser.close before its bounded signal fallback.
func (e *Engine) StopProject(ctx context.Context, workspace string, opts ProjectStopOptions) error {
	if !opts.DiscardPageState {
		return errors.New("stopping a project requires explicit consent to discard page state")
	}
	if opts.Force && opts.ExpectedGeneration == "" {
		return errors.New("forced stop requires an expected runtime generation")
	}
	if err := e.Dep.ensureDirs(); err != nil {
		return err
	}
	unlock, err := lockFile(e.Dep.lockPath())
	if err != nil {
		return err
	}
	defer unlock()
	if err := e.checkProjectLifecycleJournals(); err != nil {
		return err
	}
	ws, err := e.Dep.Workspace(workspace)
	if err != nil {
		return err
	}
	st, tracked, err := LoadState(e.Dep)
	if err != nil {
		return err
	}
	cc := e.Dep.composeFor(e.Docker, st, tracked)
	ids, err := cc.ContainerIDs(ctx, ws.RuntimeService)
	if err != nil {
		return err
	}
	var running *ContainerInfo
	for _, id := range ids {
		ci, err := e.Docker.ContainerInspect(ctx, id)
		if err != nil {
			return err
		}
		if !ci.State.Running {
			continue
		}
		if running != nil {
			return errors.New("multiple runtime containers prevent a safe stop")
		}
		running = &ci
	}
	if running != nil {
		if !tracked {
			st.Components = map[string]ComponentState{runtimeComponent(workspace): {Service: ws.RuntimeService, Image: running.Image}}
		}
		status, err := e.runtimeProbe(ctx, workspace, st, tracked, ProbeRequest{Op: ProbeStatus})
		if err != nil {
			return fmt.Errorf("inspect runtime before stop: %w", err)
		}
		if err := probeErr(status); err != nil {
			return err
		}
		if status.Status == nil || status.Status.RuntimeGeneration == "" {
			return errors.New("runtime did not report its generation; refusing stop")
		}
		generation := status.Status.RuntimeGeneration
		if opts.ExpectedGeneration != "" && opts.ExpectedGeneration != generation {
			return errors.New("runtime generation changed; refusing stop")
		}
		if opts.Force && status.Status.LeaseHeld() {
			// Lease IDs are generation-unique. A restart or a replacement lease
			// cannot turn this explicit revocation into revoking another holder.
			resp, err := e.runtimeProbe(ctx, workspace, st, tracked, ProbeRequest{Op: ProbeRevokeLease, LeaseID: status.Status.LeaseID})
			if err != nil {
				return err
			}
			if err := probeErr(resp); err != nil {
				return err
			}
		}
		fenced, err := e.runtimeProbe(ctx, workspace, st, tracked, ProbeRequest{
			Op: ProbeMaintenanceEnter, Reason: "explicit project stop", IfIdle: true, ExpectedGeneration: generation,
		})
		if err != nil {
			return fmt.Errorf("fence runtime before stop: %w", err)
		}
		if err := probeErr(fenced); err != nil {
			return fmt.Errorf("stop refused without stopping services: %w", err)
		}
		if fenced.Maintenance == nil || fenced.Maintenance.RuntimeGeneration != generation || !fenced.Maintenance.Drained {
			return errors.New("runtime did not confirm the conditional control fence; refusing stop")
		}
	}
	services := []string{ws.RuntimeService}
	if e.Dep.Gateway != nil {
		services = append(services, e.Dep.Gateway.Service)
	}
	if err := cc.Stop(ctx, time.Duration(e.Dep.StopGraceSeconds)*time.Second, services...); err != nil {
		return fmt.Errorf("project stop did not complete; runtime may remain fenced: %w", err)
	}
	for _, service := range services {
		ids, err := cc.ContainerIDs(ctx, service)
		if err != nil {
			return err
		}
		for _, id := range ids {
			ci, err := e.Docker.ContainerInspect(ctx, id)
			if err != nil {
				return err
			}
			if ci.State.Running {
				return fmt.Errorf("service %s is still running after stop", service)
			}
		}
	}
	return nil
}
