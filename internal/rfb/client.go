// Package rfb is a minimal RFB 3.8 client: the LensDock desktop CLI drives
// the runtime's control x11vnc through the lease-fenced WebSocket route with
// it, and the end-to-end tests dial x11vnc directly.
//
// It speaks exactly what x11vnc offers without authentication (security type
// 1, "None"; the Gateway and Runtime authenticate the WebSocket before any
// byte reaches x11vnc), requests only the Raw and CopyRect encodings and
// keeps an RGBA copy of the framebuffer so callers can write it out as a PNG.
// It deliberately has no encryption, no RFB authentication and no support for
// other encodings.
package rfb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"sync"
	"time"
)

// X11 keysyms used by the tests and the desktop CLI.
const (
	KeyBackSpace uint32 = 0xff08
	KeyReturn    uint32 = 0xff0d
	KeyControlL  uint32 = 0xffe3
	KeyShiftL    uint32 = 0xffe1
	KeyV         uint32 = 0x0076
	KeyA         uint32 = 0x0061
)

// Pointer button masks. The wheel is buttons 4 to 7 of the X protocol.
const (
	ButtonLeft       uint8 = 1 << 0
	ButtonMiddle     uint8 = 1 << 1
	ButtonRight      uint8 = 1 << 2
	ButtonWheelUp    uint8 = 1 << 3
	ButtonWheelDown  uint8 = 1 << 4
	ButtonWheelLeft  uint8 = 1 << 5
	ButtonWheelRight uint8 = 1 << 6
)

// RFB message types.
const (
	clientSetPixelFormat        = 0
	clientSetEncodings          = 2
	clientFBUpdateRequest       = 3
	clientKeyEvent              = 4
	clientPointerEvent          = 5
	clientCutText               = 6
	serverFBUpdate              = 0
	serverSetColourMap          = 1
	serverBell                  = 2
	serverCutText               = 3
	encodingRaw           int32 = 0
	encodingCopyRect      int32 = 1
)

const (
	maxDimension = 16384
	maxNameLen   = 4096
	maxCutText   = 16 << 20
)

// ErrNoUpdate is returned by FetchFramebuffer when the server never covered the
// whole screen before the context ended.
var ErrNoUpdate = errors.New("rfb: framebuffer was not fully delivered")

// Client is one RFB connection. Methods that send are safe for concurrent use
// with each other; reading (ReadMessage and the helpers built on it) must be
// done by a single goroutine.
type Client struct {
	conn net.Conn
	br   *bufio.Reader

	wmu sync.Mutex

	// Version is the protocol banner the server sent, for example "003.008".
	Version string
	// Name is the desktop name from ServerInit.
	Name          string
	Width, Height int

	// DiscardPixels makes ReadMessage drop rectangle payloads instead of
	// storing them. Load generators set it to avoid framebuffer copies.
	DiscardPixels bool

	// fbmu guards fb: the reading goroutine applies updates while another
	// goroutine may take Image snapshots.
	fbmu sync.Mutex
	fb   []byte // Width*Height*4, BGRX as negotiated with SetPixelFormat
}

// Message is the outcome of one ReadMessage call.
type Message struct {
	Type    uint8
	Rects   int    // FramebufferUpdate: number of rectangles
	Pixels  int    // FramebufferUpdate: pixels covered by all rectangles
	Bytes   int    // payload bytes consumed, excluding the message header
	CutText []byte // ServerCutText: raw bytes
}

// Dial connects to addr, performs the RFB 3.8 handshake with the None security
// type and negotiates a 32-bit true-colour BGRX pixel format plus Raw and
// CopyRect encodings. ctx bounds the whole handshake.
func Dial(ctx context.Context, addr string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("rfb: dial %s: %w", addr, err)
	}
	c, err := NewClient(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

// NewClient runs the handshake over an established connection. On error the
// caller still owns conn.
func NewClient(ctx context.Context, conn net.Conn) (*Client, error) {
	c := &Client{conn: conn, br: bufio.NewReaderSize(conn, 64<<10)}
	stop := c.bindContext(ctx)
	defer stop()
	if err := c.handshake(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("rfb: handshake: %w", ctxErr)
		}
		return nil, err
	}
	return c, nil
}

