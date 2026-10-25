package hostmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxResponseBody  = 4 << 20
	defaultCallLimit = 3 * time.Minute
	startupTimeout   = 30 * time.Second
	maxLogBytes      = 4 << 20
)

// CallError is a manager refusal. The response it accompanies, when present,
// still carries identifiers such as an operation ID.
type CallError struct {
	Status  int
	Message string
}

func (e *CallError) Error() string { return e.Message }

// checkSocket verifies that the endpoint is a socket owned by the current
// user inside a private directory, so a planted path is never trusted.
func checkSocket(sock string) error {
	if err := checkPrivateDir(filepath.Dir(sock)); err != nil {
		return fmt.Errorf("manager endpoint: %w", err)
	}
	fi, err := os.Lstat(sock)
	if err != nil {
		return fmt.Errorf("manager is not running: %w", err)
	}
	if fi.Mode()&os.ModeSocket == 0 || !ownedByUs(fi) {
		return errors.New("manager endpoint is not a socket owned by the current user")
	}
	return nil
}

func unixClient(sock string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: defaultCallLimit,
		},
		// A redirect is never followed: the manager does not issue them and a
		// bearer token must not travel anywhere else.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func readToken(tokenFile string) (string, error) {
	data, err := readPrivateFile(tokenFile, 4096)
	if err != nil {
		return "", fmt.Errorf("credential file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("credential file is empty")
	}
	return token, nil
}

// Call sends one request to the manager over its private Unix socket. The
// credential is read from tokenFile and sent only in the Authorization
// header. A refusal returns the partial Response together with the error.
func Call(ctx context.Context, root, tokenFile string, req Request) (Response, error) {
	canonical, err := canonicalRoot(root)
	if err != nil {
		return Response{}, err
	}
	sock := socketPath(canonical)
	if err := checkSocket(sock); err != nil {
		return Response{}, err
	}
	token, err := readToken(tokenFile)
	if err != nil {
		return Response{}, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultCallLimit)
		defer cancel()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://lensdock-manager/v1/request", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpResp, err := unixClient(sock).Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, fmt.Errorf("manager request failed: %w", err)
	}
	defer httpResp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(httpResp.Body, maxResponseBody+1))
	if err != nil {
		return Response{}, fmt.Errorf("read manager response: %w", err)
	}
	if len(data) > maxResponseBody {
		return Response{}, errors.New("manager response is too large")
	}
	var out Response
	if err := json.Unmarshal(data, &out); err != nil {
		return Response{}, fmt.Errorf("manager returned status %d without a valid response", httpResp.StatusCode)
	}
	if httpResp.StatusCode != http.StatusOK || out.Error != "" {
		msg := out.Error
		if msg == "" {
			msg = fmt.Sprintf("manager returned status %d", httpResp.StatusCode)
		}
		return out, &CallError{Status: httpResp.StatusCode, Message: msg}
	}
	return out, nil
}

// identityOK reports whether the socket answers as this manager.
func identityOK(ctx context.Context, cfg Config) bool {
	sock := socketPath(cfg.Root)
	if checkSocket(sock) != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://lensdock-manager/v1/identity", nil)
	if err != nil {
		return false
	}
	resp, err := unixClient(sock).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var id identityResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&id); err != nil {
		return false
	}
	return id.Protocol == protocolVersion && id.ManagerID == cfg.ID && id.Root == cfg.Root
}

// EnsureRunning starts the manager when no manager with this root's identity
// answers. It launches only the configured executable, serializes concurrent
// launchers with a lock, detaches the daemon from the caller's context and
// waits a bounded time for an identity-checked readiness response.
func EnsureRunning(ctx context.Context, root string) error {
	cfg, err := LoadConfig(root)
	if err != nil {
		return err
	}
	if identityOK(ctx, cfg) {
		return nil
	}
	lock, err := waitFlock(ctx, filepath.Join(cfg.Root, launchLockName), startupTimeout)
	if err != nil {
		return fmt.Errorf("manager launch lock: %w", err)
	}
	defer lock.Close()
	if identityOK(ctx, cfg) {
		return nil
	}
	if err := validateExecutable("manager executable", cfg.Executable); err != nil {
		return err
	}
	logPath := filepath.Join(cfg.Root, "logs", logName)
	if fi, err := os.Lstat(logPath); err == nil && fi.Size() > maxLogBytes {
		_ = os.Rename(logPath, logPath+".1")
	}
	cmd, err := spawnDetached(cfg, logPath, "serve", "--root", cfg.Root)
	if err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	timeout := time.NewTimer(startupTimeout)
	defer timeout.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-exited:
			if identityOK(ctx, cfg) {
				return nil
			}
			return fmt.Errorf("manager exited during startup (%v); see %s", err, logPath)
		case <-timeout.C:
			return fmt.Errorf("manager did not become ready within %s; see %s", startupTimeout, logPath)
		case <-tick.C:
			if identityOK(ctx, cfg) {
				return nil
			}
		}
	}
}
