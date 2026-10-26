package hostmanager

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// requestShutdown serializes with admission, so no new worker starts after the
// idle check. Existing browsers and persistent data are deliberately untouched.
func (s *server) requestShutdown() (Response, error) {
	s.passMu.Lock()
	defer s.passMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.shutdownCh:
		return Response{}, nil
	default:
	}
	for _, op := range s.reg.Ops {
		if workerPhase(op.Phase) {
			return Response{}, rpcErrf(http.StatusConflict, "operation %s is still active; wait for it to finish before stopping the manager", op.ID)
		}
	}
	helper, busy, err := tryFlock(helperLockPath(s.root))
	if err != nil {
		return Response{}, err
	}
	if busy {
		return Response{}, rpcErrf(http.StatusConflict, "a lifecycle helper is still active; wait for it to finish before stopping the manager")
	}
	defer helper.Close()
	for id := range s.reg.Projects {
		lock, busy, err := tryFlock(projectLockPath(s.root, id))
		if err != nil {
			return Response{}, err
		}
		if busy {
			return Response{}, rpcErrf(http.StatusConflict, "project %s still has an active worker or child process", id)
		}
		_ = lock.Close()
	}
	close(s.shutdownCh)
	return Response{}, nil
}

// Shutdown stops only the manager at root and waits until it releases its
// singleton lock. It never starts a daemon and does not stop project browsers.
func Shutdown(ctx context.Context, root string) error {
	canonical, err := canonicalRoot(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := checkPrivateDir(canonical); err != nil {
		return err
	}
	lockPath := filepath.Join(canonical, serveLockName)
	lock, busy, err := tryFlock(lockPath)
	if err != nil {
		return err
	}
	if !busy {
		return lock.Close()
	}
	if _, err := Call(ctx, canonical, AdminTokenFile(canonical), Request{Action: ActionShutdown}); err != nil {
		return err
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		lock, busy, err := tryFlock(lockPath)
		if err != nil {
			return err
		}
		if !busy {
			return lock.Close()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
