package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// HelperName is the hidden runtime subcommand that RunHelper implements.
const HelperName = "__files"

// Exit codes of the helper process; HelperStore maps them back to errors.
const (
	exitOther     = 1
	exitUsage     = 2
	exitNotFound  = 3
	exitExists    = 4
	exitInvalid   = 5
	exitQuota     = 6
	exitTooLarge  = 7
	exitTooMany   = 8
	exitSize      = 9
	getHeaderTag  = "LDF1"
	maxGetHeader  = 128
	maxHelperJSON = 4 << 20
)

var exitCodes = []struct {
	err  error
	code int
}{
	{ErrNotFound, exitNotFound},
	{ErrExists, exitExists},
	{ErrInvalidName, exitInvalid},
	{ErrInvalidArea, exitInvalid},
	{ErrQuota, exitQuota},
	{ErrTooLarge, exitTooLarge},
	{ErrTooMany, exitTooMany},
	{ErrSizeMismatch, exitSize},
}

// ExitCode maps a store error to the helper's process exit code.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	for _, e := range exitCodes {
		if errors.Is(err, e.err) {
			return e.code
		}
	}
	var u usageError
	if errors.As(err, &u) {
		return exitUsage
	}
	return exitOther
}

type usageError struct{ msg string }

func (e usageError) Error() string { return "transfer: usage: " + e.msg }

// RunHelper implements the hidden `__files` subcommand over dir. It must run
// as the owner of dir. args are the arguments after the subcommand name:
//
//	list|get|put|delete --area=NAME [--name=FILE] [--size=N] [--overwrite]
//	                    [--quota=N] [--max-file=N] [--max-files=N]
//
// list and put print one JSON document on stdout; get prints a one-line header
// ("LDF1 <size> <unix-nanoseconds>") followed by exactly <size> file bytes;
// put reads exactly --size bytes from stdin.
func RunHelper(args []string, stdin io.Reader, stdout io.Writer, dir string) error {
	if len(args) == 0 {
		return usageError{"missing operation"}
	}
	op := args[0]
	fs := flag.NewFlagSet(HelperName, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		area      = fs.String("area", "", "")
		name      = fs.String("name", "", "")
		size      = fs.Int64("size", -1, "")
		overwrite = fs.Bool("overwrite", false, "")
		quota     = fs.Int64("quota", 0, "")
		maxFile   = fs.Int64("max-file", 0, "")
		maxFiles  = fs.Int("max-files", 0, "")
	)
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return usageError{"bad flags"}
	}
	st := NewDirStore(dir, Limits{QuotaBytes: *quota, MaxFileBytes: *maxFile, MaxFiles: *maxFiles})
	ctx := context.Background()
	switch op {
	case "list":
		l, err := st.List(ctx, Area(*area))
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(l)
	case "get":
		return st.Get(ctx, Area(*area), *name, func(n int64, mt time.Time) (io.Writer, error) {
			if _, err := fmt.Fprintf(stdout, "%s %d %d\n", getHeaderTag, n, mt.UnixNano()); err != nil {
				return nil, err
			}
			return stdout, nil
		})
	case "put":
		if Area(*area) != AreaUploads {
			return ErrInvalidArea
		}
		e, err := st.Put(ctx, *name, stdin, *size, *overwrite)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(e)
	case "delete":
		return st.Delete(ctx, Area(*area), *name)
	}
	return usageError{"unknown operation"}
}

// Runner runs argv as the owner of the data directory with the given stdin
// and stdout and waits for it. A non-zero exit must be returned as the
// *exec.ExitError of the process. supervisor.Supervisor satisfies it.
type Runner interface {
	RunBrowserProcess(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer) error
}

// HelperStore implements Store by running the `__files` helper through a
// Runner.
type HelperStore struct {
	run    Runner
	self   string
	limits Limits
}

// NewHelperStore returns a Store that executes self (the runtime binary) with
// the `__files` subcommand for every operation. Zero limit fields take the
// defaults.
func NewHelperStore(run Runner, self string, limits Limits) *HelperStore {
	return &HelperStore{run: run, self: self, limits: limits.WithDefaults()}
}

func (h *HelperStore) argv(op string, area Area, extra ...string) []string {
	argv := []string{
		h.self, HelperName, op,
		"--area=" + string(area),
		"--quota=" + strconv.FormatInt(h.limits.QuotaBytes, 10),
		"--max-file=" + strconv.FormatInt(h.limits.MaxFileBytes, 10),
		"--max-files=" + strconv.Itoa(h.limits.MaxFiles),
	}
	return append(argv, extra...)
}

