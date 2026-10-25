// Package clipboard implements the runtime X11 clipboard bridge: it validates
// UTF-8 text and publishes it as the CLIPBOARD selection by running xclip as
// the browser user. The selection is cleared when the lease that set it ends,
// when it is replaced, or when its time-to-live elapses.
//
// Clipboard text is never logged, placed on an argv, or included in an error.
package clipboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// DefaultMaxBytes is the clipboard size limit used when New receives a
	// non-positive limit.
	DefaultMaxBytes = 64 * 1024
	// DefaultTTL is the selection lifetime used when New receives a
	// non-positive TTL.
	DefaultTTL = 60 * time.Second
	// confirmTimeout bounds how long Set waits to read the new value back
	// from the CLIPBOARD selection. Only a byte-exact readback through a
	// separate X client proves that xclip owns the selection and serves it.
	confirmTimeout = 2 * time.Second
	// confirmPoll is the pause between readback attempts.
	confirmPoll = 25 * time.Millisecond
	// readTimeout bounds a single readback process.
	readTimeout = 500 * time.Millisecond
)

// xclipArgv publishes stdin as the CLIPBOARD selection. -quiet keeps xclip in
// the foreground so the returned Process is the selection owner.
var xclipArgv = []string{
	"/usr/bin/xclip", "-selection", "clipboard", "-target", "UTF8_STRING", "-in", "-quiet",
}

// xclipReadArgv prints the current CLIPBOARD selection as UTF-8 on stdout.
var xclipReadArgv = []string{
	"/usr/bin/xclip", "-selection", "clipboard", "-target", "UTF8_STRING", "-out",
}

var (
	// ErrInvalidUTF8 reports text that is not valid UTF-8.
	ErrInvalidUTF8 = errors.New("clipboard: text is not valid UTF-8")
	// ErrNUL reports text containing a NUL byte.
	ErrNUL = errors.New("clipboard: text contains NUL")
	// ErrTooLarge reports text longer than the configured byte limit.
	ErrTooLarge = errors.New("clipboard: text too large")
	// ErrNoHolder reports a Set call without a lease holder.
	ErrNoHolder = errors.New("clipboard: holder is required")
	// ErrSelectionNotHeld reports that the new value could not be read back
	// from the selection: xclip exited, or the readback never matched
	// within the confirm timeout.
	ErrSelectionNotHeld = errors.New("clipboard: xclip did not take the selection")
)

// Process is a spawned selection owner.
type Process interface {
	Wait() error
	Kill() error
}

// Spawner starts processes as the browser user with the child environment
// allowlist.
type Spawner interface {
	// StartBrowserProcess starts a long-lived process; its output is discarded.
	StartBrowserProcess(ctx context.Context, argv []string, stdin io.Reader) (Process, error)
	// OutputBrowserProcess runs argv to completion and returns its stdout.
	// It fails if the process exits non-zero, if ctx ends first, or if stdout
	// exceeds maxBytes. Errors never include the output.
	OutputBrowserProcess(ctx context.Context, argv []string, maxBytes int) ([]byte, error)
}

// Holder is the lease that owns a clipboard value; lease.Ticket satisfies it.
type Holder interface {
	Done() <-chan struct{}
}

// Validate checks text for publication. It never normalizes: NFC/NFD forms,
// astral code points and ZWJ sequences are accepted byte for byte.
func Validate(text []byte, maxBytes int) error {
	if len(text) > maxBytes {
		return ErrTooLarge
	}
	if !utf8.Valid(text) {
		return ErrInvalidUTF8
	}
	if bytes.IndexByte(text, 0) >= 0 {
		return ErrNUL
	}
	return nil
}

// Bridge owns at most one selection-owner process at a time.
type Bridge struct {
	sp          Spawner
	ttl         time.Duration
	maxBytes    int
	confirm     time.Duration
	poll        time.Duration
	readTimeout time.Duration
	timer       func(time.Duration) (<-chan time.Time, func() bool)

	mu  sync.Mutex
	cur *owner
}

