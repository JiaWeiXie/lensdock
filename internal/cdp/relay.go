package cdp

import (
	"context"
	"errors"
	"io"

	"github.com/coder/websocket"
)

// errReader remembers the first non-EOF read error so a failed copy can be
// attributed to the source rather than the destination.
type errReader struct {
	r   io.Reader
	err error
}

func (e *errReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		e.err = err
	}
	return n, err
}

// pump streams whole messages from src to dst, preserving message types,
// until one side fails. It returns the source's error, or nil when the
// destination failed: only source failures carry a close status worth
// relaying. A message interrupted by a failure is abandoned, never
// completed: the destination writer is left unclosed so a truncated message
// cannot be delivered as if it were whole; the caller closes the connections.
func pump(ctx context.Context, src, dst *websocket.Conn) error {
	for {
		typ, r, err := src.Reader(ctx)
		if err != nil {
			return err
		}
		w, err := dst.Writer(ctx, typ)
		if err != nil {
			return nil
		}
		er := &errReader{r: r}
		if _, err := io.Copy(w, er); err != nil {
			return er.err
		}
		if err := w.Close(); err != nil {
			return nil
		}
	}
}
