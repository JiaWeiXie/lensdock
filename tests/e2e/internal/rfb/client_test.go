package rfb

import (
	"bytes"
	"context"
	"encoding/binary"
	"image/color"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// serve runs script against the first connection to a loopback listener and
// returns the address. The script's connection is closed when it returns.
func serve(t *testing.T, script func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		script(conn)
	}()
	return ln.Addr().String()
}

func readN(t *testing.T, r io.Reader, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		t.Errorf("server read %d bytes: %v", n, err)
	}
	return b
}

// handshake plays the server side of RFB 3.8 with security type None.
func handshake(t *testing.T, conn net.Conn, w, h int, name string) {
	t.Helper()
	_, _ = conn.Write([]byte("RFB 003.008\n"))
	if got := readN(t, conn, 12); string(got) != "RFB 003.008\n" {
		t.Errorf("client banner = %q", got)
	}
	_, _ = conn.Write([]byte{1, 1})
	if got := readN(t, conn, 1); got[0] != 1 {
		t.Errorf("client chose security type %d", got[0])
	}
	_, _ = conn.Write([]byte{0, 0, 0, 0})
	if got := readN(t, conn, 1); got[0] != 1 {
		t.Errorf("ClientInit shared flag = %d", got[0])
	}
	si := make([]byte, 24)
	binary.BigEndian.PutUint16(si[0:], uint16(w))
	binary.BigEndian.PutUint16(si[2:], uint16(h))
	si[4], si[5], si[6], si[7] = 32, 24, 0, 1 // server pixel format, overridden by the client
	binary.BigEndian.PutUint32(si[20:], uint32(len(name)))
	_, _ = conn.Write(append(si, name...))
}

func rawRect(x, y, w, h int, pix []byte) []byte {
	hdr := make([]byte, 12)
	binary.BigEndian.PutUint16(hdr[0:], uint16(x))
	binary.BigEndian.PutUint16(hdr[2:], uint16(y))
	binary.BigEndian.PutUint16(hdr[4:], uint16(w))
	binary.BigEndian.PutUint16(hdr[6:], uint16(h))
	return append(hdr, pix...)
}

func copyRectMsg(x, y, w, h, sx, sy int) []byte {
	hdr := rawRect(x, y, w, h, nil)
	binary.BigEndian.PutUint32(hdr[8:], 1)
	s := make([]byte, 4)
	binary.BigEndian.PutUint16(s[0:], uint16(sx))
	binary.BigEndian.PutUint16(s[2:], uint16(sy))
	return append(hdr, s...)
}

func updateMsg(rects ...[]byte) []byte {
	msg := []byte{0, 0, 0, byte(len(rects))}
	for _, r := range rects {
		msg = append(msg, r...)
	}
	return msg
}

func dial(t *testing.T, addr string) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestClientMessagesAreEncodedOnTheWire(t *testing.T) {
	cutText := []byte("ascii \xe7\xb9\x81\xe9\xab\x94") // UTF-8 bytes are sent verbatim
	type result struct{ got []byte }
	done := make(chan result, 1)
	addr := serve(t, func(conn net.Conn) {
		handshake(t, conn, 8, 4, "desk")
		// SetPixelFormat (20 bytes) and SetEncodings raw+CopyRect (4+8 bytes) follow the handshake.
		pf := readN(t, conn, 20)
		if pf[0] != 0 || pf[4] != 32 || pf[7] != 1 || binary.BigEndian.Uint16(pf[8:]) != 255 || pf[14] != 16 || pf[15] != 8 || pf[16] != 0 {
			t.Errorf("unexpected SetPixelFormat: % x", pf)
		}
		enc := readN(t, conn, 12)
		want := []byte{2, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 1}
		if !bytes.Equal(enc, want) {
			t.Errorf("SetEncodings = % x, want % x", enc, want)
		}
		done <- result{readN(t, conn, 8+6+8+8+len(cutText))}
	})
	c := dial(t, addr)
	if c.Width != 8 || c.Height != 4 || c.Name != "desk" || c.Version != "003.008" {
		t.Fatalf("ServerInit = %dx%d %q v%s", c.Width, c.Height, c.Name, c.Version)
	}
	if err := c.KeyEvent(true, 0xffe3); err != nil {
		t.Fatal(err)
	}
	if err := c.KeyEvent(false, 0xffe3); err != nil {
		t.Fatal(err)
	}
	if err := c.PointerEvent(ButtonLeft, 0x0102, 0x0304); err != nil {
		t.Fatal(err)
	}
	if err := c.ClientCutText(cutText); err != nil {
		t.Fatal(err)
	}
	var res result
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive client messages")
	}
	var want []byte
	want = append(want, 4, 1, 0, 0, 0, 0, 0xff, 0xe3) // key down
	want = append(want, 4, 0, 0, 0, 0, 0, 0xff, 0xe3) // key up
	want = append(want, 5, 1, 0x01, 0x02, 0x03, 0x04) // pointer
	want = append(want, 6, 0, 0, 0, 0, 0, 0, byte(len(cutText)))
	want = append(want, cutText...)
	if !bytes.Equal(res.got, want) {
		t.Fatalf("client bytes:\n got % x\nwant % x", res.got, want)
	}
}

