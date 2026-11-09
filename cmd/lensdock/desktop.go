package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/JiaWeiXie/lensdock/internal/apiclient"
	"github.com/JiaWeiXie/lensdock/internal/rfb"
)

// desktopUsageText is appended to the CLI usage text.
const desktopUsageText = `  lensdock desktop screenshot --output FILE.png [lease flags]
      full-desktop PNG of the whole Chromium window (toolbar, extension popups, menus, chrome:// pages), not only the
      page; prints {"file","width","height",...}. Pointer coordinates of every desktop command are pixels of the most
      recent screenshot (origin top left) and are checked against the live desktop size
  lensdock desktop move X Y [after flags] [lease flags]
  lensdock desktop click X Y [--button left|middle|right] [--count N] [after flags] [lease flags]
  lensdock desktop drag X1 Y1 X2 Y2 [--button left|middle|right] [--steps N] [after flags] [lease flags]
  lensdock desktop scroll X Y [--dx N] [--dy N] [after flags] [lease flags]    (positive dy scrolls down)
  lensdock desktop key CHORD... [after flags] [lease flags]
      CHORD is Return, Escape, Tab, F6, ctrl+l, ctrl+shift+t, alt+Left, ctrl+plus, a single character or 0xKEYSYM;
      each CHORD is pressed and released in turn, so the address bar, menus and native dialogs can be operated
  lensdock desktop focus [lease flags]
      prints only the focused page element's role, accessible name and selector; native browser controls are not page elements
  lensdock desktop type [TEXT] [--method auto|keys|paste] [after flags] [lease flags]    (TEXT or stdin)
      auto types printable ASCII as key events and pastes anything else (Unicode) through the clipboard of the lease
  lensdock desktop sequence STEP... [after flags] [lease flags]
      one connection, in order. STEP is move:X,Y  down[:BUTTON]  up[:BUTTON]  click:X,Y  drag:X1,Y1,X2,Y2
      scroll:X,Y,DX,DY  key:CHORD  text:STRING  wait:DURATION   (hold and release buttons with down/up)
      after flags: --screenshot FILE.png [--wait 400ms] saves a fresh full-desktop PNG after the input
` + agentLeaseUsageText

const (
	pointerSettle  = 25 * time.Millisecond
	clickHold      = 40 * time.Millisecond
	doubleClickGap = 80 * time.Millisecond
	keyHold        = 12 * time.Millisecond
	keyGap         = 25 * time.Millisecond
	dragStepGap    = 12 * time.Millisecond
	pasteSettle    = 600 * time.Millisecond
	inputSync      = 250 * time.Millisecond
	fetchTimeout   = 30 * time.Second
	handshakeLimit = 15 * time.Second
)

// screenQuiet: RFB has no completion signal, so a screen counts as settled
// when the server sent no update for this long after the last one. It is
// shorter than the 500 ms caret blink, which would otherwise keep the screen
// "changing" forever.
const screenQuiet = 120 * time.Millisecond

// desktopStep is one parsed, validated action on the connected desktop.
type desktopStep func(ctx context.Context, d *desktopSession) error

// desktopSession is one RFB connection to the desktop under one AGENT lease.
type desktopSession struct {
	rc    *rfb.Client
	lease *agentLease
	// msgs carries what the reader goroutine reads; closed when it ends.
	msgs chan rfb.Message

	buttons uint8
	x, y    int
}

// desktopFlags are the flags every desktop command shares.
type desktopFlags struct {
	*agentLeaseFlags
	screenshot string
	wait       time.Duration
}

func (c *cli) addDesktopFlags(fs *flag.FlagSet, after bool) *desktopFlags {
	df := &desktopFlags{agentLeaseFlags: c.addAgentLeaseFlags(fs)}
	if after {
		fs.StringVar(&df.screenshot, "screenshot", "", "after the input, write a fresh full-desktop PNG to this file")
		fs.DurationVar(&df.wait, "wait", 400*time.Millisecond, "longest time to watch the screen before --screenshot is taken; it is taken earlier once the screen has stopped changing, and a screen that never stops (video) gets the latest complete frame at this limit")
	}
	return df
}

