package vnc

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"
)

const (
	xrandrBinary    = "/usr/bin/xrandr"
	xrandrTimeout   = 10 * time.Second
	maxXrandrOutput = 64 << 10
)

// Size is a display size in pixels.
type Size struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

func (s Size) String() string { return fmt.Sprintf("%dx%d", s.Width, s.Height) }

// AllowedSizes is the fixed list of display sizes a client may request. The
// largest entry is the size Xvfb starts with, so a request can never grow the
// framebuffer beyond its declared geometry; the X server's own maximum is an
// additional bound.
var AllowedSizes = []Size{
	{1920, 1080},
	{1680, 1050},
	{1600, 900},
	{1440, 900},
	{1366, 768},
	{1280, 800},
	{1280, 720},
	{1024, 768},
	{800, 600},
}

var (
	// ErrUnsupportedSize is returned for a size that is not in AllowedSizes or
	// exceeds the X server's maximum.
	ErrUnsupportedSize = errors.New("vnc: display size not supported")
	// ErrResizeFailed is returned when xrandr succeeded but the screen did not
	// reach the requested size.
	ErrResizeFailed = errors.New("vnc: display resize did not take effect")
)

// Runner runs argv as the browser user (the X display owner) and returns its
// stdout. supervisor.Supervisor satisfies it.
type Runner interface {
	OutputBrowserProcess(ctx context.Context, argv []string, maxBytes int) ([]byte, error)
}

// Resizer queries and changes the X screen size with xrandr. It applies
// `xrandr --fb WxH`, which is how the Xvfb framebuffer is resized, and always
// reads the size back instead of assuming the request took effect.
type Resizer struct {
	run Runner
	mu  sync.Mutex
}

// NewResizer returns a Resizer that runs xrandr through run.
func NewResizer(run Runner) *Resizer { return &Resizer{run: run} }

// Display is the current and selectable display sizes.
type Display struct {
	Current   Size   `json:"current"`
	Supported []Size `json:"supported"`
}

var screenLineRE = regexp.MustCompile(`(?m)^Screen \d+: .*current (\d+) x (\d+), maximum (\d+) x (\d+)`)

// parseXrandr extracts the current and maximum size from `xrandr --query`
// output.
func parseXrandr(out []byte) (cur, max Size, err error) {
	m := screenLineRE.FindSubmatch(out)
	if m == nil {
		return Size{}, Size{}, errors.New("vnc: xrandr output has no Screen line")
	}
	nums := make([]int, 4)
	for i := range nums {
		n, convErr := strconv.Atoi(string(m[i+1]))
		if convErr != nil {
			return Size{}, Size{}, errors.New("vnc: xrandr output has an invalid size")
		}
		nums[i] = n
	}
	return Size{nums[0], nums[1]}, Size{nums[2], nums[3]}, nil
}

// supported returns the AllowedSizes the server maximum admits.
func supported(max Size) []Size {
	out := make([]Size, 0, len(AllowedSizes))
	for _, s := range AllowedSizes {
		if s.Width <= max.Width && s.Height <= max.Height {
			out = append(out, s)
		}
	}
	return out
}

func (z *Resizer) query(ctx context.Context) (Display, Size, error) {
	out, err := z.run.OutputBrowserProcess(ctx, []string{xrandrBinary, "--query"}, maxXrandrOutput)
	if err != nil {
		return Display{}, Size{}, fmt.Errorf("vnc: xrandr --query: %w", err)
	}
	cur, max, err := parseXrandr(out)
	if err != nil {
		return Display{}, Size{}, err
	}
	return Display{Current: cur, Supported: supported(max)}, max, nil
}

// Query returns the current size and the sizes Set would accept.
func (z *Resizer) Query(ctx context.Context) (Display, error) {
	ctx, cancel := context.WithTimeout(ctx, xrandrTimeout)
	defer cancel()
	z.mu.Lock()
	defer z.mu.Unlock()
	d, _, err := z.query(ctx)
	return d, err
}

// Set changes the screen to want and returns the size read back from the X
// server. want must be in AllowedSizes and within the server's maximum.
func (z *Resizer) Set(ctx context.Context, want Size) (Size, error) {
	allowed := false
	for _, s := range AllowedSizes {
		if s == want {
			allowed = true
			break
		}
	}
	if !allowed {
		return Size{}, ErrUnsupportedSize
	}
	ctx, cancel := context.WithTimeout(ctx, xrandrTimeout)
	defer cancel()
	z.mu.Lock()
	defer z.mu.Unlock()

	before, max, err := z.query(ctx)
	if err != nil {
		return Size{}, err
	}
	if before.Current == want {
		return want, nil
	}
	selectable := false
	for _, s := range supported(max) {
		if s == want {
			selectable = true
			break
		}
	}
	if !selectable {
		return before.Current, ErrUnsupportedSize
	}
	if _, err := z.run.OutputBrowserProcess(ctx, []string{xrandrBinary, "--fb", want.String()}, maxXrandrOutput); err != nil {
		return before.Current, fmt.Errorf("vnc: xrandr --fb %s: %w", want, err)
	}
	after, _, err := z.query(ctx)
	if err != nil {
		return Size{}, err
	}
	if after.Current != want {
		return after.Current, ErrResizeFailed
	}
	return after.Current, nil
}