// bindContext applies ctx's deadline to the connection and aborts blocked I/O
// when ctx is cancelled. The returned function clears the deadline again.
func (c *Client) bindContext(ctx context.Context) func() {
	if dl, ok := ctx.Deadline(); ok {
		_ = c.conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetDeadline(time.Unix(1, 0)) })
	return func() {
		if stop() {
			_ = c.conn.SetDeadline(time.Time{})
		}
	}
}

func (c *Client) handshake() error {
	var banner [12]byte
	if _, err := io.ReadFull(c.br, banner[:]); err != nil {
		return fmt.Errorf("rfb: read banner: %w", err)
	}
	if !bytes.HasPrefix(banner[:], []byte("RFB ")) || banner[11] != '\n' || banner[7] != '.' {
		return fmt.Errorf("rfb: unexpected banner %q", banner[:])
	}
	c.Version = string(banner[4:11])
	minor := 0
	if _, err := fmt.Sscanf(c.Version, "003.%d", &minor); err != nil {
		return fmt.Errorf("rfb: unparsable version %q", c.Version)
	}
	if minor < 7 {
		return fmt.Errorf("rfb: server version %s is older than 3.7", c.Version)
	}
	if _, err := c.conn.Write([]byte("RFB 003.008\n")); err != nil {
		return fmt.Errorf("rfb: write banner: %w", err)
	}

	n, err := c.br.ReadByte()
	if err != nil {
		return fmt.Errorf("rfb: read security types: %w", err)
	}
	if n == 0 {
		reason, rerr := c.readReason()
		if rerr != nil {
			return rerr
		}
		return fmt.Errorf("rfb: server refused connection: %s", reason)
	}
	types := make([]byte, n)
	if _, err := io.ReadFull(c.br, types); err != nil {
		return fmt.Errorf("rfb: read security types: %w", err)
	}
	if !bytes.Contains(types, []byte{1}) {
		return fmt.Errorf("rfb: server does not offer security type None (offered %v)", types)
	}
	if _, err := c.conn.Write([]byte{1}); err != nil {
		return fmt.Errorf("rfb: select security type: %w", err)
	}
	var res [4]byte
	if _, err := io.ReadFull(c.br, res[:]); err != nil {
		return fmt.Errorf("rfb: read security result: %w", err)
	}
	if binary.BigEndian.Uint32(res[:]) != 0 {
		reason, rerr := c.readReason()
		if rerr != nil {
			return fmt.Errorf("rfb: security failed: %w", rerr)
		}
		return fmt.Errorf("rfb: security failed: %s", reason)
	}

	if _, err := c.conn.Write([]byte{1}); err != nil { // ClientInit: shared
		return fmt.Errorf("rfb: write ClientInit: %w", err)
	}
	var si [24]byte
	if _, err := io.ReadFull(c.br, si[:]); err != nil {
		return fmt.Errorf("rfb: read ServerInit: %w", err)
	}
	c.Width = int(binary.BigEndian.Uint16(si[0:2]))
	c.Height = int(binary.BigEndian.Uint16(si[2:4]))
	if c.Width == 0 || c.Height == 0 || c.Width > maxDimension || c.Height > maxDimension {
		return fmt.Errorf("rfb: implausible framebuffer size %dx%d", c.Width, c.Height)
	}
	nameLen := binary.BigEndian.Uint32(si[20:24])
	if nameLen > maxNameLen {
		return fmt.Errorf("rfb: desktop name of %d bytes is too long", nameLen)
	}
	name := make([]byte, nameLen)
	if _, err := io.ReadFull(c.br, name); err != nil {
		return fmt.Errorf("rfb: read desktop name: %w", err)
	}
	c.Name = string(name)
	c.fb = make([]byte, c.Width*c.Height*4)

	if err := c.setPixelFormat(); err != nil {
		return err
	}
	return c.SetEncodings(encodingRaw, encodingCopyRect)
}