// desktopCommand dispatches "lensdock desktop ...".
func (c *cli) desktopCommand(ctx context.Context, args []string) error {
	return c.subcommand(args, "desktop", map[string]func([]string) error{
		"screenshot": func(a []string) error { return c.desktopScreenshot(ctx, a) },
		"move":       func(a []string) error { return c.desktopMove(ctx, a) },
		"click":      func(a []string) error { return c.desktopClick(ctx, a) },
		"drag":       func(a []string) error { return c.desktopDrag(ctx, a) },
		"scroll":     func(a []string) error { return c.desktopScroll(ctx, a) },
		"key":        func(a []string) error { return c.desktopKey(ctx, a) },
		"focus":      func(a []string) error { return c.desktopFocus(ctx, a) },
		"type":       func(a []string) error { return c.desktopType(ctx, a) },
		"sequence":   func(a []string) error { return c.desktopSequence(ctx, a) },
	})
}

func (c *cli) desktopScreenshot(ctx context.Context, args []string) error {
	fs := c.newFlagSet("desktop screenshot")
	df := c.addDesktopFlags(fs, false)
	output := fs.String("output", "", "PNG file to write (mode 0600; an existing file is replaced)")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	if *output == "" {
		return usageErrorf("--output is required")
	}
	df.screenshot, df.wait = *output, 0
	return c.runDesktop(ctx, df, nil)
}

func (c *cli) desktopMove(ctx context.Context, args []string) error {
	fs := c.newFlagSet("desktop move")
	df := c.addDesktopFlags(fs, true)
	pos, err := parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	x, y, err := parsePoint(pos[0], pos[1])
	if err != nil {
		return err
	}
	return c.runDesktop(ctx, df, []desktopStep{stepMove(x, y)})
}

func (c *cli) desktopClick(ctx context.Context, args []string) error {
	fs := c.newFlagSet("desktop click")
	df := c.addDesktopFlags(fs, true)
	button := fs.String("button", "left", "left, middle or right")
	count := fs.Int("count", 1, "1 for a click, 2 for a double click, 3 for a triple click")
	pos, err := parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	x, y, err := parsePoint(pos[0], pos[1])
	if err != nil {
		return err
	}
	b, err := rfb.ParseButton(*button)
	if err != nil {
		return usageErrorf("--button: %v", err)
	}
	if *count < 1 {
		return usageErrorf("--count must be at least 1")
	}
	return c.runDesktop(ctx, df, []desktopStep{stepClick(x, y, b, *count)})
}

func (c *cli) desktopDrag(ctx context.Context, args []string) error {
	fs := c.newFlagSet("desktop drag")
	df := c.addDesktopFlags(fs, true)
	button := fs.String("button", "left", "left, middle or right")
	steps := fs.Int("steps", 12, "intermediate pointer moves between the two points")
	pos, err := parse(fs, args, 4, 4)
	if err != nil {
		return err
	}
	x1, y1, err := parsePoint(pos[0], pos[1])
	if err != nil {
		return err
	}
	x2, y2, err := parsePoint(pos[2], pos[3])
	if err != nil {
		return err
	}
	b, err := rfb.ParseButton(*button)
	if err != nil {
		return usageErrorf("--button: %v", err)
	}
	if *steps < 1 {
		return usageErrorf("--steps must be at least 1")
	}
	return c.runDesktop(ctx, df, []desktopStep{stepDrag(x1, y1, x2, y2, b, *steps)})
}

func (c *cli) desktopScroll(ctx context.Context, args []string) error {
	fs := c.newFlagSet("desktop scroll")
	df := c.addDesktopFlags(fs, true)
	dx := fs.Int("dx", 0, "wheel clicks to the right (negative: left)")
	dy := fs.Int("dy", 0, "wheel clicks down (negative: up)")
	pos, err := parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	x, y, err := parsePoint(pos[0], pos[1])
	if err != nil {
		return err
	}
	if *dx == 0 && *dy == 0 {
		return usageErrorf("--dx or --dy is required")
	}
	return c.runDesktop(ctx, df, []desktopStep{stepScroll(x, y, *dx, *dy)})
}

