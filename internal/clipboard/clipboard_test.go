package clipboard

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	const max = 64
	tests := []struct {
		name string
		text []byte
		want error
	}{
		{"empty", nil, nil},
		{"ascii", []byte("hello"), nil},
		{"astral code point", []byte("\U0001F600 \U00020BB7"), nil},
		{"ZWJ family sequence", []byte("\U0001F468\u200D\U0001F469\u200D\U0001F467"), nil},
		{"NFD kept", []byte("e\u0301"), nil},
		{"exactly max", []byte(strings.Repeat("a", max)), nil},
		{"one over max", []byte(strings.Repeat("a", max+1)), ErrTooLarge},
		{"invalid continuation byte", []byte{'a', 0xff, 'b'}, ErrInvalidUTF8},
		{"truncated sequence", []byte{0xe4, 0xb8}, ErrInvalidUTF8},
		{"surrogate encoded", []byte{0xed, 0xa0, 0x80}, ErrInvalidUTF8},
		{"overlong NUL", []byte{0xc0, 0x80}, ErrInvalidUTF8},
		{"NUL byte", []byte("a\x00b"), ErrNUL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Validate(tt.text, max); !errors.Is(got, tt.want) {
				t.Fatalf("Validate = %v, want %v", got, tt.want)
			}
		})
	}
}

type fakeProc struct {
	killed   chan struct{}
	killOnce sync.Once
	exitNow  bool  // Wait returns waitErr immediately, like xclip failing at startup
	waitErr  error // only used with exitNow
}

func (p *fakeProc) Wait() error {
	if p.exitNow {
		return p.waitErr
	}
	<-p.killed
	return nil
}
func (p *fakeProc) Kill() error { p.killOnce.Do(func() { close(p.killed) }); return nil }

func (p *fakeProc) isKilled() bool {
	select {
	case <-p.killed:
		return true
	default:
		return false
	}
}

type spawnCall struct {
	argv  []string
	stdin []byte
	proc  *fakeProc
}

type fakeSpawner struct {
	mu      sync.Mutex
	calls   []spawnCall
	err     error
	exitNow bool
	waitErr error

	// Readback model: the newest owner serves its stdin once it is alive
	// and pendingReads readbacks have happened since it started. Until then
	// a readback returns staleValue (nil error) when set, else an error like
	// xclip's "target not available".
	pendingReads int
	neverOwned   bool
	staleValue   []byte
	reads        int
	readArgv     []string
	sinceOwner   int
}

func (s *fakeSpawner) StartBrowserProcess(_ context.Context, argv []string, stdin io.Reader) (Process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		return nil, err
	}
	p := &fakeProc{killed: make(chan struct{}), exitNow: s.exitNow, waitErr: s.waitErr}
	s.calls = append(s.calls, spawnCall{argv: append([]string(nil), argv...), stdin: data, proc: p})
	s.sinceOwner = 0
	return p, nil
}

var errTargetNotAvailable = errors.New("exit status 1")

func (s *fakeSpawner) OutputBrowserProcess(ctx context.Context, argv []string, maxBytes int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	s.readArgv = append([]string(nil), argv...)
	s.sinceOwner++
	if n := len(s.calls); n > 0 && !s.neverOwned && s.sinceOwner > s.pendingReads {
		c := s.calls[n-1]
		if !c.proc.exitNow && !c.proc.isKilled() {
			return append([]byte(nil), c.stdin...), nil
		}
	}
	if s.staleValue != nil {
		return append([]byte(nil), s.staleValue...), nil
	}
	return nil, errTargetNotAvailable
}

func (s *fakeSpawner) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func (s *fakeSpawner) call(t *testing.T, i int) spawnCall {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.calls) {
		t.Fatalf("spawn call %d missing, have %d", i, len(s.calls))
	}
	return s.calls[i]
}

func (s *fakeSpawner) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

type holder struct{ done chan struct{} }

func newHolder() *holder                { return &holder{done: make(chan struct{})} }
func (h *holder) Done() <-chan struct{} { return h.done }

func waitKilled(t *testing.T, p *fakeProc) {
	t.Helper()
	select {
	case <-p.killed:
	case <-time.After(2 * time.Second):
		t.Fatal("owner was not killed")
	}
}