// mapExit turns a helper's exit status back into the store error it encodes.
func mapExit(err error) error {
	if err == nil {
		return nil
	}
	// *exec.ExitError satisfies this; the interface keeps the mapping
	// testable without spawning processes.
	var ee interface{ ExitCode() int }
	if !errors.As(err, &ee) {
		return err
	}
	code := ee.ExitCode()
	for _, e := range exitCodes {
		if e.code == code {
			return e.err
		}
	}
	return fmt.Errorf("transfer: helper failed with status %d", code)
}

// cappedBuffer collects at most max bytes and fails the writer beyond that.
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

var errHelperOutput = errors.New("transfer: helper output too large")

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.max {
		return 0, errHelperOutput
	}
	return c.buf.Write(p)
}

// List implements Store.
func (h *HelperStore) List(ctx context.Context, area Area) (Listing, error) {
	if !ValidArea(area) {
		return Listing{}, ErrInvalidArea
	}
	out := &cappedBuffer{max: maxHelperJSON}
	if err := h.run.RunBrowserProcess(ctx, h.argv("list", area), nil, out); err != nil {
		return Listing{}, mapExit(err)
	}
	var l Listing
	if err := json.Unmarshal(out.buf.Bytes(), &l); err != nil {
		return Listing{}, fmt.Errorf("transfer: decode helper listing: %w", err)
	}
	return l, nil
}

// getSink parses the get header and forwards the file bytes.
type getSink struct {
	begin   func(int64, time.Time) (io.Writer, error)
	hdr     []byte
	w       io.Writer
	size    int64
	written int64
}

func (g *getSink) Write(p []byte) (int, error) {
	total := len(p)
	if g.w == nil {
		g.hdr = append(g.hdr, p...)
		i := bytes.IndexByte(g.hdr, '\n')
		if i < 0 {
			if len(g.hdr) > maxGetHeader {
				return 0, errors.New("transfer: malformed helper header")
			}
			return total, nil
		}
		fields := strings.Fields(string(g.hdr[:i]))
		if len(fields) != 3 || fields[0] != getHeaderTag {
			return 0, errors.New("transfer: malformed helper header")
		}
		size, err1 := strconv.ParseInt(fields[1], 10, 64)
		ns, err2 := strconv.ParseInt(fields[2], 10, 64)
		if err1 != nil || err2 != nil || size < 0 {
			return 0, errors.New("transfer: malformed helper header")
		}
		w, err := g.begin(size, time.Unix(0, ns).UTC())
		if err != nil {
			return 0, err
		}
		g.w, g.size = w, size
		p = g.hdr[i+1:]
		g.hdr = nil
	}
	if len(p) > 0 {
		n, err := g.w.Write(p)
		g.written += int64(n)
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

// Get implements Store.
func (h *HelperStore) Get(ctx context.Context, area Area, name string, begin func(int64, time.Time) (io.Writer, error)) error {
	if !ValidArea(area) {
		return ErrInvalidArea
	}
	if err := ValidName(name); err != nil {
		return err
	}
	sink := &getSink{begin: begin}
	err := h.run.RunBrowserProcess(ctx, h.argv("get", area, "--name="+name), nil, sink)
	if err != nil {
		if sink.w == nil {
			return mapExit(err)
		}
		return fmt.Errorf("transfer: download interrupted: %w", err)
	}
	if sink.w == nil {
		return errors.New("transfer: helper produced no data")
	}
	if sink.written != sink.size {
		return fmt.Errorf("transfer: helper delivered %d of %d bytes", sink.written, sink.size)
	}
	return nil
}

// Put implements Store.
func (h *HelperStore) Put(ctx context.Context, name string, body io.Reader, size int64, overwrite bool) (Entry, error) {
	if err := ValidName(name); err != nil {
		return Entry{}, err
	}
	if size < 0 {
		return Entry{}, ErrSizeMismatch
	}
	if size > h.limits.MaxFileBytes {
		return Entry{}, ErrTooLarge
	}
	extra := []string{"--name=" + name, "--size=" + strconv.FormatInt(size, 10)}
	if overwrite {
		extra = append(extra, "--overwrite")
	}
	out := &cappedBuffer{max: maxHelperJSON}
	if err := h.run.RunBrowserProcess(ctx, h.argv("put", AreaUploads, extra...), body, out); err != nil {
		return Entry{}, mapExit(err)
	}
	var e Entry
	if err := json.Unmarshal(out.buf.Bytes(), &e); err != nil {
		return Entry{}, fmt.Errorf("transfer: decode helper entry: %w", err)
	}
	return e, nil
}

// Delete implements Store.
func (h *HelperStore) Delete(ctx context.Context, area Area, name string) error {
	if !ValidArea(area) {
		return ErrInvalidArea
	}
	if err := ValidName(name); err != nil {
		return err
	}
	return mapExit(h.run.RunBrowserProcess(ctx, h.argv("delete", area, "--name="+name), nil, io.Discard))
}