func (c *cli) desktopKey(ctx context.Context, args []string) error {
	fs := c.newFlagSet("desktop key")
	df := c.addDesktopFlags(fs, true)
	pos, err := parse(fs, args, 1, math.MaxInt)
	if err != nil {
		return err
	}
	steps := make([]desktopStep, 0, len(pos))
	for _, spec := range pos {
		step, err := parseKeyStep(spec)
		if err != nil {
			return err
		}
		steps = append(steps, step)
	}
	return c.runDesktop(ctx, df, steps)
}

func (c *cli) desktopType(ctx context.Context, args []string) error {
	fs := c.newFlagSet("desktop type")
	df := c.addDesktopFlags(fs, true)
	method := fs.String("method", "auto", "auto, keys (key events) or paste (clipboard of the lease, then Ctrl+V)")
	pos, err := parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	var text string
	if len(pos) == 1 {
		text = pos[0]
	} else {
		data, err := io.ReadAll(c.stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		text = string(data)
	}
	step, err := parseTextStep(text, *method)
	if err != nil {
		return err
	}
	return c.runDesktop(ctx, df, []desktopStep{step})
}

func (c *cli) desktopSequence(ctx context.Context, args []string) error {
	fs := c.newFlagSet("desktop sequence")
	df := c.addDesktopFlags(fs, true)
	pos, err := parse(fs, args, 1, math.MaxInt)
	if err != nil {
		return err
	}
	steps := make([]desktopStep, 0, len(pos))
	for _, spec := range pos {
		step, err := parseSequenceStep(spec)
		if err != nil {
			return err
		}
		steps = append(steps, step)
	}
	return c.runDesktop(ctx, df, steps)
}

// runDesktop is the lifecycle of every desktop command: lease (own or
// reused), lease-fenced control socket, RFB handshake, the steps, an optional
// fresh screenshot, then control socket closed before the lease is released.
// A nil steps slice means screenshot only.
func (c *cli) runDesktop(ctx context.Context, df *desktopFlags, steps []desktopStep) error {
	if df.wait < 0 {
		return usageErrorf("--wait must not be negative")
	}
	leaseCtx, l, err := c.openAgentLease(ctx, df.agentLeaseFlags)
	if err != nil {
		return err
	}
	defer l.close()
	conn, err := l.client.DialDesktop(leaseCtx, l.lf.WorkspaceID, l.lf.CapabilityToken, l.lf.Lease.Epoch, l.lf.Lease.RuntimeGeneration)
	if err != nil {
		return l.explain(err)
	}
	hctx, cancel := context.WithTimeout(leaseCtx, handshakeLimit)
	rc, err := rfb.NewClient(hctx, conn)
	cancel()
	if err != nil {
		_ = conn.Close()
		return l.explain(err)
	}
	// The socket closes before the lease is released (defers run last in
	// first out), and a close frame from the runtime, which it sends when
	// the lease ends, cancels dctx at once through the reader.
	dctx, dcancel := context.WithCancelCause(leaseCtx)
	defer dcancel(nil)
	d := &desktopSession{rc: rc, lease: l, msgs: make(chan rfb.Message, 64)}
	defer closeDesktop(rc, conn)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		d.read(dctx, dcancel)
	}()

	result, err := d.run(dctx, df, steps)
	if err != nil {
		return l.explain(desktopFailure(dctx, readerDone, err))
	}
	result["leaseId"] = l.lf.Lease.LeaseID
	return c.writeJSONLine(result)
}

// desktopFailure lets the sole reader publish the peer's close reason before a
// concurrent write's generic net.ErrClosed wins. A lease close frame can shut
// down the WebSocket while its reader is still returning that frame. Do not
// infer a lease failure from net.ErrClosed itself: absent a classified peer
// reason, keep the original transport error. The wait is bounded even if the
// reader cannot finish (for example, its message queue is full).
func desktopFailure(ctx context.Context, readerDone <-chan struct{}, err error) error {
	if errors.Is(err, net.ErrClosed) && context.Cause(ctx) == nil {
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-readerDone:
		case <-ctx.Done():
		case <-timer.C:
		}
	}
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return err
}