func TestSetPassesBytesVerbatimOnStdinOnly(t *testing.T) {
	sp := &fakeSpawner{}
	b := newTestBridge(sp, time.Minute, 1024)
	text := []byte("cafe\u0301 \U0001F468\u200D\U0001F469 \u4E2D\u6587")
	if err := b.Set(context.Background(), text, newHolder()); err != nil {
		t.Fatal(err)
	}
	c := sp.call(t, 0)
	if !bytes.Equal(c.stdin, text) {
		t.Fatalf("stdin = %q, want %q", c.stdin, text)
	}
	for _, a := range c.argv {
		if strings.Contains(a, "cafe") {
			t.Fatalf("clipboard text leaked into argv: %v", c.argv)
		}
	}
	want := []string{"-selection", "clipboard", "-target", "UTF8_STRING", "-in", "-quiet"}
	if got := c.argv[1:]; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("xclip args = %v, want %v", got, want)
	}
	clearOwner(b)
}

func TestSetRejectsInvalidTextWithoutSpawning(t *testing.T) {
	sp := &fakeSpawner{}
	b := New(sp, time.Minute, 8)
	for _, text := range [][]byte{{0xff}, []byte("a\x00"), []byte("123456789")} {
		if err := b.Set(context.Background(), text, newHolder()); err == nil {
			t.Fatalf("Set(%q) succeeded", text)
		}
	}
	if sp.count() != 0 {
		t.Fatalf("spawned %d processes for invalid text", sp.count())
	}
}

func TestNoSizeLimitUnlessConfigured(t *testing.T) {
	// 200 KiB: above the former 64 KiB cap.
	text := []byte(strings.Repeat("a", 200<<10))
	if err := Validate(text, 0); err != nil {
		t.Fatalf("Validate without a limit = %v", err)
	}
	if err := Validate(text, len(text)-1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Validate one byte over a configured limit = %v, want ErrTooLarge", err)
	}
	if err := Validate([]byte{'a', 0xff}, 0); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("no limit must still require valid UTF-8, got %v", err)
	}

	sp := &fakeSpawner{}
	if err := newTestBridge(sp, time.Minute, 0).Set(context.Background(), text, newHolder()); err != nil {
		t.Fatalf("Set above the former cap without a configured limit = %v", err)
	}
}

func TestSetRequiresHolder(t *testing.T) {
	b := New(&fakeSpawner{}, time.Minute, 8)
	if err := b.Set(context.Background(), []byte("x"), nil); !errors.Is(err, ErrNoHolder) {
		t.Fatalf("Set without holder = %v, want ErrNoHolder", err)
	}
}

func TestSetReplacesPreviousOwner(t *testing.T) {
	sp := &fakeSpawner{}
	b := newTestBridge(sp, time.Minute, 1024)
	if err := b.Set(context.Background(), []byte("one"), newHolder()); err != nil {
		t.Fatal(err)
	}
	first := sp.call(t, 0).proc
	if err := b.Set(context.Background(), []byte("two"), newHolder()); err != nil {
		t.Fatal(err)
	}
	waitKilled(t, first)
	second := sp.call(t, 1).proc
	if second.isKilled() {
		t.Fatal("replacement owner was killed")
	}
	clearOwner(b)
	waitKilled(t, second)
}

func TestSpawnFailureLeavesNoOwner(t *testing.T) {
	sp := &fakeSpawner{}
	b := newTestBridge(sp, time.Minute, 1024)
	if err := b.Set(context.Background(), []byte("one"), newHolder()); err != nil {
		t.Fatal(err)
	}
	first := sp.call(t, 0).proc
	sp.mu.Lock()
	sp.err = errors.New("spawn failed")
	sp.mu.Unlock()
	if err := b.Set(context.Background(), []byte("two"), newHolder()); err == nil {
		t.Fatal("Set succeeded despite spawn failure")
	}
	waitKilled(t, first)
}

func TestClearedWhenTTLElapses(t *testing.T) {
	sp := &fakeSpawner{}
	b := newTestBridge(sp, 7*time.Second, 1024)
	fire := make(chan time.Time)
	gotTTL := make(chan time.Duration, 1)
	b.timer = func(d time.Duration) (<-chan time.Time, func() bool) {
		gotTTL <- d
		return fire, func() bool { return true }
	}
	if err := b.Set(context.Background(), []byte("x"), newHolder()); err != nil {
		t.Fatal(err)
	}
	p := sp.call(t, 0).proc
	if got := <-gotTTL; got != 7*time.Second {
		t.Fatalf("timer duration = %v, want 7s", got)
	}
	if p.isKilled() {
		t.Fatal("owner killed before TTL elapsed")
	}
	fire <- time.Time{}
	waitKilled(t, p)
}

func TestStaleWatcherDoesNotKillReplacement(t *testing.T) {
	sp := &fakeSpawner{}
	b := newTestBridge(sp, time.Hour, 1024)
	h1 := newHolder()
	if err := b.Set(context.Background(), []byte("one"), h1); err != nil {
		t.Fatal(err)
	}
	first := sp.call(t, 0).proc
	h2 := newHolder()
	if err := b.Set(context.Background(), []byte("two"), h2); err != nil {
		t.Fatal(err)
	}
	waitKilled(t, first)
	second := sp.call(t, 1).proc

	close(h1.done) // the first lease ending must not clear the second value
	time.Sleep(50 * time.Millisecond)
	if second.isKilled() {
		t.Fatal("ending the replaced holder cleared the new value")
	}
	close(h2.done)
	waitKilled(t, second)
}

