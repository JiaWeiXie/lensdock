//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/JiaWeiXie/lensdock/tests/e2e/internal/cdpc"
	"github.com/JiaWeiXie/lensdock/tests/e2e/internal/harness"
	"github.com/JiaWeiXie/lensdock/tests/e2e/internal/rfb"
)

const cjkFixture = "/cjk-emoji.html"

// openCJKPage loads the CJK/emoji fixture in the visible tab.
func (s *sidecar) openCJKPage(t *testing.T) {
	t.Helper()
	withAgent(t, s.api, func(ctx context.Context, c *cdpc.Client, _ *cdpc.Discovery) {
		if err := c.Navigate(ctx, s.fixture+cjkFixture); err != nil {
			t.Fatalf("navigate to the CJK fixture: %v", err)
		}
		if err := c.BringToFront(ctx); err != nil {
			t.Fatalf("bring page to front: %v", err)
		}
	})
}

// clearInput empties the fixture textarea and focuses it.
func (s *sidecar) clearInput(t *testing.T) {
	t.Helper()
	withAgent(t, s.api, func(ctx context.Context, c *cdpc.Client, _ *cdpc.Discovery) {
		var focused bool
		js := `(() => { const i = document.getElementById("input"); i.value = ""; window.focus(); i.focus(); return document.activeElement === i; })()`
		if err := c.Evaluate(ctx, js, &focused); err != nil || !focused {
			t.Fatalf("could not focus the fixture textarea: %v (focused=%v)", err, focused)
		}
	})
}

// readInput returns the fixture textarea value exactly as the page sees it.
func (s *sidecar) readInput(t *testing.T) string {
	t.Helper()
	var v string
	withAgent(t, s.api, func(ctx context.Context, c *cdpc.Client, _ *cdpc.Discovery) {
		if err := c.Evaluate(ctx, `document.getElementById("input").value`, &v); err != nil {
			t.Fatalf("read the fixture textarea: %v", err)
		}
	})
	return v
}

// vncPaste connects to port, optionally runs before (for example ClientCutText),
// then presses Ctrl+V and waits for Chromium to process it.
func (s *sidecar) vncPaste(t *testing.T, port int, before func(v *rfb.Client) error) {
	t.Helper()
	v := s.dialRFB(t, port)
	defer v.Close()
	if before != nil {
		if err := before(v); err != nil {
			t.Fatalf("send before pasting: %v", err)
		}
		time.Sleep(700 * time.Millisecond) // let x11vnc take ownership of the selection
	}
	if err := v.Chord(rfb.KeyControlL, rfb.KeyV); err != nil {
		t.Fatalf("send Ctrl+V: %v", err)
	}
	drain(v, time.Second)
	settle()
}

// vncPasteRightAfter connects to port first, then runs put and presses Ctrl+V
// with no delay in between. A PUT /clipboard that returned before the new
// value was served would paste the previous case's text or nothing.
func (s *sidecar) vncPasteRightAfter(t *testing.T, port int, put func()) {
	t.Helper()
	v := s.dialRFB(t, port)
	defer v.Close()
	put()
	if err := v.Chord(rfb.KeyControlL, rfb.KeyV); err != nil {
		t.Fatalf("send Ctrl+V: %v", err)
	}
	drain(v, time.Second)
	settle()
}

