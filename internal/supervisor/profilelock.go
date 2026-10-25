package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
)

// The profile lock keeps two runtimes from opening one Chromium profile
// (RT-05). Chromium's own SingletonLock is not enough across containers: it
// holds "hostname-pid", and a second container with the same hostname and a
// PID that is not alive in its namespace takes the lock over as stale. The
// runtime therefore holds an flock(2) on /data/profile/.lensdock.lock for as
// long as it may run Chromium. The lock lives in a helper process owned by the
// browser user (the profile directory is not writable by the supervisor), is
// released by the kernel whenever that process ends, and is never deleted
// or stolen.

const (
	// ProfileLockHelperName is the hidden subcommand RunProfileLockHelper
	// implements.
	ProfileLockHelperName = "__profile-lock"
	// ExitProfileLocked is the helper's exit status when another runtime holds
	// the lock (EX_TEMPFAIL).
	ExitProfileLocked = 75

	profileLockFile    = ".lensdock.lock"
	profileLockedLine  = "locked"
	profileCleanCmd    = "clean"
	profileCleanedLine = "clean"
	profileLockWait    = 10 * time.Second
	profileCleanWait   = 5 * time.Second
	profileReleaseWait = 2 * time.Second
)

// ErrProfileLocked is returned by RunProfileLockHelper when another runtime
// holds the profile lock.
var ErrProfileLocked = errors.New("supervisor: profile is locked by another runtime")

// singletonFiles are Chromium's own process-singleton artifacts. They are
// removed only while the profile lock is held, so no live browser can own
// them.
var singletonFiles = []string{"SingletonLock", "SingletonCookie", "SingletonSocket"}

type lockOwner struct {
	PID       int       `json:"pid"`
	Hostname  string    `json:"hostname"`
	StartedAt time.Time `json:"startedAt"`
}

// RunProfileLockHelper implements the hidden `__profile-lock` subcommand. It
// must run as the owner of dir (empty selects /data/profile). It takes the
// exclusive profile lock without blocking (ErrProfileLocked when taken),
// removes stale Chromium singleton files, prints "locked", and then holds the
// lock until stdin reaches EOF. A "clean" line on stdin removes the singleton
// files again (the supervisor sends it before every Chromium launch) and
// prints "clean".
func RunProfileLockHelper(stdin io.Reader, stdout io.Writer, dir string) error {
	if dir == "" {
		dir = profileDir
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open profile: %w", err)
	}
	defer root.Close()
	f, err := root.OpenFile(profileLockFile, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open profile lock: %w", err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return ErrProfileLocked
		}
		return fmt.Errorf("flock profile: %w", err)
	}
	// Informational only; the lock is the flock, not this content.
	host, _ := os.Hostname()
	if body, err := json.Marshal(lockOwner{PID: os.Getpid(), Hostname: host, StartedAt: time.Now().UTC()}); err == nil {
		if err := f.Truncate(0); err == nil {
			_, _ = f.WriteAt(body, 0)
		}
	}
	if err := cleanSingletons(root); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(stdout, profileLockedLine); err != nil {
		return err
	}
	sc := bufio.NewScanner(io.LimitReader(stdin, 1<<20))
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != profileCleanCmd {
			continue
		}
		if err := cleanSingletons(root); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(stdout, profileCleanedLine); err != nil {
			return err
		}
	}
	return nil
}

// cleanSingletons removes the process-singleton files if present.
func cleanSingletons(root *os.Root) error {
	for _, name := range singletonFiles {
		if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove stale %s: %w", name, err)
		}
	}
	return nil
}

// lineSink turns child stdout into lines on a channel. A full channel drops
// lines: the helper prints a handful per run.
type lineSink struct {
	mu  sync.Mutex
	buf []byte
	ch  chan string
}

func (l *lineSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, b := range p {
		if b != '\n' {
			if len(l.buf) < 256 {
				l.buf = append(l.buf, b)
			}
			continue
		}
		select {
		case l.ch <- string(l.buf):
		default:
		}
		l.buf = l.buf[:0]
	}
	return len(p), nil
}

// profileLock is the running helper holding the flock.
type profileLock struct {
	c     *child
	stdin *io.PipeWriter
	lines chan string
}

func (l *profileLock) alive() bool {
	select {
	case <-l.c.done:
		return false
	default:
		return true
	}
}