// newTestBridge keeps the real confirm timeout but polls fast; fake owners
// are readable at once unless a test says otherwise.
func newTestBridge(sp Spawner, ttl time.Duration, maxBytes int) *Bridge {
	b := New(sp, ttl, maxBytes)
	b.poll = time.Millisecond
	return b
}

func clearOwner(b *Bridge) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.killLocked()
}

func TestSetReportsXclipThatExitsBeforeConfirming(t *testing.T) {
	for _, tt := range []struct {
		name    string
		waitErr error
	}{
		{"non-zero exit", errors.New("exit status 1")},
		{"clean exit", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sp := &fakeSpawner{exitNow: true, waitErr: tt.waitErr}
			b := newTestBridge(sp, time.Minute, 1024)
			b.confirm = 5 * time.Second // returns as soon as the owner exits
			start := time.Now()
			err := b.Set(context.Background(), []byte("secret text"), newHolder())
			if !errors.Is(err, ErrSelectionNotHeld) {
				t.Fatalf("Set = %v, want ErrSelectionNotHeld", err)
			}
			if strings.Contains(err.Error(), "secret text") {
				t.Fatalf("error leaks clipboard text: %v", err)
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("Set waited for the full confirm window after the owner exited")
			}
		})
	}
}

func TestSetKeepsLongLivedXclipRunning(t *testing.T) {
	sp := &fakeSpawner{}
	b := newTestBridge(sp, time.Minute, 1024)
	if err := b.Set(context.Background(), []byte("x"), newHolder()); err != nil {
		t.Fatalf("Set with a live owner = %v", err)
	}
	if sp.call(t, 0).proc.isKilled() {
		t.Fatal("a confirmed owner must keep the selection")
	}
	clearOwner(b)
}

func TestSetCanceledWhileConfirmingKillsOwner(t *testing.T) {
	sp := &fakeSpawner{neverOwned: true}
	b := newTestBridge(sp, time.Minute, 1024)
	b.confirm = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- b.Set(ctx, []byte("x"), newHolder()) }()
	for sp.count() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Set = %v, want context.Canceled", err)
	}
	waitKilled(t, sp.call(t, 0).proc)
}

func TestSetWaitsUntilTheSelectionReadsBack(t *testing.T) {
	sp := &fakeSpawner{pendingReads: 3}
	b := newTestBridge(sp, time.Minute, 1024)
	text := []byte("\u4E2D\u6587 \U0001F600")
	if err := b.Set(context.Background(), text, newHolder()); err != nil {
		t.Fatalf("Set with delayed ownership = %v", err)
	}
	if got := sp.readCount(); got != 4 {
		t.Fatalf("readbacks = %d, want 4 (3 before ownership, 1 match)", got)
	}
	sp.mu.Lock()
	readArgv := sp.readArgv
	sp.mu.Unlock()
	want := []string{"/usr/bin/xclip", "-selection", "clipboard", "-target", "UTF8_STRING", "-out"}
	if strings.Join(readArgv, " ") != strings.Join(want, " ") {
		t.Fatalf("readback argv = %v, want %v", readArgv, want)
	}
	if sp.call(t, 0).proc.isKilled() {
		t.Fatal("a confirmed owner must keep the selection")
	}
	clearOwner(b)
}

func TestSetFailsWhenTheSelectionNeverReadsBack(t *testing.T) {
	for _, tt := range []struct {
		name  string
		stale []byte
	}{
		{"no owner", nil},
		{"stale value from another owner", []byte("old value")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sp := &fakeSpawner{neverOwned: true, staleValue: tt.stale}
			b := newTestBridge(sp, time.Minute, 1024)
			b.confirm = 50 * time.Millisecond
			err := b.Set(context.Background(), []byte("secret text"), newHolder())
			if !errors.Is(err, ErrSelectionNotHeld) {
				t.Fatalf("Set = %v, want ErrSelectionNotHeld", err)
			}
			if strings.Contains(err.Error(), "secret text") || strings.Contains(err.Error(), "old value") {
				t.Fatalf("error leaks clipboard text: %v", err)
			}
			waitKilled(t, sp.call(t, 0).proc)
			b.mu.Lock()
			cur := b.cur
			b.mu.Unlock()
			if cur != nil {
				t.Fatal("an unconfirmed owner was kept as the current value")
			}
		})
	}
}