// TestTX01: CJK text renders with the language-matched Noto
// Sans CJK face, checked through CSS.getPlatformFontsForNode. Browser and VNC
// captures are stored as evidence for the visual no-tofu review.
func TestTX01(t *testing.T) {
	s := newSidecar(t)
	langs := []struct{ label, selector, family, postScript string }{
		{"zh-Hant-TW", `td[lang="zh-Hant-TW"]`, "Noto Sans CJK TC", "NotoSansCJKtc"},
		{"zh-Hans-CN", `td[lang="zh-Hans-CN"]`, "Noto Sans CJK SC", "NotoSansCJKsc"},
		{"ja", `td[lang="ja"]`, "Noto Sans CJK JP", "NotoSansCJKjp"},
		{"ko", `td[lang="ko"]`, "Noto Sans CJK KR", "NotoSansCJKkr"},
	}
	type row struct {
		Lang     string              `json:"lang"`
		Want     string              `json:"want"`
		Dominant string              `json:"dominantFamily"`
		Fonts    []cdpc.PlatformFont `json:"fonts"`
	}
	var rows []row
	var browserPNG []byte

	withAgent(t, s.api, func(ctx context.Context, c *cdpc.Client, _ *cdpc.Discovery) {
		if err := c.Navigate(ctx, s.fixture+cjkFixture); err != nil {
			t.Fatalf("navigate: %v", err)
		}
		if err := c.BringToFront(ctx); err != nil {
			t.Fatalf("bring to front: %v", err)
		}
		if err := c.EnableCSS(ctx); err != nil {
			t.Fatalf("enable DOM/CSS: %v", err)
		}
		for _, l := range langs {
			ids, err := c.QuerySelectorAll(ctx, l.selector)
			if err != nil || len(ids) != 1 {
				t.Fatalf("%s: %d nodes for %s, %v", l.label, len(ids), l.selector, err)
			}
			fonts, err := c.PlatformFonts(ctx, ids[0])
			if err != nil {
				t.Fatalf("%s: platform fonts: %v", l.label, err)
			}
			dom, ok := cdpc.DominantFont(fonts)
			rows = append(rows, row{Lang: l.label, Want: l.family, Dominant: dom.FamilyName, Fonts: fonts})
			matches := ok && (strings.EqualFold(dom.FamilyName, l.family) ||
				strings.HasPrefix(strings.ToLower(dom.PostScriptName), strings.ToLower(l.postScript)))
			if !matches {
				t.Errorf("%s: dominant font = %+v, want %s (all fonts: %+v)", l.label, dom, l.family, fonts)
			}
		}
		var err error
		if browserPNG, err = c.Screenshot(ctx); err != nil {
			t.Fatalf("Page.captureScreenshot: %v", err)
		}
	})
	settle()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	v := s.dialRFB(t, vncViewPort)
	img, _, err := v.FetchFramebuffer(ctx)
	if err != nil {
		t.Fatalf("RFB framebuffer from port %d: %v", vncViewPort, err)
	}
	vncPNG, err := rfb.EncodePNG(img)
	if err != nil {
		t.Fatal(err)
	}

	browserSum := s.ev.Save(t, "TestTX01/browser-screenshot.png", browserPNG)
	vncSum := s.ev.Save(t, "TestTX01/vnc-5901-framebuffer.png", vncPNG)
	s.ev.SaveJSON(t, "TestTX01/fonts.json", map[string]any{
		"rows": rows, "browserScreenshotSha256": browserSum, "vncFramebufferSha256": vncSum,
		"vncSize": fmt.Sprintf("%dx%d", v.Width, v.Height),
	})

	if cfg, err := png.DecodeConfig(bytes.NewReader(browserPNG)); err != nil || cfg.Width == 0 {
		t.Errorf("browser screenshot is not a PNG: %v", err)
	}
	if n := rfb.DistinctColors(img, 64); n < 8 {
		t.Errorf("VNC framebuffer has only %d distinct colours; it looks blank", n)
	}
}

// emojiSequences mirrors the fixture's emoji row; a drifting fixture fails the test.
var emojiSequences = []struct{ name, codepoints string }{
	{"grinning face", "1F600"},
	{"thumbs up, skin tone 4", "1F44D 1F3FD"},
	{"woman technologist (ZWJ)", "1F469 200D 1F4BB"},
	{"family (ZWJ x3)", "1F468 200D 1F469 200D 1F467 200D 1F466"},
	{"flag Taiwan (regional indicators)", "1F1F9 1F1FC"},
	{"red heart + VS16", "2764 FE0F"},
	{"rainbow flag (VS16 + ZWJ)", "1F3F3 FE0F 200D 1F308"},
}

func codepointsOf(s string) string {
	var parts []string
	for _, r := range s {
		parts = append(parts, fmt.Sprintf("%X", r))
	}
	return strings.Join(parts, " ")
}