// waitLine waits for want on the helper's stdout.
func (l *profileLock) waitLine(ctx context.Context, want string, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case line := <-l.lines:
			if line == want {
				return nil
			}
		case <-l.c.done:
			return fmt.Errorf("profile lock helper exited: %w", orExit(l.c.err))
		case <-timer.C:
			return fmt.Errorf("profile lock helper did not answer %q within %s", want, d)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (l *profileLock) release() {
	_ = l.stdin.Close() // EOF makes the helper return and the kernel drop the lock
	select {
	case <-l.c.done:
	case <-time.After(profileReleaseWait):
		_ = l.c.killGroup()
		<-l.c.done
	}
}

func (s *Supervisor) startProfileLock(ctx context.Context) (*profileLock, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate runtime binary: %w", err)
	}
	pr, pw := io.Pipe()
	lines := make(chan string, 8)
	c, err := startChild([]string{self, ProfileLockHelperName}, startOpts{
		Stdin:  pr,
		Stdout: &lineSink{ch: lines},
		Stderr: newLineLogger(s.log, "profile-lock", "stderr"),
	})
	if err != nil {
		_ = pw.Close()
		return nil, err
	}
	l := &profileLock{c: c, stdin: pw, lines: lines}
	if err := l.waitLine(ctx, profileLockedLine, profileLockWait); err != nil {
		_ = pw.Close()
		_ = c.killGroup()
		<-c.done
		var ee *exec.ExitError
		if errors.As(c.err, &ee) && ee.ExitCode() == ExitProfileLocked {
			return nil, &blockedError{
				code: apiwire.CodeProfileLocked,
				err:  errors.New("the profile is locked by another runtime; refusing to open it (no lock was removed)"),
			}
		}
		return nil, err
	}
	return l, nil
}

// acquireProfileLock returns the profile lock holder, starting the helper if
// none is alive, and has it remove stale Chromium singleton files. Another
// runtime holding the lock is a blockedError (PROFILE_LOCKED): the runtime
// stays not ready and never retries.
func (s *Supervisor) acquireProfileLock(ctx context.Context) (*profileLock, error) {
	s.profileMu.Lock()
	defer s.profileMu.Unlock()
	if l := s.profileLock; l != nil && l.alive() {
		if err := s.cleanProfileLocked(ctx, l); err != nil {
			return nil, err
		}
		return l, nil
	}
	s.profileLock = nil
	l, err := s.startProfileLock(ctx)
	if err != nil {
		return nil, err
	}
	s.profileLock = l
	return l, nil
}

func (s *Supervisor) cleanProfileLocked(ctx context.Context, l *profileLock) error {
	if _, err := io.WriteString(l.stdin, profileCleanCmd+"\n"); err != nil {
		return fmt.Errorf("profile lock helper: %w", err)
	}
	return l.waitLine(ctx, profileCleanedLine, profileCleanWait)
}

// watchProfileLock fails safe: if the lock holder dies while Chromium runs the
// lock is gone, so Chromium is killed and relaunched through the normal
// restart path, which re-takes the lock or blocks on PROFILE_LOCKED.
func (s *Supervisor) watchProfileLock(l *profileLock, chromium *child) {
	select {
	case <-l.c.done:
		select {
		case <-chromium.done:
		default:
			s.log.Error("profile lock helper exited while Chromium runs; stopping Chromium")
			_ = chromium.killGroup()
		}
	case <-chromium.done:
	}
}

// releaseProfileLock ends the helper after Chromium has exited.
func (s *Supervisor) releaseProfileLock() {
	s.profileMu.Lock()
	l := s.profileLock
	s.profileLock = nil
	s.profileMu.Unlock()
	if l != nil {
		l.release()
	}
}

// RunBrowserProcess runs argv (absolute binary path) as the browser user with
// the given stdin and stdout and waits for it. Stderr is discarded. The
// process group is killed when ctx ends. A non-zero exit is returned as the
// process's *exec.ExitError.
func (s *Supervisor) RunBrowserProcess(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := startChild(argv, startOpts{Stdin: stdin, Stdout: stdout})
	if err != nil {
		return err
	}
	select {
	case <-c.done:
	case <-ctx.Done():
		_ = c.killGroup()
		<-c.done
		return ctx.Err()
	}
	return c.err
}
