package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/JiaWeiXie/lensdock/internal/apiclient"
	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
)

const (
	fakeAgentIdentity = "ldi_agent"
	fakeLeaseToken    = "ldl_auto"
	fakeLeaseID       = "lse_auto"
	fakeEpoch         = 7
	fakeGeneration    = "gen-9"
	fakeWidth         = 48
	fakeHeight        = 32
)

// desktopGateway plays the Gateway: AGENT lease endpoints, the lease-fenced
// control socket with an RFB server behind it, the clipboard and CDP discovery.
type desktopGateway struct {
	srv *httptest.Server

	mu        sync.Mutex
	events    []string
	pointers  [][3]int // buttons, x, y
	keys      []string
	clipboard []string

	acquires atomic.Int32
	renews   atomic.Int32
	releases atomic.Int32
	wsOpen   atomic.Int32
	// endLeaseAfter, when positive, makes the runtime end the lease that long
	// after the control socket opened, with close code 4001.
	endLeaseAfter time.Duration
	// stagedRepaint makes a key repaint the screen in two steps, like a
	// browser updating its omnibox before its dropdown: the second step
	// shows up only after the client has asked for another update.
	stagedRepaint bool
	// animate answers every incremental request with a changed frame, so
	// the screen never stops changing.
	animate bool
}

func newDesktopGateway(t *testing.T) *desktopGateway {
	t.Helper()
	f := &desktopGateway{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *desktopGateway) log(event string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
}

func (f *desktopGateway) snapshot() (events []string, pointers [][3]int, keys, clip []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.events), slices.Clone(f.pointers), slices.Clone(f.keys), slices.Clone(f.clipboard)
}

func (f *desktopGateway) grantLease() apiwire.Lease {
	return apiwire.Lease{
		LeaseID: fakeLeaseID, WorkspaceID: "default", HolderID: "bot", Mode: "AGENT",
		Epoch: fakeEpoch, RuntimeGeneration: fakeGeneration, ExpiresAt: time.Now().Add(30 * time.Second),
	}
}

func (f *desktopGateway) serve(w http.ResponseWriter, r *http.Request) {
	const ws = "/api/v1/workspaces/default"
	bearer := r.Header.Get("Authorization")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == ws+"/leases":
		if bearer != "Bearer "+fakeAgentIdentity {
			apiJSONError(w, 401, apiwire.CodeUnauthenticated)
			return
		}
		var req apiwire.AcquireLease
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.acquires.Add(1)
		f.log("acquire:" + req.Mode)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(apiwire.LeaseGrant{Lease: f.grantLease(), CapabilityToken: fakeLeaseToken})
	case r.Method == http.MethodPost && r.URL.Path == ws+"/leases/"+fakeLeaseID+"/renew":
		if bearer != "Bearer "+fakeLeaseToken {
			apiJSONError(w, 401, apiwire.CodeUnauthenticated)
			return
		}
		f.renews.Add(1)
		_ = json.NewEncoder(w).Encode(f.grantLease())
	case r.Method == http.MethodDelete && r.URL.Path == ws+"/leases/"+fakeLeaseID:
		if bearer != "Bearer "+fakeLeaseToken {
			apiJSONError(w, 401, apiwire.CodeUnauthenticated)
			return
		}
		// The runtime waits for the in-flight control socket of a lease it
		// ends; a client that releases first stalls here until it gives up.
		for deadline := time.Now().Add(2 * time.Second); f.wsOpen.Load() > 0 && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}
		f.releases.Add(1)
		f.log("release")
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut && r.URL.Path == ws+"/clipboard":
		if bearer != "Bearer "+fakeLeaseToken || r.Header.Get("X-LensDock-Lease-Epoch") != "7" {
			apiJSONError(w, 401, apiwire.CodeUnauthenticated)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.clipboard = append(f.clipboard, string(body))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == ws+"/cdp/json/version":
		if bearer != "Bearer "+fakeLeaseToken {
			apiJSONError(w, 401, apiwire.CodeUnauthenticated)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Browser":"Chrome/1","webSocketDebuggerUrl":"ws://runtime.invalid/api/v1/workspaces/default/cdp/devtools/browser/abc-123"}`)
	case r.Method == http.MethodGet && r.URL.Path == ws+"/vnc/control":
		q := r.URL.Query()
		if bearer != "Bearer "+fakeLeaseToken || q.Get("epoch") != "7" || q.Get("generation") != fakeGeneration {
			apiJSONError(w, 412, apiwire.CodeEpochMismatch)
			return
		}
		f.controlSocket(w, r)
	default:
		apiJSONError(w, 404, apiwire.CodeNotFound)
	}
}

