package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	helperTimeout    = 5 * time.Second
	childWaitDelay   = 3 * time.Second
	maxLogLineBytes  = 2048
	signalHelperName = "__signal"
)

// child is a started process running as the browser user in its own process
// group. The supervisor holds a pidfd so it can signal exactly this process
// (via the signal helper, since supervisor and browser UIDs differ and the
// supervisor has no CAP_KILL) without racing PID reuse.
type child struct {
	pid   int
	cmd   *exec.Cmd
	pidfd *os.File

	mu     sync.Mutex
	reaped bool

	done chan struct{} // closed once the process has been reaped
	err  error         // Wait result; valid after done is closed

	// sigHook replaces OS signal delivery when non-nil (tests); group is
	// true for process-group delivery.
	sigHook func(sig syscall.Signal, group bool) error
}

type startOpts struct {
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	ExtraFiles []*os.File
}

// startChild starts argv as the browser user with the child environment
// allowlist, in a new process group, killed if the supervisor dies.
func startChild(argv []string, o startOpts) (*child, error) {
	if len(argv) == 0 || !filepath.IsAbs(argv[0]) {
		return nil, errors.New("child argv[0] must be an absolute path")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = childEnv()
	cmd.Dir = "/"
	cmd.Stdin = o.Stdin
	cmd.Stdout = o.Stdout
	cmd.Stderr = o.Stderr
	cmd.ExtraFiles = o.ExtraFiles
	cmd.WaitDelay = childWaitDelay

	pidfd, err := startProcess(cmd)
	if err != nil {
		if cmd.Process != nil {
			// Started but unusable without a pidfd: kill its group, then reap.
			_ = signalGroup(cmd.Process.Pid, syscall.SIGKILL)
			go func() { _ = cmd.Wait() }()
		}
		return nil, fmt.Errorf("start %s: %w", filepath.Base(argv[0]), err)
	}
	c := &child{pid: cmd.Process.Pid, cmd: cmd, pidfd: pidfd, done: make(chan struct{})}
	go c.wait()
	return c, nil
}

func (c *child) wait() {
	err := c.cmd.Wait()
	c.mu.Lock()
	c.reaped = true
	_ = c.pidfd.Close()
	c.mu.Unlock()
	c.err = err
	close(c.done)
}

// signal sends sig to the child through its pidfd. It is a no-op after the
// child has been reaped.
func (c *child) signal(sig syscall.Signal) error {
	if c.sigHook != nil {
		return c.sigHook(sig, false)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reaped {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), helperTimeout)
	defer cancel()
	return runSignalHelper(ctx, c.pidfd, sig, 0)
}

// killGroup SIGKILLs the child's whole process group.
func (c *child) killGroup() error {
	if c.sigHook != nil {
		return c.sigHook(syscall.SIGKILL, true)
	}
	return signalGroup(c.pid, syscall.SIGKILL)
}

// signalGroup sends sig to every process in group pgid (the group leader's
// PID). ESRCH (empty group) is reported as success by the helper.
func signalGroup(pgid int, sig syscall.Signal) error {
	ctx, cancel := context.WithTimeout(context.Background(), helperTimeout)
	defer cancel()
	return runSignalHelper(ctx, nil, sig, pgid)
}

// parseSignalArgs parses the arguments of the `__signal` helper:
// `<signal>` signals the process behind fd 3 (a pidfd); `<signal> <pgid>`
// signals a whole process group. Only SIGTERM and SIGKILL are accepted.
func parseSignalArgs(args []string) (syscall.Signal, int, error) {
	if len(args) < 1 || len(args) > 2 {
		return 0, 0, errors.New("usage: __signal <9|15> [pgid]")
	}
	n, err := strconv.Atoi(args[0])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid signal %q", args[0])
	}
	sig := syscall.Signal(n)
	if sig != syscall.SIGTERM && sig != syscall.SIGKILL {
		return 0, 0, fmt.Errorf("signal %d not allowed", n)
	}
	if len(args) == 1 {
		return sig, 0, nil
	}
	pgid, err := strconv.Atoi(args[1])
	if err != nil || pgid <= 1 {
		return 0, 0, fmt.Errorf("invalid process group %q", args[1])
	}
	return sig, pgid, nil
}

// lineLogger turns a child's output stream into capped slog lines.
type lineLogger struct {
	mu   sync.Mutex
	log  *slog.Logger
	buf  []byte
	over bool // current line already exceeded the cap and was truncated
}

func newLineLogger(log *slog.Logger, unit, stream string) *lineLogger {
	return &lineLogger{log: log.With("unit", unit, "stream", stream)}
}

func (l *lineLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			l.flush()
			continue
		}
		if len(l.buf) >= maxLogLineBytes {
			l.over = true
			continue
		}
		l.buf = append(l.buf, b)
	}
	return len(p), nil
}

func (l *lineLogger) flush() {
	if len(l.buf) == 0 && !l.over {
		return
	}
	msg := string(l.buf)
	if l.over {
		msg += " [truncated]"
	}
	l.log.Info("child output", "line", msg)
	l.buf = l.buf[:0]
	l.over = false
}
