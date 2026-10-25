package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// CLI runs the real `lensdock` binary against the runtime API.
type CLI struct {
	Bin       string
	API       string
	TokenFile string
	Workspace string
}

// NewCLI builds a CLI from the LENSDOCK_E2E_* environment.
func NewCLI(t TB, api *API) CLI {
	t.Helper()
	bin := Require(t, EnvLensdockBin)
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("lensdock binary %s: %v", bin, err)
	}
	return CLI{Bin: bin, API: api.Base, TokenFile: Require(t, EnvOperatorToken), Workspace: api.Workspace}
}

// Result is a finished CLI invocation.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Run executes the CLI and waits for it. A non-zero exit status is reported in
// Result.ExitCode, not as an error; err is only set when the process could not
// be run to completion.
func (c CLI) Run(ctx context.Context, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	res := Result{Stdout: out.Bytes(), Stderr: errb.Bytes()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
	default:
		return res, fmt.Errorf("run lensdock: %w", err)
	}
	return res, nil
}

// AcquireToFile runs `lensdock lease acquire` writing the lease file.
func (c CLI) AcquireToFile(ctx context.Context, mode string, ttlSeconds int, leaseFile string) error {
	res, err := c.Run(ctx, "lease", "acquire", c.Workspace,
		"--mode", mode, "--ttl", strconv.Itoa(ttlSeconds),
		"--api", c.API, "--operator-token-file", c.TokenFile, "--output", leaseFile)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("lensdock lease acquire exited %d: %s", res.ExitCode, bytes.TrimSpace(res.Stderr))
	}
	return nil
}

// ReleaseFile runs `lensdock lease release --lease-file`.
func (c CLI) ReleaseFile(ctx context.Context, leaseFile string) (Result, error) {
	return c.Run(ctx, "lease", "release", "--lease-file", leaseFile, "--api", c.API)
}

// BridgeEndpoints is the JSON line `lensdock cdp bridge` prints on stdout.
type BridgeEndpoints struct {
	DiscoveryURL        string `json:"discoveryUrl"`
	BrowserWebSocketURL string `json:"browserWebSocketUrl"`
	LeaseID             string `json:"leaseId"`
}

// BridgeProc is a running `lensdock cdp bridge`.
type BridgeProc struct {
	Endpoints BridgeEndpoints

	cmd    *exec.Cmd
	done   chan struct{}
	exit   int
	waitEr error
	stderr *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// StartBridge starts `lensdock cdp bridge` on a loopback ephemeral port and
// waits (up to startTimeout) for its endpoint line. The process lives until
// ctx is cancelled or Kill is called.
// With pathSecret the bridge runs with --path-secret (random URL prefix).
func (c CLI) StartBridge(ctx context.Context, leaseFile string, startTimeout time.Duration, pathSecret bool) (*BridgeProc, error) {
	// An os.Pipe (not StdoutPipe) lets the reader goroutine and cmd.Wait run
	// concurrently: os/exec only forbids Wait before reads finish for its own pipes.
	stdout, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("bridge stdout: %w", err)
	}
	args := []string{"cdp", "bridge", "--lease-file", leaseFile, "--listen", "127.0.0.1:0"}
	if pathSecret {
		args = append(args, "--path-secret")
	}
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Stdout = stdoutW
	b := &BridgeProc{cmd: cmd, done: make(chan struct{}), stderr: &syncBuffer{}}
	cmd.Stderr = b.stderr
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stdoutW.Close()
		return nil, fmt.Errorf("start bridge: %w", err)
	}
	_ = stdoutW.Close() // the child holds the write end; EOF arrives when it exits
	lines := make(chan string, 1)
	go func() {
		defer stdout.Close()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		if sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
		for sc.Scan() { // keep draining so the child never blocks on a full pipe
		}
	}()
	go func() {
		err := cmd.Wait()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			b.exit, err = ee.ExitCode(), nil
		}
		b.waitEr = err
		close(b.done)
	}()

	select {
	case line, ok := <-lines:
		if !ok {
			<-b.done
			return nil, fmt.Errorf("bridge exited %d before printing its endpoints: %s", b.exit, b.stderr.String())
		}
		if err := json.Unmarshal([]byte(line), &b.Endpoints); err != nil {
			b.Kill()
			return nil, fmt.Errorf("bridge printed a non-JSON first line: %w", err)
		}
		if b.Endpoints.DiscoveryURL == "" || b.Endpoints.BrowserWebSocketURL == "" {
			b.Kill()
			return nil, errors.New("bridge endpoint line lacks discoveryUrl or browserWebSocketUrl")
		}
		return b, nil
	case <-time.After(startTimeout):
		b.Kill()
		return nil, fmt.Errorf("bridge printed no endpoints within %v", startTimeout)
	}
}

// WaitExit waits for the bridge to exit and returns its exit code.
func (b *BridgeProc) WaitExit(timeout time.Duration) (int, error) {
	select {
	case <-b.done:
		return b.exit, b.waitEr
	case <-time.After(timeout):
		return -1, fmt.Errorf("bridge still running after %v", timeout)
	}
}

// Kill terminates the bridge and waits for it.
func (b *BridgeProc) Kill() {
	if b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
	}
	<-b.done
}

// Stderr returns what the bridge wrote to stderr so far.
func (b *BridgeProc) Stderr() string { return b.stderr.String() }
