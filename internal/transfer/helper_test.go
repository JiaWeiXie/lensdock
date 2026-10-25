package transfer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type exitErr struct{ code int }

func (e exitErr) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e exitErr) ExitCode() int { return e.code }

// inProcessRunner executes the helper protocol in-process, recording argv.
type inProcessRunner struct {
	dir  string
	argv [][]string
}

func (r *inProcessRunner) RunBrowserProcess(_ context.Context, argv []string, stdin io.Reader, stdout io.Writer) error {
	r.argv = append(r.argv, slices.Clone(argv))
	if len(argv) < 3 || argv[1] != HelperName {
		return errors.New("not a helper invocation")
	}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	if err := RunHelper(argv[2:], stdin, stdout, r.dir); err != nil {
		return exitErr{ExitCode(err)}
	}
	return nil
}

func TestHelperStoreRoundTripThroughTheHelperProtocol(t *testing.T) {
	dir := t.TempDir()
	run := &inProcessRunner{dir: dir}
	s := NewHelperStore(run, "/usr/local/bin/lensdock-runtime", Limits{QuotaBytes: 1000, MaxFileBytes: 500, MaxFiles: 10})

	name := "測試 file; rm -rf $(x).txt"
	e, err := s.Put(ctx, name, strings.NewReader("hello"), 5, false)
	if err != nil || e.Name != name || e.Size != 5 {
		t.Fatalf("Put = %+v, %v", e, err)
	}
	var buf bytes.Buffer
	var gotSize int64
	err = s.Get(ctx, AreaUploads, name, func(size int64, _ time.Time) (io.Writer, error) {
		gotSize = size
		return &buf, nil
	})
	if err != nil || buf.String() != "hello" || gotSize != 5 {
		t.Fatalf("Get = %q size %d, %v", buf.String(), gotSize, err)
	}
	l, err := s.List(ctx, AreaUploads)
	if err != nil || len(l.Files) != 1 || l.Usage.QuotaBytes != 1000 || l.Usage.MaxFiles != 10 {
		t.Fatalf("List = %+v, %v (limits must travel to the helper)", l, err)
	}
	if err := s.Delete(ctx, AreaUploads, name); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "uploads", name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file survived Delete: %v", err)
	}

	// The name is one argv element, never interpreted; the binary is fixed.
	for _, argv := range run.argv {
		if argv[0] != "/usr/local/bin/lensdock-runtime" || argv[1] != HelperName {
			t.Fatalf("argv = %q", argv)
		}
	}
	found := false
	for _, a := range run.argv[0] {
		if a == "--name="+name {
			found = true
		}
	}
	if !found {
		t.Fatalf("the name was not passed as one --name= argument: %q", run.argv[0])
	}
}

func TestHelperStoreMapsExitStatusesBackToErrors(t *testing.T) {
	dir := t.TempDir()
	s := NewHelperStore(&inProcessRunner{dir: dir}, "/bin/rt", Limits{QuotaBytes: 100, MaxFileBytes: 50, MaxFiles: 1})
	if _, err := s.Put(ctx, "a", strings.NewReader("x"), 1, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "a", strings.NewReader("x"), 1, false); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate = %v", err)
	}
	if _, err := s.Put(ctx, "b", strings.NewReader("x"), 1, false); !errors.Is(err, ErrTooMany) {
		t.Fatalf("count = %v", err)
	}
	if _, err := s.Put(ctx, "c", strings.NewReader(strings.Repeat("x", 60)), 60, false); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("per-file = %v", err)
	}
	if _, err := s.Put(ctx, "a", strings.NewReader("xy"), 5, true); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("size mismatch = %v", err)
	}
	if err := s.Get(ctx, AreaUploads, "nope", func(int64, time.Time) (io.Writer, error) {
		t.Fatal("begin must not run for a missing file")
		return nil, nil
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v", err)
	}
	for _, name := range []string{"../x", "a/b", ".dot"} {
		if _, err := s.Put(ctx, name, strings.NewReader("x"), 1, false); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Put(%q) = %v, want ErrInvalidName (rejected before any process starts)", name, err)
		}
	}
	if _, err := s.List(ctx, Area("x")); !errors.Is(err, ErrInvalidArea) {
		t.Fatalf("List(bad area) = %v", err)
	}
}

func TestMapExit(t *testing.T) {
	for _, tt := range []struct {
		code int
		want error
	}{
		{exitNotFound, ErrNotFound}, {exitExists, ErrExists}, {exitInvalid, ErrInvalidName},
		{exitQuota, ErrQuota}, {exitTooLarge, ErrTooLarge}, {exitTooMany, ErrTooMany}, {exitSize, ErrSizeMismatch},
	} {
		if got := mapExit(exitErr{tt.code}); !errors.Is(got, tt.want) {
			t.Errorf("mapExit(%d) = %v, want %v", tt.code, got, tt.want)
		}
	}
	if got := mapExit(exitErr{1}); got == nil || errors.Is(got, ErrNotFound) {
		t.Errorf("mapExit(1) = %v, want a generic failure", got)
	}
	if got := mapExit(context.Canceled); !errors.Is(got, context.Canceled) {
		t.Errorf("non-exit errors must pass through, got %v", got)
	}
	if mapExit(nil) != nil {
		t.Error("mapExit(nil) must be nil")
	}
	for _, e := range exitCodes {
		if ExitCode(fmt.Errorf("wrapped: %w", e.err)) != e.code {
			t.Errorf("ExitCode(%v) != %d", e.err, e.code)
		}
	}
	if ExitCode(nil) != 0 || ExitCode(errors.New("boom")) != exitOther || ExitCode(usageError{"x"}) != exitUsage {
		t.Error("ExitCode defaults are wrong")
	}
}