// TestTX02: every emoji sequence of the fixture is shaped into a
// single Noto Color Emoji glyph (skin tone, flag, ZWJ and variation selector).
func TestTX02(t *testing.T) {
	s := newSidecar(t)
	type row struct {
		Name       string              `json:"name"`
		Codepoints string              `json:"codepoints"`
		Fonts      []cdpc.PlatformFont `json:"fonts"`
	}
	var rows []row
	var shot []byte

	withAgent(t, s.api, func(ctx context.Context, c *cdpc.Client, _ *cdpc.Discovery) {
		if err := c.Navigate(ctx, s.fixture+cjkFixture); err != nil {
			t.Fatalf("navigate: %v", err)
		}
		if err := c.BringToFront(ctx); err != nil {
			t.Fatalf("bring to front: %v", err)
		}
		// Split the fixture's emoji row into one span per space-separated sequence.
		split := `(() => {
			const cell = document.querySelector("td.emoji");
			const parts = cell.textContent.trim().split(" ");
			cell.textContent = "";
			for (const p of parts) {
				const s = document.createElement("span");
				s.className = "seq"; s.textContent = p; s.style.marginRight = "16px";
				cell.appendChild(s);
			}
			// Force style/layout now: CSS.getPlatformFontsForNode reads the
			// existing layout objects and returns [] for nodes not laid out yet.
			void cell.getBoundingClientRect();
			return parts.length;
		})()`
		var n int
		if err := c.Evaluate(ctx, split, &n); err != nil || n != len(emojiSequences) {
			t.Fatalf("emoji row split into %d sequences (%v), want %d", n, err, len(emojiSequences))
		}
		if err := c.EnableCSS(ctx); err != nil {
			t.Fatalf("enable DOM/CSS: %v", err)
		}
		ids, err := c.QuerySelectorAll(ctx, "td.emoji span.seq")
		if err != nil || len(ids) != len(emojiSequences) {
			t.Fatalf("%d span nodes, %v; want %d", len(ids), err, len(emojiSequences))
		}
		for i, want := range emojiSequences {
			var text string
			if err := c.Evaluate(ctx, fmt.Sprintf(`document.querySelectorAll("td.emoji span.seq")[%d].textContent`, i), &text); err != nil {
				t.Fatalf("read sequence %d: %v", i, err)
			}
			got := codepointsOf(text)
			fonts, err := c.PlatformFonts(ctx, ids[i])
			if err != nil {
				t.Fatalf("%s: platform fonts: %v", want.name, err)
			}
			rows = append(rows, row{Name: want.name, Codepoints: got, Fonts: fonts})
			if got != want.codepoints {
				t.Errorf("%s: fixture has code points %s, want %s", want.name, got, want.codepoints)
			}
			if len(fonts) != 1 || fonts[0].FamilyName != "Noto Color Emoji" || fonts[0].GlyphCount != 1 {
				t.Errorf("%s: fonts = %+v, want exactly one Noto Color Emoji glyph", want.name, fonts)
			}
		}
		if shot, err = c.Screenshot(ctx); err != nil {
			t.Fatalf("Page.captureScreenshot: %v", err)
		}
	})
	s.ev.SaveJSON(t, "TestTX02/emoji-fonts.json", rows)
	s.ev.Save(t, "TestTX02/emoji-screenshot.png", shot)
}

