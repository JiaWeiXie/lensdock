//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/tests/e2e/internal/cdpc"
	"github.com/JiaWeiXie/lensdock/tests/e2e/internal/harness"
	"github.com/JiaWeiXie/lensdock/tests/e2e/internal/rfb"
)

type recordedEvent struct {
	Type string  `json:"type"`
	Key  *string `json:"key"`
	X    *int    `json:"x"`
	Y    *int    `json:"y"`
	Data *string `json:"data"`
}

// TestCT04: the view-only VNC port (5901) ignores forged RFB
// KeyEvent, PointerEvent and ClientCutText messages, while the control port (5900)
// delivers the same messages to the page. The positive control proves the
// injection path works, so "zero events" on 5901 is meaningful.
func TestCT04(t *testing.T) {
	s := newSidecar(t)
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	canary := "CANARY-" + hex.EncodeToString(raw[:])

	// Prepare the recorder page and focus its textarea.
	withAgent(t, s.api, func(ctx context.Context, c *cdpc.Client, _ *cdpc.Discovery) {
		if err := c.Navigate(ctx, s.fixture+"/input-recorder.html"); err != nil {
			t.Fatalf("navigate to the recorder: %v", err)
		}
		if err := c.BringToFront(ctx); err != nil {
			t.Fatalf("bring page to front: %v", err)
		}
		var focused bool
		if err := c.Evaluate(ctx, `window.__reset(); window.__focus()`, &focused); err != nil || !focused {
			t.Fatalf("recorder textarea not focused: %v (focused=%v)", err, focused)
		}
	})

	// Negative: everything a hostile viewer could send to the view-only port.
	withHuman(t, s.api, func(ctx context.Context, _ harness.Grant) {
		v := s.dialRFB(t, vncViewPort)
		cx, cy := v.Width/2, v.Height/2
		steps := []func() error{
			func() error { return v.PointerEvent(0, cx, cy) },
			func() error { return v.Click(cx, cy) },
			func() error { return v.PointerEvent(rfb.ButtonLeft, cx+40, cy+40) }, // drag
			func() error { return v.PointerEvent(0, cx+40, cy+40) },
			func() error { return v.PointerEvent(1<<3, cx, cy) }, // wheel up
			func() error { return v.PointerEvent(0, cx, cy) },
			func() error { return v.Key('x') },
			func() error { return v.Key('y') },
			func() error { return v.Key('z') },
			func() error { return v.Key(rfb.KeyReturn) },
			func() error { return v.ClientCutText([]byte(canary)) },
			func() error { return v.Chord(rfb.KeyControlL, rfb.KeyV) },
		}
		for i, step := range steps {
			if err := step(); err != nil {
				t.Fatalf("send forged input %d on the view-only port: %v", i, err)
			}
		}
		cuts := drain(v, 2*time.Second)
		for _, c := range cuts {
			if strings.Contains(string(c), canary) {
				t.Errorf("view-only port reflected the canary as ServerCutText")
			}
		}
		settle()
	})

	events, value := readRecorder(t, s)
	s.ev.SaveJSON(t, "TestCT04/negative-5901.json", map[string]any{"events": events, "textareaValue": value})
	if len(events) != 0 {
		t.Errorf("view-only port 5901 delivered %d events to the page (first: %s)", len(events), describe(events[0]))
	}
	if value != "" {
		t.Errorf("view-only port 5901 changed the textarea to %q", value)
	}

	// Positive control on 5900; Ctrl+V also shows that the canary never reached the clipboard.
	withHuman(t, s.api, func(ctx context.Context, _ harness.Grant) {
		v := s.dialRFB(t, vncControlPort)
		cx, cy := v.Width/2, v.Height/2
		for _, step := range []func() error{
			func() error { return v.Click(cx, cy) },
			func() error { return v.Key('a') },
			func() error { return v.Key('b') },
			func() error { return v.Chord(rfb.KeyControlL, rfb.KeyV) },
		} {
			if err := step(); err != nil {
				t.Fatalf("send input on the control port: %v", err)
			}
		}
		drain(v, 1*time.Second)
		settle()
	})

	events, value = readRecorder(t, s)
	s.ev.SaveJSON(t, "TestCT04/positive-5900.json", map[string]any{"events": events, "textareaValue": value})
	if !hasEvent(events, "pointerdown", "") {
		t.Error("positive control: no pointerdown reached the page through port 5900")
	}
	for _, k := range []string{"a", "b"} {
		if !hasEvent(events, "keydown", k) {
			t.Errorf("positive control: no keydown %q reached the page through port 5900", k)
		}
	}
	if !strings.HasPrefix(value, "ab") {
		t.Errorf("positive control: textarea = %q, want it to start with \"ab\"", value)
	}
	if strings.Contains(value, canary) {
		t.Error("the ClientCutText canary sent to the view-only port was pasted from the clipboard")
	}
	for _, e := range events {
		if e.Data != nil && strings.Contains(*e.Data, canary) {
			t.Error("a recorded event carries the canary")
		}
	}
}

// readRecorder returns the recorded page events and the textarea value under an AGENT lease.
func readRecorder(t *testing.T, s *sidecar) (events []recordedEvent, value string) {
	t.Helper()
	withAgent(t, s.api, func(ctx context.Context, c *cdpc.Client, _ *cdpc.Discovery) {
		var js string
		if err := c.Evaluate(ctx, `JSON.stringify(window.__events)`, &js); err != nil {
			t.Fatalf("read recorded events: %v", err)
		}
		if err := json.Unmarshal([]byte(js), &events); err != nil {
			t.Fatalf("decode recorded events: %v", err)
		}
		if err := c.Evaluate(ctx, `document.getElementById("sink").value`, &value); err != nil {
			t.Fatalf("read textarea: %v", err)
		}
	})
	return events, value
}

func hasEvent(events []recordedEvent, typ, key string) bool {
	for _, e := range events {
		if e.Type != typ {
			continue
		}
		if key == "" || (e.Key != nil && *e.Key == key) {
			return true
		}
	}
	return false
}

func describe(e recordedEvent) string {
	b, _ := json.Marshal(e)
	return string(b)
}