func TestRunHelperRejectsBadUsage(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) error {
		return RunHelper(args, strings.NewReader(""), io.Discard, dir)
	}
	for name, err := range map[string]error{
		"no op":          run(),
		"unknown op":     run("format"),
		"unknown flag":   run("list", "--area=downloads", "--rm"),
		"stray argument": run("list", "--area=downloads", "extra"),
		"bad number":     run("list", "--area=downloads", "--quota=abc"),
	} {
		if err == nil || ExitCode(err) != exitUsage {
			t.Errorf("%s: err = %v (exit %d), want a usage error", name, err, ExitCode(err))
		}
	}
	if err := run("put", "--area=downloads", "--name=x", "--size=1"); !errors.Is(err, ErrInvalidArea) {
		t.Errorf("put into downloads = %v, want ErrInvalidArea", err)
	}
	if err := run("list", "--area=etc"); !errors.Is(err, ErrInvalidArea) {
		t.Errorf("list of an unknown area = %v", err)
	}
}

func TestGetSinkParsesTheHeaderAcrossArbitraryWriteBoundaries(t *testing.T) {
	var buf bytes.Buffer
	var gotSize int64
	calls := 0
	g := &getSink{begin: func(size int64, mt time.Time) (io.Writer, error) {
		calls++
		gotSize = size
		if mt.UnixNano() != 1234 {
			t.Errorf("mtime = %d", mt.UnixNano())
		}
		return &buf, nil
	}}
	payload := "LDF1 5 1234\nhello"
	for i := range len(payload) {
		if n, err := g.Write([]byte{payload[i]}); n != 1 || err != nil {
			t.Fatalf("byte %d: %d, %v", i, n, err)
		}
	}
	if calls != 1 || gotSize != 5 || buf.String() != "hello" || g.written != 5 {
		t.Fatalf("calls=%d size=%d body=%q written=%d", calls, gotSize, buf.String(), g.written)
	}
}

func TestGetSinkRejectsMalformedHeaders(t *testing.T) {
	for name, in := range map[string]string{
		"wrong tag":     "XXXX 5 1\nhello",
		"missing field": "LDF1 5\nhello",
		"negative size": "LDF1 -5 1\nhello",
		"not a number":  "LDF1 five 1\nhello",
		"no newline":    strings.Repeat("a", maxGetHeader+1),
	} {
		g := &getSink{begin: func(int64, time.Time) (io.Writer, error) {
			t.Errorf("%s: begin ran for a malformed header", name)
			return io.Discard, nil
		}}
		if _, err := g.Write([]byte(in)); err == nil {
			t.Errorf("%s: header accepted", name)
		}
	}
}

// truncatingRunner reports a size it does not deliver.
type truncatingRunner struct{}

func (truncatingRunner) RunBrowserProcess(_ context.Context, _ []string, _ io.Reader, stdout io.Writer) error {
	_, err := io.WriteString(stdout, "LDF1 10 1\nabc")
	return err
}

func TestHelperStoreGetDetectsShortDeliveryAndHelperFailureMidStream(t *testing.T) {
	s := NewHelperStore(truncatingRunner{}, "/bin/rt", Limits{})
	var buf bytes.Buffer
	err := s.Get(ctx, AreaDownloads, "f", func(int64, time.Time) (io.Writer, error) { return &buf, nil })
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("short delivery = %v, want an error", err)
	}

	failing := runnerFunc(func(_ context.Context, _ []string, _ io.Reader, stdout io.Writer) error {
		_, _ = io.WriteString(stdout, "LDF1 10 1\nabc")
		return exitErr{exitNotFound}
	})
	err = NewHelperStore(failing, "/bin/rt", Limits{}).Get(ctx, AreaDownloads, "f", func(int64, time.Time) (io.Writer, error) { return &buf, nil })
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("mid-stream failure = %v, want an interrupted-download error, not a clean 404", err)
	}

	noOutput := runnerFunc(func(context.Context, []string, io.Reader, io.Writer) error { return nil })
	if err := NewHelperStore(noOutput, "/bin/rt", Limits{}).Get(ctx, AreaDownloads, "f", func(int64, time.Time) (io.Writer, error) { return &buf, nil }); err == nil {
		t.Fatal("a helper that printed nothing must not look like success")
	}
}

type runnerFunc func(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer) error

func (f runnerFunc) RunBrowserProcess(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer) error {
	return f(ctx, argv, stdin, stdout)
}

func TestHelperStoreCapsHelperJSONOutput(t *testing.T) {
	flood := runnerFunc(func(_ context.Context, _ []string, _ io.Reader, stdout io.Writer) error {
		chunk := bytes.Repeat([]byte("x"), 1<<20)
		for range 8 {
			if _, err := stdout.Write(chunk); err != nil {
				return err
			}
		}
		return nil
	})
	if _, err := NewHelperStore(flood, "/bin/rt", Limits{}).List(ctx, AreaDownloads); err == nil {
		t.Fatal("an oversized helper listing was accepted")
	}
}