// run executes the steps and the optional screenshot on the connected desktop.
func (d *desktopSession) run(ctx context.Context, df *desktopFlags, steps []desktopStep) (map[string]any, error) {
	result := map[string]any{"width": d.rc.Width, "height": d.rc.Height}
	// Complete the initial framebuffer exchange before sending input. x11vnc
	// can serve its cached connection-start frame on that first request.
	if err := d.fetchFrame(ctx); err != nil {
		return nil, err
	}
	for _, step := range steps {
		if err := step(ctx, d); err != nil {
			_ = d.releaseButtons()
			return nil, err
		}
	}
	if err := d.releaseButtons(); err != nil {
		return nil, err
	}
	if df.screenshot == "" {
		// A barrier so the events reached x11vnc before the socket closes: a
		// quiet screen sends nothing and that is fine.
		if err := d.rc.RequestUpdate(true); err != nil {
			return nil, err
		}
		select {
		case <-d.msgs:
		case <-time.After(inputSync):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return result, nil
	}
	shot, err := d.screenshot(ctx, df.screenshot, df.wait)
	if err != nil {
		return nil, err
	}
	result["screenshot"] = shot
	return result, nil
}

// read is the only reader of the RFB connection. It hands messages to the
// command and, when the connection ends for any reason (the runtime closes
// it with 4001 or 4002 when the lease ends), ends the command's context.
func (d *desktopSession) read(ctx context.Context, cancel context.CancelCauseFunc) {
	defer close(d.msgs)
	for {
		m, err := d.rc.ReadMessage()
		if err != nil {
			if ctx.Err() == nil {
				cancel(fmt.Errorf("desktop connection closed: %w", err))
			}
			return
		}
		select {
		case d.msgs <- m:
		case <-ctx.Done():
			return
		}
	}
}

// explain turns a failure on the lease's channels into the CLI's lease error
// when the lease was the cause.
func (l *agentLease) explain(err error) error {
	if lost := l.err(); lost != nil {
		return lost
	}
	if apiclient.DesktopLeaseEnded(err) {
		return leaseErrorf("the lease ended; the desktop connection was closed: %v", err)
	}
	return err
}

func closeDesktop(rc *rfb.Client, conn net.Conn) {
	_ = rc.Close()
	_ = conn.Close()
}

// fetchFrame requests a complete framebuffer after the previous exchange ended.
func (d *desktopSession) fetchFrame(ctx context.Context) error {
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	if err := d.rc.RequestUpdate(false); err != nil {
		return err
	}
	for covered := 0; covered < d.rc.Width*d.rc.Height; {
		select {
		case m, ok := <-d.msgs:
			if !ok {
				return errors.New("desktop connection closed before the frame arrived")
			}
			if m.IsFramebufferUpdate() {
				covered += m.Pixels
			}
		case <-fctx.Done():
			return fmt.Errorf("desktop frame did not arrive: %w", fctx.Err())
		}
	}
	return nil
}

// screenshot observes the screen for at most budget (never less than
// screenQuiet), then atomically writes the latest complete frame as a PNG.
func (d *desktopSession) screenshot(ctx context.Context, path string, budget time.Duration) (map[string]any, error) {
	if err := d.settle(ctx, max(budget, screenQuiet)); err != nil {
		return nil, err
	}
	data, err := rfb.EncodePNG(d.rc.Image())
	if err != nil {
		return nil, err
	}
	if err := replacePrivateFile(path, data); err != nil {
		return nil, err
	}
	return map[string]any{"file": path, "width": d.rc.Width, "height": d.rc.Height, "bytes": len(data)}, nil
}

// settle applies the server's incremental updates to the local framebuffer
// until the screen stopped changing or the observation budget is spent,
// whichever comes first. It is not a claim that the browser finished
// painting: RFB cannot know that.
//
// A full-frame request is no synchronization point: x11vnc answers it with
// whatever it has scanned so far, which is a torn frame while the browser is
// still repainting (the typed omnibox text new, its dropdown old). An
// incremental request is answered with every region changed since the last
// answer, so the loop keeps exactly one incremental request outstanding and
// applies each answer. Nothing more for screenQuiet after an answer ends the
// wait early; a screen that never stops changing (video, animation) ends it
// when the budget is spent. Either way the frame is the latest complete
// update, and only a real failure of the connection or the context is an
// error.
func (d *desktopSession) settle(ctx context.Context, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	quiet := budget // until the first answer only the budget bounds the wait
	for request := true; ; {
		if request {
			if err := d.rc.RequestUpdate(true); err != nil {
				return err
			}
			request = false
		}
		wait := min(quiet, time.Until(deadline))
		if wait <= 0 {
			return ctx.Err()
		}
		timer := time.NewTimer(wait)
		select {
		case m, ok := <-d.msgs:
			timer.Stop()
			if !ok {
				return errors.New("desktop connection closed before the screen settled")
			}
			if m.IsFramebufferUpdate() {
				quiet, request = screenQuiet, true
			}
		case <-timer.C:
			return ctx.Err()
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

// replacePrivateFile writes data to path (mode 0600) through a temporary file
// in the same directory, so a reader never sees a partial PNG and a symlink at
// path is replaced rather than followed.
func replacePrivateFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lensdock-shot-*")
	if err != nil {
		return fmt.Errorf("write screenshot: %w", err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write screenshot: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write screenshot: %w", err)
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// ---- input primitives ----------------------------------------------------

func (d *desktopSession) checkPoint(x, y int) error {
	if x < 0 || y < 0 || x >= d.rc.Width || y >= d.rc.Height {
		return usageErrorf("point (%d,%d) is outside the %dx%d desktop; coordinates are pixels of a fresh screenshot", x, y, d.rc.Width, d.rc.Height)
	}
	return nil
}

// pointer sends one pointer event and remembers the button state, so a
// command that ends with a button held releases it.
func (d *desktopSession) pointer(buttons uint8, x, y int) error {
	if err := d.rc.PointerEvent(buttons, x, y); err != nil {
		return err
	}
	d.buttons, d.x, d.y = buttons, x, y
	return nil
}

func (d *desktopSession) moveAndSettle(ctx context.Context, x, y int) error {
	if err := d.pointer(d.buttons, x, y); err != nil {
		return err
	}
	return sleepCtx(ctx, pointerSettle)
}

func (d *desktopSession) releaseButtons() error {
	if d.buttons == 0 {
		return nil
	}
	return d.pointer(0, d.x, d.y)
}

func stepMove(x, y int) desktopStep {
	return func(ctx context.Context, d *desktopSession) error {
		if err := d.checkPoint(x, y); err != nil {
			return err
		}
		return d.moveAndSettle(ctx, x, y)
	}
}

func stepClick(x, y int, button uint8, count int) desktopStep {
	return func(ctx context.Context, d *desktopSession) error {
		if err := d.checkPoint(x, y); err != nil {
			return err
		}
		if err := d.moveAndSettle(ctx, x, y); err != nil {
			return err
		}
		for i := range count {
			if i > 0 {
				if err := sleepCtx(ctx, doubleClickGap); err != nil {
					return err
				}
			}
			if err := d.pointer(d.buttons|button, x, y); err != nil {
				return err
			}
			if err := sleepCtx(ctx, clickHold); err != nil {
				return err
			}
			if err := d.pointer(d.buttons&^button, x, y); err != nil {
				return err
			}
		}
		return nil
	}
}

func stepButton(button uint8, down bool) desktopStep {
	return func(ctx context.Context, d *desktopSession) error {
		buttons := d.buttons &^ button
		if down {
			buttons |= button
		}
		if err := d.pointer(buttons, d.x, d.y); err != nil {
			return err
		}
		return sleepCtx(ctx, clickHold)
	}
}

func stepDrag(x1, y1, x2, y2 int, button uint8, steps int) desktopStep {
	return func(ctx context.Context, d *desktopSession) error {
		if err := errors.Join(d.checkPoint(x1, y1), d.checkPoint(x2, y2)); err != nil {
			return err
		}
		if err := d.moveAndSettle(ctx, x1, y1); err != nil {
			return err
		}
		if err := d.pointer(d.buttons|button, x1, y1); err != nil {
			return err
		}
		if err := sleepCtx(ctx, clickHold); err != nil {
			return err
		}
		for i := 1; i <= steps; i++ {
			x := x1 + (x2-x1)*i/steps
			y := y1 + (y2-y1)*i/steps
			if err := d.pointer(d.buttons, x, y); err != nil {
				return err
			}
			if err := sleepCtx(ctx, dragStepGap); err != nil {
				return err
			}
		}
		if err := sleepCtx(ctx, clickHold); err != nil {
			return err
		}
		return d.pointer(d.buttons&^button, x2, y2)
	}
}

func stepScroll(x, y, dx, dy int) desktopStep {
	return func(ctx context.Context, d *desktopSession) error {
		if err := d.checkPoint(x, y); err != nil {
			return err
		}
		if err := d.moveAndSettle(ctx, x, y); err != nil {
			return err
		}
		wheel := func(button uint8, n int) error {
			for range n {
				if err := d.pointer(d.buttons|button, x, y); err != nil {
					return err
				}
				if err := d.pointer(d.buttons&^button, x, y); err != nil {
					return err
				}
				if err := sleepCtx(ctx, keyHold); err != nil {
					return err
				}
			}
			return nil
		}
		down, right := rfb.ButtonWheelDown, rfb.ButtonWheelRight
		if dy < 0 {
			down = rfb.ButtonWheelUp
		}
		if dx < 0 {
			right = rfb.ButtonWheelLeft
		}
		return errors.Join(wheel(down, abs(dy)), wheel(right, abs(dx)))
	}
}

func stepChord(keys []uint32) desktopStep {
	return func(ctx context.Context, d *desktopSession) error {
		for _, k := range keys {
			if err := d.rc.KeyEvent(true, k); err != nil {
				return err
			}
		}
		if err := sleepCtx(ctx, keyHold); err != nil {
			return err
		}
		for i := len(keys) - 1; i >= 0; i-- {
			if err := d.rc.KeyEvent(false, keys[i]); err != nil {
				return err
			}
		}
		return sleepCtx(ctx, keyGap)
	}
}

func stepKeys(keysyms []uint32) desktopStep {
	return func(ctx context.Context, d *desktopSession) error {
		for _, k := range keysyms {
			if err := d.rc.Key(k); err != nil {
				return err
			}
			if err := sleepCtx(ctx, keyHold); err != nil {
				return err
			}
		}
		return nil
	}
}

// stepPaste puts text on the X clipboard through the lease-bound clipboard
// route, which also keeps it owned only as long as the lease lives, then
// presses Ctrl+V and gives the browser time to read it before the lease can
// end.
func stepPaste(text string) desktopStep {
	return func(ctx context.Context, d *desktopSession) error {
		l := d.lease
		if err := l.client.SetClipboard(ctx, l.lf.WorkspaceID, l.lf.CapabilityToken, []byte(text), l.lf.Lease.Epoch, l.lf.Lease.RuntimeGeneration); err != nil {
			return err
		}
		if err := stepChord([]uint32{rfb.KeyControlL, rfb.KeyV})(ctx, d); err != nil {
			return err
		}
		return sleepCtx(ctx, pasteSettle)
	}
}

// ---- argument parsing ----------------------------------------------------

func parsePoint(xs, ys string) (int, int, error) {
	x, errX := strconv.Atoi(xs)
	y, errY := strconv.Atoi(ys)
	if errX != nil || errY != nil || x < 0 || y < 0 || x > 0xffff || y > 0xffff {
		return 0, 0, usageErrorf("coordinates must be non-negative integers, got %q %q", xs, ys)
	}
	return x, y, nil
}

func parseKeyStep(spec string) (desktopStep, error) {
	keys, err := rfb.ParseChord(spec)
	if err != nil {
		return nil, usageErrorf("%v", err)
	}
	return stepChord(keys), nil
}

// asciiTypable reports whether text is printable ASCII, tab or line breaks,
// which key events type reliably on any keyboard layout.
func asciiTypable(text string) bool {
	for _, r := range text {
		if !(r >= 0x20 && r <= 0x7e) && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
	}
	return true
}

func parseTextStep(text, method string) (desktopStep, error) {
	if text == "" {
		return nil, usageErrorf("there is no text to type")
	}
	if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return nil, usageErrorf("the text must be valid UTF-8 without NUL")
	}
	switch method {
	case "auto":
		if asciiTypable(text) {
			method = "keys"
		} else {
			method = "paste"
		}
	case "keys", "paste":
	default:
		return nil, usageErrorf("--method must be auto, keys or paste")
	}
	if method == "paste" {
		return stepPaste(text), nil
	}
	keysyms := make([]uint32, 0, len(text))
	for _, r := range text {
		k, ok := rfb.KeysymForRune(r)
		if !ok {
			return nil, usageErrorf("the character %U cannot be typed as a key event; use --method paste", r)
		}
		keysyms = append(keysyms, k)
	}
	return stepKeys(keysyms), nil
}

// parseSequenceStep parses one "name:args" step of "desktop sequence".
func parseSequenceStep(spec string) (desktopStep, error) {
	name, arg, _ := strings.Cut(spec, ":")
	ints := func(n int) ([]int, error) {
		parts := strings.Split(arg, ",")
		if len(parts) != n {
			return nil, usageErrorf("step %q needs %d comma-separated integers", spec, n)
		}
		out := make([]int, n)
		for i, p := range parts {
			v, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil {
				return nil, usageErrorf("step %q: %q is not an integer", spec, p)
			}
			out[i] = v
		}
		return out, nil
	}
	point := func(v []int) error {
		for i := 0; i+1 < len(v); i += 2 {
			if v[i] < 0 || v[i+1] < 0 || v[i] > 0xffff || v[i+1] > 0xffff {
				return usageErrorf("step %q: coordinates must be non-negative", spec)
			}
		}
		return nil
	}
	button := func() (uint8, error) {
		if arg == "" {
			return rfb.ButtonLeft, nil
		}
		b, err := rfb.ParseButton(arg)
		if err != nil {
			return 0, usageErrorf("step %q: %v", spec, err)
		}
		return b, nil
	}
	switch name {
	case "move", "click":
		v, err := ints(2)
		if err != nil {
			return nil, err
		}
		if err := point(v); err != nil {
			return nil, err
		}
		if name == "move" {
			return stepMove(v[0], v[1]), nil
		}
		return stepClick(v[0], v[1], rfb.ButtonLeft, 1), nil
	case "drag":
		v, err := ints(4)
		if err != nil {
			return nil, err
		}
		if err := point(v); err != nil {
			return nil, err
		}
		return stepDrag(v[0], v[1], v[2], v[3], rfb.ButtonLeft, 12), nil
	case "scroll":
		v, err := ints(4)
		if err != nil {
			return nil, err
		}
		if err := point(v[:2]); err != nil {
			return nil, err
		}
		if v[2] == 0 && v[3] == 0 {
			return nil, usageErrorf("step %q: DX,DY must not both be 0", spec)
		}
		return stepScroll(v[0], v[1], v[2], v[3]), nil
	case "down", "up":
		b, err := button()
		if err != nil {
			return nil, err
		}
		return stepButton(b, name == "down"), nil
	case "key":
		return parseKeyStep(arg)
	case "text":
		return parseTextStep(arg, "auto")
	case "wait":
		w, err := time.ParseDuration(arg)
		if err != nil || w < 0 {
			return nil, usageErrorf("step %q: wait needs a non-negative duration such as 500ms", spec)
		}
		return func(ctx context.Context, _ *desktopSession) error { return sleepCtx(ctx, w) }, nil
	}
	return nil, usageErrorf("unknown sequence step %q (move, click, drag, scroll, down, up, key, text, wait)", spec)
}