// TestTX03: text written by a HUMAN lease through
// PUT /clipboard and pasted with Ctrl+V over the control VNC arrives in the page
// byte for byte, for astral code points, NFD, ZWJ sequences and newlines. After the
// HUMAN lease ends the selection is empty (run-sidecar.sh additionally checks the
// X selection with xclip inside the container).
func TestTX03(t *testing.T) {
	s := newSidecar(t)
	s.openCJKPage(t)

	var expected string
	withAgent(t, s.api, func(ctx context.Context, c *cdpc.Client, _ *cdpc.Discovery) {
		if err := c.Evaluate(ctx, `document.getElementById("expected").textContent`, &expected); err != nil || expected == "" {
			t.Fatalf("read the fixture's expected text: %v", err)
		}
	})
	cases := []struct{ name, text string }{
		{"fixture-expected-line", expected},
		{"astral-planes", "😀𝒜𠀀🎉 U+1F600 U+1D49C U+20000"},
		{"nfd-and-nfc", "e\u0301 vs \u00e9, a\u0308, \u1112\u1161\u11ab 한"},
		{"zwj-flags-skin-tone", "👩‍💻 👨‍👩‍👧‍👦 🏳️‍🌈 🇹🇼 ❤️ 👍🏽"},
		{"newline", "line one\nline two\n繁體中文"},
	}
	type result struct {
		Name    string `json:"name"`
		WantHex string `json:"wantHex"`
		GotHex  string `json:"gotHex"`
		Exact   bool   `json:"exact"`
	}
	var results []result

	for _, tc := range cases {
		s.clearInput(t)
		withHuman(t, s.api, func(ctx context.Context, g harness.Grant) {
			s.vncPasteRightAfter(t, vncControlPort, func() {
				if err := s.api.PutClipboard(ctx, g.Token, []byte(tc.text)); err != nil {
					t.Fatalf("%s: PUT /clipboard: %v", tc.name, err)
				}
			})
		})
		got := s.readInput(t)
		results = append(results, result{tc.name, hexOf([]byte(tc.text)), hexOf([]byte(got)), got == tc.text})
		if got != tc.text {
			t.Errorf("%s: pasted text differs\n got % x\nwant % x", tc.name, []byte(got), []byte(tc.text))
		}
	}

	// Repeat the fixture line once more and let the page's own "Verify exact text" button judge it.
	s.clearInput(t)
	withHuman(t, s.api, func(ctx context.Context, g harness.Grant) {
		s.vncPasteRightAfter(t, vncControlPort, func() {
			if err := s.api.PutClipboard(ctx, g.Token, []byte(expected)); err != nil {
				t.Fatalf("PUT /clipboard: %v", err)
			}
		})
	})
	withAgent(t, s.api, func(ctx context.Context, c *cdpc.Client, _ *cdpc.Discovery) {
		var verdict string
		if err := c.Evaluate(ctx, `document.getElementById("verify").click(); document.getElementById("result").textContent`, &verdict); err != nil {
			t.Fatalf("run the page verifier: %v", err)
		}
		if verdict != "PASS: exact text match" {
			t.Errorf("fixture verifier says %q", verdict)
		}
	})

	// HUMAN lease is over: the clipboard must be empty again.
	time.Sleep(time.Second)
	s.clearInput(t)
	s.vncPaste(t, vncControlPort, nil)
	if after := s.readInput(t); after != "" {
		t.Errorf("after the HUMAN lease ended Ctrl+V still pasted %d bytes", len(after))
	}
	s.ev.SaveJSON(t, "TestTX03/results.json", results)
}

// TestSpike03CutText records what RFB ClientCutText delivers to
// Chromium via x11vnc on the control port. Nothing is asserted: this is the
// compatibility probe's evidence. Observed with x11vnc 0.9.17: the text lands in CUT_BUFFER0
// but neither PRIMARY nor CLIPBOARD yields it to xclip, so Ctrl+V in Chromium
// pastes nothing, and the protocol is Latin-1 only anyway. Both justify
// PUT /clipboard (xclip owns the selection) as the only text path.
func TestSpike03CutText(t *testing.T) {
	s := newSidecar(t)
	s.openCJKPage(t)
	cases := []struct {
		name    string
		payload []byte
	}{
		{"ascii", []byte("LensDock cut text 0123 !@#")},
		{"cjk-utf8-bytes", []byte("繁體中文 日本語 한국어")},
		{"latin1-bytes", []byte{'c', 'a', 'f', 0xe9}},
	}
	type result struct {
		Name       string `json:"name"`
		SentHex    string `json:"sentHex"`
		ObservedHx string `json:"observedHex"`
		Observed   string `json:"observedText"`
		ValidUTF8  bool   `json:"observedValidUTF8"`
		Exact      bool   `json:"exact"`
	}
	var results []result
	for _, tc := range cases {
		s.clearInput(t)
		withHuman(t, s.api, func(ctx context.Context, _ harness.Grant) {
			s.vncPaste(t, vncControlPort, func(v *rfb.Client) error { return v.ClientCutText(tc.payload) })
		})
		got := s.readInput(t)
		results = append(results, result{tc.name, hexOf(tc.payload), hexOf([]byte(got)), got, utf8.ValidString(got), got == string(tc.payload)})
		t.Logf("%s: sent %x, page saw %x", tc.name, tc.payload, []byte(got))
	}
	s.ev.SaveJSON(t, "TestSpike03CutText/results.json", results)
}