func (f *desktopGateway) controlSocket(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"binary"}, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	f.wsOpen.Add(1)
	f.log("ws-open")
	defer func() {
		f.log("ws-closed")
		f.wsOpen.Add(-1)
	}()
	if f.endLeaseAfter > 0 {
		time.AfterFunc(f.endLeaseAfter, func() { _ = c.Close(4001, "lease ended") })
	}
	conn := websocket.NetConn(r.Context(), c, websocket.MessageBinary)
	f.rfbServer(conn)
	_ = c.CloseNow()
}

// fakePixel is the colour of the fake desktop at (x, y), as BGRX bytes.
func fakePixel(x, y int) [4]byte { return [4]byte{byte(x), byte(y), 200, 0} }

// rfbServer is the server side of RFB 3.8 with security type None, Raw rectangles.
func (f *desktopGateway) rfbServer(conn net.Conn) {
	read := func(n int) []byte {
		b := make([]byte, n)
		if _, err := io.ReadFull(conn, b); err != nil {
			return nil
		}
		return b
	}
	_, _ = conn.Write([]byte("RFB 003.008\n"))
	if read(12) == nil {
		return
	}
	_, _ = conn.Write([]byte{1, 1})
	if read(1) == nil {
		return
	}
	_, _ = conn.Write([]byte{0, 0, 0, 0})
	if read(1) == nil {
		return
	}
	init := make([]byte, 24)
	binary.BigEndian.PutUint16(init[0:], fakeWidth)
	binary.BigEndian.PutUint16(init[2:], fakeHeight)
	binary.BigEndian.PutUint32(init[20:], 4)
	_, _ = conn.Write(append(init, "desk"...))
	// live is the server's screen: the red channel of each row. It starts
	// uniform; dirty rows are those changed since the last answer.
	var live [fakeHeight]byte
	for y := range live {
		live[y] = fakePixel(0, y)[2]
	}
	dirtyFrom, dirtyTo, stage := fakeHeight, 0, 0
	frame := 0
	paint := func(from, to int) {
		for y := from; y < to; y++ {
			live[y] = 100
		}
		dirtyFrom, dirtyTo = min(dirtyFrom, from), max(dirtyTo, to)
	}
	sendRows := func(from, to int) {
		msg := []byte{0, 0, 0, 1, 0, 0, byte(from >> 8), byte(from), 0, fakeWidth, byte((to - from) >> 8), byte(to - from), 0, 0, 0, 0}
		for y := from; y < to; y++ {
			for x := range fakeWidth {
				p := fakePixel(x, y)
				p[2] = live[y]
				msg = append(msg, p[:]...)
			}
		}
		_, _ = conn.Write(msg)
	}
	for {
		t := read(1)
		if t == nil {
			return
		}
		switch t[0] {
		case 0: // SetPixelFormat
			read(19)
		case 2: // SetEncodings
			h := read(3)
			if h == nil {
				return
			}
			read(4 * int(binary.BigEndian.Uint16(h[1:3])))
		case 3: // FramebufferUpdateRequest
			b := read(9)
			if b == nil {
				return
			}
			switch {
			case b[0] == 0: // full frame: whatever the server has scanned so far
				sendRows(0, fakeHeight)
				dirtyFrom, dirtyTo = fakeHeight, 0
			case dirtyFrom < dirtyTo: // incremental: everything changed since the last answer
				sendRows(dirtyFrom, dirtyTo)
				dirtyFrom, dirtyTo = fakeHeight, 0
			case f.animate: // video: every answer carries a new frame, forever
				frame++
				live[0] = byte(1 + frame%50)
				sendRows(0, 1)
			case f.stagedRepaint && stage == 1: // the browser repaints the dropdown once asked again
				stage = 2
				paint(10, 20)
				sendRows(dirtyFrom, dirtyTo)
				dirtyFrom, dirtyTo = fakeHeight, 0
			}
		case 4: // KeyEvent
			b := read(7)
			if b == nil {
				return
			}
			dir := "up"
			if b[0] == 1 {
				dir = "down"
			}
			f.mu.Lock()
			f.keys = append(f.keys, dir+":0x"+strconv.FormatUint(uint64(binary.BigEndian.Uint32(b[3:7])), 16))
			f.mu.Unlock()
			if f.stagedRepaint && stage == 0 {
				stage = 1
				paint(0, 10)
			}
		case 5: // PointerEvent
			b := read(5)
			if b == nil {
				return
			}
			f.mu.Lock()
			f.pointers = append(f.pointers, [3]int{int(b[0]), int(binary.BigEndian.Uint16(b[1:3])), int(binary.BigEndian.Uint16(b[3:5]))})
			f.mu.Unlock()
		case 6: // ClientCutText
			b := read(7)
			if b == nil {
				return
			}
			read(int(binary.BigEndian.Uint32(b[3:7])))
		default:
			return
		}
	}
}