type owner struct {
	proc   Process
	stop   chan struct{}   // closed to cancel the watcher
	exited <-chan struct{} // closed once proc has been reaped
}

// New returns a Bridge. A non-positive ttl or maxBytes selects DefaultTTL or
// DefaultMaxBytes.
func New(sp Spawner, ttl time.Duration, maxBytes int) *Bridge {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &Bridge{
		sp: sp, ttl: ttl, maxBytes: maxBytes,
		confirm: confirmTimeout, poll: confirmPoll, readTimeout: readTimeout,
		timer: realTimer,
	}
}

func realTimer(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(d)
	return t.C, t.Stop
}

// Set replaces the clipboard with text. The previous owner is killed first.
// Set returns nil only after a separate xclip reads text back from the
// CLIPBOARD selection byte for byte, so a paste issued after Set returns
// sees the new value. If the owner exits, or the readback does not match
// within the confirm timeout, the owner is killed and ErrSelectionNotHeld is
// returned. After that the selection is cleared when h.Done closes, when the
// TTL elapses, or when the owner process exits on its own.
func (b *Bridge) Set(ctx context.Context, text []byte, h Holder) error {
	if h == nil {
		return ErrNoHolder
	}
	if err := Validate(text, b.maxBytes); err != nil {
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if prev := b.cur; prev != nil {
		b.killLocked()
		// A dying owner can still answer selection requests; wait until it is
		// gone so the readback below cannot be served by the old value.
		select {
		case <-prev.exited:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	proc, err := b.sp.StartBrowserProcess(ctx, xclipArgv, bytes.NewReader(text))
	if err != nil {
		return err
	}
	exited := make(chan struct{})
	var waitErr error // valid once exited is closed
	go func() {
		waitErr = proc.Wait()
		close(exited)
	}()

	if err := b.confirmOwner(ctx, text, exited, &waitErr); err != nil {
		_ = proc.Kill()
		return err
	}

	o := &owner{proc: proc, stop: make(chan struct{}), exited: exited}
	b.cur = o
	go b.watch(o, h.Done())
	return nil
}

// confirmOwner polls the selection until it reads back as text. waitErr is
// read only after exited is closed.
func (b *Bridge) confirmOwner(ctx context.Context, text []byte, exited <-chan struct{}, waitErr *error) error {
	cctx, cancel := context.WithTimeout(ctx, b.confirm)
	defer cancel()
	for {
		select {
		case <-exited:
			if *waitErr != nil {
				return fmt.Errorf("%w: owner exited: %v", ErrSelectionNotHeld, *waitErr)
			}
			return fmt.Errorf("%w: owner exited", ErrSelectionNotHeld)
		default:
		}

		rctx, rcancel := context.WithTimeout(cctx, b.readTimeout)
		got, err := b.sp.OutputBrowserProcess(rctx, xclipReadArgv, b.maxBytes)
		rcancel()
		if err == nil && bytes.Equal(got, text) {
			return nil
		}

		poll := time.NewTimer(b.poll)
		select {
		case <-exited:
			// reported at the top of the loop
		case <-cctx.Done():
			poll.Stop()
			if err := ctx.Err(); err != nil {
				return err
			}
			return fmt.Errorf("%w: readback did not match within %v", ErrSelectionNotHeld, b.confirm)
		case <-poll.C:
		}
		poll.Stop()
	}
}

func (b *Bridge) killLocked() {
	o := b.cur
	if o == nil {
		return
	}
	b.cur = nil
	close(o.stop)
	_ = o.proc.Kill()
}

func (b *Bridge) watch(o *owner, done <-chan struct{}) {
	expire, stopTimer := b.timer(b.ttl)
	defer stopTimer()

	select {
	case <-o.stop:
		return // replaced or cleared; the killer already handled the process
	case <-done:
	case <-expire:
	case <-o.exited:
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cur != o {
		return
	}
	b.killLocked()
}