func (c *Client) readReason() (string, error) {
	var l [4]byte
	if _, err := io.ReadFull(c.br, l[:]); err != nil {
		return "", fmt.Errorf("rfb: read reason length: %w", err)
	}
	n := binary.BigEndian.Uint32(l[:])
	if n > maxNameLen {
		return "", fmt.Errorf("rfb: reason of %d bytes is too long", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c.br, b); err != nil {
		return "", fmt.Errorf("rfb: read reason: %w", err)
	}
	return string(b), nil
}

// setPixelFormat selects 32 bpp, depth 24, little-endian true colour with
// red<<16, green<<8, blue (BGRX in memory).
func (c *Client) setPixelFormat() error {
	msg := make([]byte, 20)
	msg[0] = clientSetPixelFormat
	pf := msg[4:]
	pf[0] = 32 // bits per pixel
	pf[1] = 24 // depth
	pf[2] = 0  // little endian
	pf[3] = 1  // true colour
	binary.BigEndian.PutUint16(pf[4:], 255)
	binary.BigEndian.PutUint16(pf[6:], 255)
	binary.BigEndian.PutUint16(pf[8:], 255)
	pf[10], pf[11], pf[12] = 16, 8, 0
	return c.write(msg)
}

func (c *Client) write(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.conn.Write(b); err != nil {
		return fmt.Errorf("rfb: write: %w", err)
	}
	return nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

// SetDeadline sets the I/O deadline of the connection.
func (c *Client) SetDeadline(t time.Time) error { return c.conn.SetDeadline(t) }

// SetEncodings announces the supported encodings in preference order.
func (c *Client) SetEncodings(encs ...int32) error {
	msg := make([]byte, 4+4*len(encs))
	msg[0] = clientSetEncodings
	binary.BigEndian.PutUint16(msg[2:], uint16(len(encs)))
	for i, e := range encs {
		binary.BigEndian.PutUint32(msg[4+4*i:], uint32(e))
	}
	return c.write(msg)
}

// RequestUpdate asks for a FramebufferUpdate of the whole screen.
func (c *Client) RequestUpdate(incremental bool) error {
	msg := make([]byte, 10)
	msg[0] = clientFBUpdateRequest
	if incremental {
		msg[1] = 1
	}
	binary.BigEndian.PutUint16(msg[6:], uint16(c.Width))
	binary.BigEndian.PutUint16(msg[8:], uint16(c.Height))
	return c.write(msg)
}

// KeyEvent sends one key press or release for an X11 keysym.
func (c *Client) KeyEvent(down bool, keysym uint32) error {
	msg := make([]byte, 8)
	msg[0] = clientKeyEvent
	if down {
		msg[1] = 1
	}
	binary.BigEndian.PutUint32(msg[4:], keysym)
	return c.write(msg)
}

// Key presses and releases keysym.
func (c *Client) Key(keysym uint32) error {
	if err := c.KeyEvent(true, keysym); err != nil {
		return err
	}
	return c.KeyEvent(false, keysym)
}

// Chord presses all keysyms in order and releases them in reverse order, for
// example Chord(KeyControlL, KeyV) for Ctrl+V.
func (c *Client) Chord(keysyms ...uint32) error {
	for _, k := range keysyms {
		if err := c.KeyEvent(true, k); err != nil {
			return err
		}
	}
	for i := len(keysyms) - 1; i >= 0; i-- {
		if err := c.KeyEvent(false, keysyms[i]); err != nil {
			return err
		}
	}
	return nil
}

// PointerEvent moves the pointer to (x, y) with the given button mask.
func (c *Client) PointerEvent(buttons uint8, x, y int) error {
	msg := make([]byte, 6)
	msg[0] = clientPointerEvent
	msg[1] = buttons
	binary.BigEndian.PutUint16(msg[2:], uint16(x))
	binary.BigEndian.PutUint16(msg[4:], uint16(y))
	return c.write(msg)
}

// Click moves to (x, y) and performs a left click.
func (c *Client) Click(x, y int) error {
	for _, b := range []uint8{0, ButtonLeft, 0} {
		if err := c.PointerEvent(b, x, y); err != nil {
			return err
		}
	}
	return nil
}

// IsFramebufferUpdate reports whether the message was a FramebufferUpdate;
// Rects and Pixels describe it.
func (m Message) IsFramebufferUpdate() bool { return m.Type == serverFBUpdate }

// ClientCutText sends the bytes verbatim as an RFB ClientCutText message. The
// protocol defines Latin-1; the tests also send UTF-8 bytes on purpose to
// observe what the server does with them.
func (c *Client) ClientCutText(b []byte) error {
	if len(b) > maxCutText {
		return fmt.Errorf("rfb: cut text of %d bytes is too large", len(b))
	}
	msg := make([]byte, 8+len(b))
	msg[0] = clientCutText
	binary.BigEndian.PutUint32(msg[4:], uint32(len(b)))
	copy(msg[8:], b)
	return c.write(msg)
}

// ReadMessage reads one server message and applies framebuffer updates to the
// local copy (unless DiscardPixels is set).
func (c *Client) ReadMessage() (Message, error) {
	t, err := c.br.ReadByte()
	if err != nil {
		return Message{}, fmt.Errorf("rfb: read message type: %w", err)
	}
	m := Message{Type: t}
	switch t {
	case serverFBUpdate:
		return c.readUpdate(m)
	case serverSetColourMap:
		var h [5]byte
		if _, err := io.ReadFull(c.br, h[:]); err != nil {
			return m, fmt.Errorf("rfb: read colour map header: %w", err)
		}
		n := int(binary.BigEndian.Uint16(h[3:5])) * 6
		if _, err := io.CopyN(io.Discard, c.br, int64(n)); err != nil {
			return m, fmt.Errorf("rfb: read colour map: %w", err)
		}
		m.Bytes = n
	case serverBell:
	case serverCutText:
		var h [7]byte
		if _, err := io.ReadFull(c.br, h[:]); err != nil {
			return m, fmt.Errorf("rfb: read cut text header: %w", err)
		}
		n := binary.BigEndian.Uint32(h[3:7])
		if n > maxCutText {
			return m, fmt.Errorf("rfb: server cut text of %d bytes is too large", n)
		}
		m.CutText = make([]byte, n)
		if _, err := io.ReadFull(c.br, m.CutText); err != nil {
			return m, fmt.Errorf("rfb: read cut text: %w", err)
		}
		m.Bytes = int(n)
	default:
		return m, fmt.Errorf("rfb: unknown server message type %d", t)
	}
	return m, nil
}

// rectOp is one decoded rectangle of a FramebufferUpdate, applied together
// with the rest of its update once the whole update has been read.
type rectOp struct {
	x, y, w, h int
	sx, sy     int
	copySrc    bool   // CopyRect from (sx, sy)
	pix        []byte // Raw: w*h*4 bytes
}

// readUpdate reads a whole FramebufferUpdate off the connection, without
// holding the framebuffer lock, then applies all of its rectangles under one
// lock. Image therefore only ever shows the state after a complete update; an
// update that fails midway changes nothing.
func (c *Client) readUpdate(m Message) (Message, error) {
	var h [3]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		return m, fmt.Errorf("rfb: read update header: %w", err)
	}
	m.Rects = int(binary.BigEndian.Uint16(h[1:3]))
	var ops []rectOp
	for range m.Rects {
		var r [12]byte
		if _, err := io.ReadFull(c.br, r[:]); err != nil {
			return m, fmt.Errorf("rfb: read rectangle header: %w", err)
		}
		x := int(binary.BigEndian.Uint16(r[0:2]))
		y := int(binary.BigEndian.Uint16(r[2:4]))
		w := int(binary.BigEndian.Uint16(r[4:6]))
		hh := int(binary.BigEndian.Uint16(r[6:8]))
		enc := int32(binary.BigEndian.Uint32(r[8:12]))
		if x+w > c.Width || y+hh > c.Height {
			return m, fmt.Errorf("rfb: rectangle %d,%d %dx%d outside %dx%d framebuffer", x, y, w, hh, c.Width, c.Height)
		}
		switch enc {
		case encodingRaw:
			if c.DiscardPixels {
				if _, err := io.CopyN(io.Discard, c.br, int64(w)*int64(hh)*4); err != nil {
					return m, fmt.Errorf("rfb: read raw rectangle: %w", err)
				}
			} else {
				pix := make([]byte, w*hh*4)
				if _, err := io.ReadFull(c.br, pix); err != nil {
					return m, fmt.Errorf("rfb: read raw rectangle: %w", err)
				}
				ops = append(ops, rectOp{x: x, y: y, w: w, h: hh, pix: pix})
			}
			m.Bytes += w * hh * 4
		case encodingCopyRect:
			var s [4]byte
			if _, err := io.ReadFull(c.br, s[:]); err != nil {
				return m, fmt.Errorf("rfb: read CopyRect source: %w", err)
			}
			sx := int(binary.BigEndian.Uint16(s[0:2]))
			sy := int(binary.BigEndian.Uint16(s[2:4]))
			if sx+w > c.Width || sy+hh > c.Height {
				return m, fmt.Errorf("rfb: CopyRect source %d,%d %dx%d outside framebuffer", sx, sy, w, hh)
			}
			if !c.DiscardPixels {
				ops = append(ops, rectOp{x: x, y: y, w: w, h: hh, sx: sx, sy: sy, copySrc: true})
			}
			m.Bytes += 4
		default:
			return m, fmt.Errorf("rfb: server used unrequested encoding %d", enc)
		}
		m.Pixels += w * hh
	}
	if len(ops) > 0 {
		c.fbmu.Lock()
		for _, op := range ops {
			if op.copySrc {
				c.copyRectLocked(op.x, op.y, op.sx, op.sy, op.w, op.h)
				continue
			}
			for r := 0; r < op.h; r++ {
				off := ((op.y+r)*c.Width + op.x) * 4
				copy(c.fb[off:off+op.w*4], op.pix[r*op.w*4:(r+1)*op.w*4])
			}
		}
		c.fbmu.Unlock()
	}
	return m, nil
}

// copyRectLocked moves a rectangle inside the framebuffer; fbmu is held.
func (c *Client) copyRectLocked(dx, dy, sx, sy, w, h int) {
	step, start, end := 1, 0, h
	if sy < dy { // overlapping vertical move down: copy bottom-up
		step, start, end = -1, h-1, -1
	}
	for row := start; row != end; row += step {
		src := ((sy+row)*c.Width + sx) * 4
		dst := ((dy+row)*c.Width + dx) * 4
		copy(c.fb[dst:dst+w*4], c.fb[src:src+w*4]) // copy handles horizontal overlap
	}
}

// FetchFramebuffer requests a non-incremental update and reads messages until
// the rectangles of the answer cover the whole screen, then returns an image.
// ServerCutText messages seen on the way are returned as well.
func (c *Client) FetchFramebuffer(ctx context.Context) (*image.RGBA, [][]byte, error) {
	stop := c.bindContext(ctx)
	defer stop()
	if err := c.RequestUpdate(false); err != nil {
		return nil, nil, err
	}
	var cuts [][]byte
	covered := 0
	for covered < c.Width*c.Height {
		m, err := c.ReadMessage()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, cuts, fmt.Errorf("%w: %w", ErrNoUpdate, ctxErr)
			}
			return nil, cuts, err
		}
		switch m.Type {
		case serverFBUpdate:
			covered += m.Pixels
		case serverCutText:
			cuts = append(cuts, m.CutText)
		}
	}
	return c.Image(), cuts, nil
}

// Image converts the local framebuffer copy to an opaque RGBA image.
func (c *Client) Image() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, c.Width, c.Height))
	c.fbmu.Lock()
	defer c.fbmu.Unlock()
	for i := range c.Width * c.Height {
		img.Pix[i*4+0] = c.fb[i*4+2] // R
		img.Pix[i*4+1] = c.fb[i*4+1] // G
		img.Pix[i*4+2] = c.fb[i*4+0] // B
		img.Pix[i*4+3] = 0xff
	}
	return img
}

// EncodePNG encodes img as PNG.
func EncodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("rfb: encode png: %w", err)
	}
	return buf.Bytes(), nil
}

// DistinctColors counts distinct colours in img up to limit, a cheap check that
// a capture is not a blank frame.
func DistinctColors(img image.Image, limit int) int {
	seen := make(map[color.RGBA]struct{})
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y && len(seen) < limit; y++ {
		for x := b.Min.X; x < b.Max.X && len(seen) < limit; x++ {
			r, g, bl, a := img.At(x, y).RGBA()
			seen[color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(bl >> 8), A: uint8(a >> 8)}] = struct{}{}
		}
	}
	return len(seen)
}
