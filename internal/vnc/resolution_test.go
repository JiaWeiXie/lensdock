package vnc

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// xrandrFixture mirrors what Xvfb prints: the framebuffer's maximum is far
// larger than the initial screen, and only the current mode is listed.
const xrandrFixture = `Screen 0: minimum 1 x 1, current %d x %d, maximum %d x %d
screen connected primary %dx%d+0+0 0mm x 0mm
   %dx%d     0.00*
`

type fakeX struct {
	cur   Size
	max   Size
	calls [][]string
	stick bool
	fail  error
}

func (f *fakeX) OutputBrowserProcess(_ context.Context, argv []string, _ int) ([]byte, error) {
	f.calls = append(f.calls, slices.Clone(argv))
	if f.fail != nil {
		return nil, f.fail
	}
	switch {
	case slices.Equal(argv[1:], []string{"--query"}):
		shown := f.cur
		return []byte(fmt.Sprintf(xrandrFixture, shown.Width, shown.Height, f.max.Width, f.max.Height, shown.Width, shown.Height, shown.Width, shown.Height)), nil
	case len(argv) == 3 && argv[1] == "--fb":
		if !f.stick {
			var w, h int
			_, _ = fmt.Sscanf(argv[2], "%dx%d", &w, &h)
			f.cur = Size{Width: w, Height: h}
		}
		return nil, nil
	}
	return nil, errors.New("unexpected argv")
}

func (f *fakeX) sets() [][]string {
	var out [][]string
	for _, c := range f.calls {
		if len(c) == 3 {
			out = append(out, c)
		}
	}
	return out
}

func TestParseXrandr(t *testing.T) {
	out := fmt.Sprintf(xrandrFixture, 1920, 1080, 32767, 32767, 1920, 1080, 1920, 1080)
	cur, max, err := parseXrandr([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if cur != (Size{1920, 1080}) || max != (Size{32767, 32767}) {
		t.Fatalf("cur=%v max=%v", cur, max)
	}
	if _, _, err := parseXrandr([]byte("Can't open display")); err == nil {
		t.Fatal("output without a Screen line must fail")
	}
}

func TestQueryListsAllowedSizesWithinTheServerMaximum(t *testing.T) {
	z := NewResizer(&fakeX{cur: Size{1920, 1080}, max: Size{1600, 900}})
	d, err := z.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Size{{1600, 900}, {1440, 900}, {1366, 768}, {1280, 800}, {1280, 720}, {1024, 768}, {800, 600}}
	if d.Current != (Size{1920, 1080}) || !slices.Equal(d.Supported, want) {
		t.Fatalf("display = %+v, want supported %v", d, want)
	}
	// An Xvfb-like maximum admits every allowed size.
	d, err = NewResizer(&fakeX{cur: Size{800, 600}, max: Size{32767, 32767}}).Query(context.Background())
	if err != nil || !slices.Equal(d.Supported, AllowedSizes) {
		t.Fatalf("unbounded server: %+v, %v", d, err)
	}
}

func TestSetResizesTheFramebufferWithFixedArgvAndReadsBack(t *testing.T) {
	x := &fakeX{cur: Size{1920, 1080}, max: Size{32767, 32767}}
	z := NewResizer(x)
	got, err := z.Set(context.Background(), Size{1280, 720})
	if err != nil || got != (Size{1280, 720}) {
		t.Fatalf("Set = %v, %v", got, err)
	}
	sets := x.sets()
	if len(sets) != 1 || !slices.Equal(sets[0], []string{"/usr/bin/xrandr", "--fb", "1280x720"}) {
		t.Fatalf("xrandr set calls = %v", sets)
	}
	// Already at the size: no second xrandr call.
	if got, err := z.Set(context.Background(), Size{1280, 720}); err != nil || got != (Size{1280, 720}) {
		t.Fatalf("Set same size = %v, %v", got, err)
	}
	if n := len(x.sets()); n != 1 {
		t.Fatalf("same-size Set ran xrandr again (%d calls)", n)
	}
	// Growing back to the startup size is allowed.
	if got, err := z.Set(context.Background(), Size{1920, 1080}); err != nil || got != (Size{1920, 1080}) {
		t.Fatalf("Set grow back = %v, %v", got, err)
	}
}

func TestSetRejectsSizesOutsideTheListOrTheMaximum(t *testing.T) {
	x := &fakeX{cur: Size{1024, 768}, max: Size{1280, 1024}}
	z := NewResizer(x)
	for name, s := range map[string]Size{
		"not allowed": {640, 480},
		"too wide":    {1680, 1050}, // allowed in the list, beyond the server maximum
		"absurd":      {3840, 2160},
	} {
		if _, err := z.Set(context.Background(), s); !errors.Is(err, ErrUnsupportedSize) {
			t.Errorf("%s: Set(%v) = %v, want ErrUnsupportedSize", name, s, err)
		}
	}
	if sets := x.sets(); len(sets) != 0 {
		t.Fatalf("xrandr resized for a rejected size: %v", sets)
	}
}

func TestSetDetectsAResizeThatDidNotTakeEffect(t *testing.T) {
	x := &fakeX{cur: Size{1920, 1080}, max: Size{1920, 1080}, stick: true}
	got, err := NewResizer(x).Set(context.Background(), Size{1600, 900})
	if !errors.Is(err, ErrResizeFailed) || got != (Size{1920, 1080}) {
		t.Fatalf("Set = %v, %v, want ErrResizeFailed with the actual size", got, err)
	}
}

func TestResizerSurfacesXrandrFailure(t *testing.T) {
	x := &fakeX{cur: Size{1920, 1080}, max: Size{1920, 1080}, fail: errors.New("exit status 1")}
	if _, err := NewResizer(x).Set(context.Background(), Size{1600, 900}); err == nil || errors.Is(err, ErrUnsupportedSize) {
		t.Fatalf("Set = %v, want a plain failure", err)
	}
	if _, err := NewResizer(x).Query(context.Background()); err == nil || !strings.Contains(err.Error(), "xrandr") {
		t.Fatalf("Query = %v", err)
	}
}