func TestFetchFramebufferAppliesRawAndCopyRect(t *testing.T) {
	// 4x2 screen, BGRX pixels. Row 0 is filled by a raw rectangle; a CopyRect then
	// moves it onto row 1 (non-overlapping vertical copy).
	row := []byte{
		0x01, 0x02, 0x03, 0, // B,G,R -> RGB 03,02,01
		0x10, 0x20, 0x30, 0,
		0x40, 0x50, 0x60, 0,
		0x70, 0x80, 0x90, 0,
	}
	addr := serve(t, func(conn net.Conn) {
		handshake(t, conn, 4, 2, "d")
		readN(t, conn, 20+12) // SetPixelFormat, SetEncodings
		if req := readN(t, conn, 10); req[0] != 3 || req[1] != 0 {
			t.Errorf("expected a non-incremental FramebufferUpdateRequest, got % x", req)
		}
		_, _ = conn.Write([]byte{2})                                       // Bell is ignored
		_, _ = conn.Write(append([]byte{3, 0, 0, 0, 0, 0, 0, 2}, "hi"...)) // ServerCutText
		_, _ = conn.Write(updateMsg(rawRect(0, 0, 4, 1, row)))             // first half of the screen
		_, _ = conn.Write(updateMsg(copyRectMsg(0, 1, 4, 1, 0, 0)))        // second half
	})
	c := dial(t, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	img, cuts, err := c.FetchFramebuffer(ctx)
	if err != nil {
		t.Fatalf("FetchFramebuffer: %v", err)
	}
	if len(cuts) != 1 || string(cuts[0]) != "hi" {
		t.Errorf("cut texts = %q, want [hi]", cuts)
	}
	for y := range 2 {
		got := img.RGBAAt(1, y)
		if want := (color.RGBA{R: 0x30, G: 0x20, B: 0x10, A: 0xff}); got != want {
			t.Errorf("pixel (1,%d) = %v, want %v", y, got, want)
		}
	}
	if got := img.RGBAAt(0, 1); got != (color.RGBA{R: 3, G: 2, B: 1, A: 0xff}) {
		t.Errorf("copied pixel (0,1) = %v", got)
	}
	if png, err := EncodePNG(img); err != nil || !bytes.HasPrefix(png, []byte("\x89PNG")) {
		t.Errorf("EncodePNG = %v, %v", len(png), err)
	}
}

func TestCopyRectOverlappingDownwardCopiesBottomUp(t *testing.T) {
	c := &Client{Width: 1, Height: 4, fb: make([]byte, 16)}
	for i := range 4 {
		c.fb[i*4] = byte(i + 1)
	}
	c.copyRect(0, 1, 0, 0, 1, 3) // rows 0..2 move to rows 1..3
	for i, want := range []byte{1, 1, 2, 3} {
		if c.fb[i*4] != want {
			t.Fatalf("row %d = %d, want %d", i, c.fb[i*4], want)
		}
	}
}

func TestDiscardPixelsConsumesPayloadWithoutStoringIt(t *testing.T) {
	pix := bytes.Repeat([]byte{9}, 2*2*4)
	addr := serve(t, func(conn net.Conn) {
		handshake(t, conn, 2, 2, "d")
		readN(t, conn, 20+12)
		_, _ = conn.Write(updateMsg(rawRect(0, 0, 2, 2, pix)))
		_, _ = conn.Write([]byte{2})
	})
	c := dial(t, addr)
	c.DiscardPixels = true
	m, err := c.ReadMessage()
	if err != nil || m.Rects != 1 || m.Pixels != 4 || m.Bytes != 16 {
		t.Fatalf("update = %+v, %v", m, err)
	}
	if next, err := c.ReadMessage(); err != nil || next.Type != 2 {
		t.Fatalf("stream out of sync after discarded payload: %+v, %v", next, err)
	}
	if bytes.Contains(c.fb, []byte{9}) {
		t.Fatal("pixels were stored despite DiscardPixels")
	}
}

func TestHandshakeFailures(t *testing.T) {
	reason := func(s string) []byte {
		b := make([]byte, 4, 4+len(s))
		binary.BigEndian.PutUint32(b, uint32(len(s)))
		return append(b, s...)
	}
	tests := []struct {
		name    string
		script  func(net.Conn)
		wantErr string
	}{
		{"not an RFB server", func(c net.Conn) { _, _ = c.Write([]byte("HTTP/1.1 400 \n")) }, "unexpected banner"},
		{"protocol too old", func(c net.Conn) { _, _ = c.Write([]byte("RFB 003.003\n")) }, "older than 3.7"},
		{"connection refused with reason", func(c net.Conn) {
			_, _ = c.Write([]byte("RFB 003.008\n"))
			readN(t, c, 12)
			_, _ = c.Write(append([]byte{0}, reason("too many clients")...))
		}, "too many clients"},
		{"None not offered", func(c net.Conn) {
			_, _ = c.Write([]byte("RFB 003.008\n"))
			readN(t, c, 12)
			_, _ = c.Write([]byte{1, 2}) // VNC authentication only
		}, "does not offer security type None"},
		{"security result failure", func(c net.Conn) {
			_, _ = c.Write([]byte("RFB 003.008\n"))
			readN(t, c, 12)
			_, _ = c.Write([]byte{1, 1})
			readN(t, c, 1)
			_, _ = c.Write(append([]byte{0, 0, 0, 1}, reason("denied")...))
		}, "denied"},
		{"zero sized screen", func(c net.Conn) { handshake(t, c, 0, 0, "x") }, "implausible framebuffer size"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			addr := serve(t, tc.script)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err := Dial(ctx, addr)
			if err == nil {
				_ = c.Close()
				t.Fatal("Dial succeeded")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestReadMessageRejectsProtocolViolations(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		wantErr string
	}{
		{"unrequested encoding", func() []byte {
			r := rawRect(0, 0, 1, 1, nil)
			binary.BigEndian.PutUint32(r[8:], 7) // Tight
			return updateMsg(r)
		}(), "unrequested encoding"},
		{"rectangle outside the screen", updateMsg(rawRect(1, 1, 2, 2, nil)), "outside"},
		{"unknown message type", []byte{99}, "unknown server message"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			addr := serve(t, func(conn net.Conn) {
				handshake(t, conn, 2, 2, "d")
				readN(t, conn, 20+12)
				_, _ = conn.Write(tc.payload)
			})
			c := dial(t, addr)
			if _, err := c.ReadMessage(); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestDialContextDeadlineAbortsSilentServer(t *testing.T) {
	addr := serve(t, func(conn net.Conn) { time.Sleep(3 * time.Second) })
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if c, err := Dial(ctx, addr); err == nil {
		_ = c.Close()
		t.Fatal("Dial succeeded against a silent server")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Dial took %v despite a 150ms context", d)
	}
}

func TestDistinctColorsStopsAtLimit(t *testing.T) {
	c := &Client{Width: 2, Height: 1, fb: []byte{1, 0, 0, 0, 2, 0, 0, 0}}
	img := c.Image()
	if got := DistinctColors(img, 100); got != 2 {
		t.Fatalf("DistinctColors = %d, want 2", got)
	}
	if got := DistinctColors(img, 1); got != 1 {
		t.Fatalf("DistinctColors with limit 1 = %d, want 1", got)
	}
}