// agentArgs are the standalone connection flags of the fake Gateway.
func (f *desktopGateway) agentArgs(t *testing.T) []string {
	t.Helper()
	tok := filepath.Join(t.TempDir(), "agent-token")
	writeFile(t, tok, fakeAgentIdentity+"\n", 0o600)
	return []string{"--workspace", "default", "--token-file", tok, "--api", f.srv.URL}
}

func TestDesktopClickAcquiresItsOwnAgentLeaseAndReleasesIt(t *testing.T) {
	f := newDesktopGateway(t)
	args := append([]string{"desktop", "click", "10", "20"}, f.agentArgs(t)...)
	r := runCLI(t, context.Background(), "", nil, args...)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	var out struct {
		LeaseID string `json:"leaseId"`
		Width   int    `json:"width"`
		Height  int    `json:"height"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil || out.LeaseID != fakeLeaseID || out.Width != fakeWidth || out.Height != fakeHeight {
		t.Fatalf("stdout %q (%v)", r.stdout, err)
	}
	events, pointers, _, _ := f.snapshot()
	if want := []string{"acquire:AGENT", "ws-open", "ws-closed", "release"}; !slices.Equal(events, want) {
		t.Errorf("events = %v, want %v", events, want)
	}
	if want := [][3]int{{0, 10, 20}, {1, 10, 20}, {0, 10, 20}}; !slices.Equal(pointers, want) {
		t.Errorf("pointer events = %v, want %v", pointers, want)
	}
}

func TestDesktopScreenshotReturnsTheFullDesktopAsPNG(t *testing.T) {
	f := newDesktopGateway(t)
	file := filepath.Join(t.TempDir(), "shot.png")
	writeFile(t, file, "stale", 0o600) // a repeated screenshot replaces the file
	args := append([]string{"desktop", "screenshot", "--output", file}, f.agentArgs(t)...)
	r := runCLI(t, context.Background(), "", nil, args...)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	var out struct {
		Width, Height int
		Screenshot    struct {
			File          string
			Width, Height int
		}
	}
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil || out.Screenshot.File != file || out.Screenshot.Width != fakeWidth || out.Screenshot.Height != fakeHeight {
		t.Fatalf("stdout %q (%v)", r.stdout, err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not a PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != fakeWidth || b.Dy() != fakeHeight {
		t.Fatalf("PNG is %dx%d, want %dx%d", b.Dx(), b.Dy(), fakeWidth, fakeHeight)
	}
	if cr, cg, cb, _ := img.At(5, 7).RGBA(); cr>>8 != 200 || cg>>8 != 7 || cb>>8 != 5 {
		t.Errorf("pixel (5,7) = %d,%d,%d, want 200,7,5", cr>>8, cg>>8, cb>>8)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("screenshot mode = %v, %v; want 0600", fi, err)
	}
}

func TestDesktopScreenshotWaitsForTheBrowserToFinishRepainting(t *testing.T) {
	f := newDesktopGateway(t)
	f.stagedRepaint = true
	file := filepath.Join(t.TempDir(), "after.png")
	args := append([]string{"desktop", "key", "ctrl+l", "--screenshot", file, "--wait", "0s"}, f.agentArgs(t)...)
	if r := runCLI(t, context.Background(), "", nil, args...); r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	// The first answer after the key carries the omnibox rows; the dropdown
	// rows only repaint once the client asks for the next update. A frame
	// taken from a full-frame request, or after the first answer, is torn.
	for y, want := range map[int]uint32{0: 100, 9: 100, 10: 100, 19: 100, 20: 200, fakeHeight - 1: 200} {
		if red, _, _, _ := img.At(5, y).RGBA(); red>>8 != want {
			t.Errorf("row %d: red = %d, want %d (a torn frame was written)", y, red>>8, want)
		}
	}
}

func TestDesktopScreenshotOfAnIdleScreenReturnsWithoutWaitingForChanges(t *testing.T) {
	f := newDesktopGateway(t)
	file := filepath.Join(t.TempDir(), "idle.png")
	args := append([]string{"desktop", "key", "Escape", "--screenshot", file, "--wait", "0s"}, f.agentArgs(t)...)
	start := time.Now()
	if r := runCLI(t, context.Background(), "", nil, args...); r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("an unchanging screen kept the command for %s", elapsed)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopScreenshotOfAScreenThatNeverStopsChangingReturnsACurrentFrameWithinWait(t *testing.T) {
	f := newDesktopGateway(t)
	f.animate = true
	file := filepath.Join(t.TempDir(), "video.png")
	args := append([]string{"desktop", "key", "Escape", "--screenshot", file, "--wait", "500ms"}, f.agentArgs(t)...)
	start := time.Now()
	r := runCLI(t, context.Background(), "", nil, args...)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("a changing screen kept the command for %s although --wait was 500ms", elapsed)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	// Row 0 carries the animation; any value but the initial 200 shows that
	// the PNG holds frames received after the initial full-frame request.
	if red, _, _, _ := img.At(5, 0).RGBA(); red>>8 == 200 || red>>8 == 0 {
		t.Errorf("row 0 red = %d, want an animated value", red>>8)
	}
	if red, _, _, _ := img.At(5, 5).RGBA(); red>>8 != 200 {
		t.Errorf("row 5 red = %d, want the unchanged 200", red>>8)
	}
}

func TestDesktopLeaseEndStillFailsWhileTheScreenKeepsChanging(t *testing.T) {
	f := newDesktopGateway(t)
	f.animate = true
	f.endLeaseAfter = 200 * time.Millisecond
	file := filepath.Join(t.TempDir(), "never.png")
	args := append([]string{"desktop", "key", "Escape", "--screenshot", file, "--wait", "1h"}, f.agentArgs(t)...)
	start := time.Now()
	r := runCLI(t, context.Background(), "", nil, args...)
	if r.code != exitLease {
		t.Fatalf("exit %d stderr %q, want %d", r.code, r.stderr, exitLease)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %s to notice that the lease ended", elapsed)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("a screenshot was written after the lease ended: %v", err)
	}
}

func TestDesktopDragHoldsTheButtonAcrossMovesAndReleasesIt(t *testing.T) {
	f := newDesktopGateway(t)
	args := append([]string{"desktop", "drag", "2", "3", "22", "13", "--steps", "2"}, f.agentArgs(t)...)
	if r := runCLI(t, context.Background(), "", nil, args...); r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	_, pointers, _, _ := f.snapshot()
	want := [][3]int{{0, 2, 3}, {1, 2, 3}, {1, 12, 8}, {1, 22, 13}, {0, 22, 13}}
	if !slices.Equal(pointers, want) {
		t.Errorf("pointer events = %v, want %v", pointers, want)
	}
}

func TestDesktopSequenceReleasesAHeldButtonWhenTheCommandEnds(t *testing.T) {
	f := newDesktopGateway(t)
	args := append([]string{"desktop", "sequence", "move:4,5", "down:right", "move:6,7"}, f.agentArgs(t)...)
	if r := runCLI(t, context.Background(), "", nil, args...); r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	_, pointers, _, _ := f.snapshot()
	want := [][3]int{{0, 4, 5}, {4, 4, 5}, {4, 6, 7}, {0, 6, 7}}
	if !slices.Equal(pointers, want) {
		t.Errorf("pointer events = %v, want %v", pointers, want)
	}
}

func TestDesktopTypePastesUnicodeThroughTheLeaseClipboard(t *testing.T) {
	f := newDesktopGateway(t)
	text := "\u7e41\u9ad4\u4e2d\u6587 \U0001F600"
	args := append([]string{"desktop", "type", text}, f.agentArgs(t)...)
	if r := runCLI(t, context.Background(), "", nil, args...); r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	_, _, keys, clip := f.snapshot()
	if !slices.Equal(clip, []string{text}) {
		t.Errorf("clipboard = %q, want %q", clip, text)
	}
	if want := []string{"down:0xffe3", "down:0x76", "up:0x76", "up:0xffe3"}; !slices.Equal(keys, want) {
		t.Errorf("key events = %v, want Ctrl+V %v", keys, want)
	}
}

func TestDesktopTypeSendsASCIIAsKeyEventsWithoutTouchingTheClipboard(t *testing.T) {
	f := newDesktopGateway(t)
	args := append([]string{"desktop", "type", "Hi\n"}, f.agentArgs(t)...)
	if r := runCLI(t, context.Background(), "", nil, args...); r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	_, _, keys, clip := f.snapshot()
	if len(clip) != 0 {
		t.Errorf("clipboard written for ASCII text: %q", clip)
	}
	want := []string{"down:0x48", "up:0x48", "down:0x69", "up:0x69", "down:0xff0d", "up:0xff0d"}
	if !slices.Equal(keys, want) {
		t.Errorf("key events = %v, want %v", keys, want)
	}
}

func TestDesktopWithALeaseFileRenewsItAndNeverReleasesIt(t *testing.T) {
	f := newDesktopGateway(t)
	path := filepath.Join(t.TempDir(), "lease.json")
	if err := apiclient.WriteLeaseFile(path, apiclient.LeaseFile{
		APIBaseURL: f.srv.URL, WorkspaceID: "default", Lease: f.grantLease(), CapabilityToken: auth.Secret(fakeLeaseToken),
	}); err != nil {
		t.Fatal(err)
	}
	r := runCLI(t, context.Background(), "", nil, "desktop", "key", "Return", "--lease-file", path)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if f.acquires.Load() != 0 || f.releases.Load() != 0 || f.renews.Load() == 0 {
		t.Errorf("acquires %d, releases %d, renews %d; a reused lease is renewed and left to its owner", f.acquires.Load(), f.releases.Load(), f.renews.Load())
	}
	_, _, keys, _ := f.snapshot()
	if want := []string{"down:0xff0d", "up:0xff0d"}; !slices.Equal(keys, want) {
		t.Errorf("key events = %v, want %v", keys, want)
	}
}

func TestDesktopRefusesLeasesAndFlagsItMustNotUse(t *testing.T) {
	f := newDesktopGateway(t)
	dir := t.TempDir()
	human := writeLease(t, dir, f.srv.URL, "HUMAN", "ldl_human")
	agent := writeLease(t, dir, f.srv.URL, "AGENT", "ldl_agent")
	for name, args := range map[string][]string{
		"human lease":               {"desktop", "key", "Return", "--lease-file", human},
		"lease file and workspace":  {"desktop", "key", "Return", "--lease-file", agent, "--workspace", "default"},
		"ttl out of range":          {"desktop", "key", "Return", "--workspace", "default", "--ttl", "5"},
		"feedback revision alone":   {"desktop", "key", "Return", "--workspace", "default", "--feedback-revision", "2"},
		"no connection":             {"desktop", "key", "Return"},
		"unknown chord":             {"desktop", "key", "nosuchkey", "--lease-file", agent},
		"negative coordinate":       {"desktop", "move", "-1", "2"},
		"scroll without a distance": {"desktop", "scroll", "1", "2"},
		"unknown sequence step":     {"desktop", "sequence", "teleport:1,2"},
		"type method":               {"desktop", "type", "x", "--method", "telepathy"},
	} {
		t.Run(name, func(t *testing.T) {
			r := runCLI(t, context.Background(), "", nil, args...)
			if r.code != exitUsage {
				t.Errorf("exit %d, want %d (stderr %q)", r.code, exitUsage, r.stderr)
			}
		})
	}
	if f.acquires.Load() != 0 || f.renews.Load() != 0 || f.wsOpen.Load() != 0 {
		t.Errorf("a refused command reached the Gateway: acquires %d, renews %d", f.acquires.Load(), f.renews.Load())
	}
}

func TestDesktopRejectsAPointOutsideTheDesktopAndStillReleasesItsLease(t *testing.T) {
	f := newDesktopGateway(t)
	args := append([]string{"desktop", "click", "500", "20"}, f.agentArgs(t)...)
	r := runCLI(t, context.Background(), "", nil, args...)
	if r.code != exitUsage || !strings.Contains(r.stderr, "outside the 48x32 desktop") {
		t.Fatalf("exit %d stderr %q", r.code, r.stderr)
	}
	if f.releases.Load() != 1 {
		t.Errorf("releases = %d, want the command's own lease released", f.releases.Load())
	}
	if _, pointers, _, _ := f.snapshot(); len(pointers) != 0 {
		t.Errorf("pointer events sent for a rejected point: %v", pointers)
	}
}

func TestDesktopStopsPromptlyWhenTheRuntimeEndsTheLease(t *testing.T) {
	f := newDesktopGateway(t)
	f.endLeaseAfter = 100 * time.Millisecond
	args := append([]string{"desktop", "sequence", "wait:30s", "click:1,1"}, f.agentArgs(t)...)
	start := time.Now()
	r := runCLI(t, context.Background(), "", nil, args...)
	if r.code != exitLease {
		t.Fatalf("exit %d stderr %q, want %d", r.code, r.stderr, exitLease)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %s to notice that the lease ended", elapsed)
	}
	if _, pointers, _, _ := f.snapshot(); len(pointers) != 0 {
		t.Errorf("input was sent after the lease ended: %v", pointers)
	}
}

func TestDesktopDoesNotTakeOverAWorkspaceHeldByAHuman(t *testing.T) {
	var takeovers atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "takeover") {
			takeovers.Add(1)
		}
		apiJSONError(w, http.StatusConflict, apiwire.CodeLeaseConflict)
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "agent-token")
	writeFile(t, tok, fakeAgentIdentity+"\n", 0o600)
	r := runCLI(t, context.Background(), "", nil, "desktop", "click", "1", "1", "--workspace", "default", "--token-file", tok, "--api", srv.URL)
	if r.code == 0 {
		t.Fatal("click succeeded while the workspace was held")
	}
	if takeovers.Load() != 0 {
		t.Errorf("the command called takeover %d times", takeovers.Load())
	}
}

func TestBridgeAcquiresRenewsAndReleasesItsOwnLeaseAndCanShareIt(t *testing.T) {
	f := newDesktopGateway(t)
	dir := t.TempDir()
	urlFile := filepath.Join(dir, "bridge.json")
	shared := filepath.Join(dir, "shared-lease.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan result, 1)
	args := append([]string{"cdp", "bridge", "--url-file", urlFile, "--lease-file-out", shared}, f.agentArgs(t)...)
	go func() { done <- runCLI(t, ctx, "", nil, args...) }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if line, _ := os.ReadFile(urlFile); bytes.HasSuffix(line, []byte("\n")) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	lf, err := apiclient.ReadLeaseFile(shared)
	if err != nil {
		cancel()
		r := <-done
		t.Fatalf("shared lease file: %v (exit %d stderr %q)", err, r.code, r.stderr)
	}
	if lf.Lease.LeaseID != fakeLeaseID || lf.Lease.Mode != "AGENT" {
		t.Errorf("shared lease = %+v", lf.Lease)
	}
	if f.releases.Load() != 0 {
		t.Error("the lease was released while the bridge was serving")
	}
	// A desktop command under the shared lease works while the bridge holds it.
	if r := runCLI(t, context.Background(), "", nil, "desktop", "key", "Escape", "--lease-file", shared); r.code != 0 {
		t.Errorf("desktop with the bridge's lease: exit %d: %s", r.code, r.stderr)
	}
	if f.releases.Load() != 0 {
		t.Error("a desktop command released the bridge's lease")
	}

	cancel()
	select {
	case r := <-done:
		if r.code != 0 {
			t.Fatalf("bridge exit %d: %s", r.code, r.stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("bridge did not stop")
	}
	if f.acquires.Load() != 1 || f.releases.Load() != 1 {
		t.Errorf("acquires %d, releases %d; want the bridge's one lease acquired and released", f.acquires.Load(), f.releases.Load())
	}
	if _, err := os.Stat(shared); !os.IsNotExist(err) {
		t.Errorf("shared lease file outlived the bridge: %v", err)
	}
}
