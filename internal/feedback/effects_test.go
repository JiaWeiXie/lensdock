package feedback

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math/rand/v2"
	"strings"
	"testing"
)

func TestParseAnnotationsAdvancedKindsAndStylesRoundTrip(t *testing.T) {
	want := []string{
		`{"id":"h","kind":"highlight","geometry":{"x":0.1,"y":0.1,"width":0.5,"height":0.2},"style":{"stroke":"#f2a900","opacity":0.35}}`,
		`{"id":"s","kind":"spotlight","geometry":{"x":0,"y":0,"width":1,"height":1}}`,
		`{"id":"b","kind":"blur","geometry":{"x":0.1,"y":0.1,"width":0.5,"height":0.2},"style":{"effectSizePx":12}}`,
		`{"id":"p","kind":"pixelate","geometry":{"x":0.1,"y":0.1,"width":0.5,"height":0.2},"comment":"c","style":{"effectSizePx":64}}`,
		`{"id":"a","kind":"arrow","geometry":{"start":{"x":0,"y":0},"end":{"x":1,"y":1},"control":{"x":0.5,"y":0}},"style":{"stroke":"#e5362b","arrowStyle":"curved"}}`,
		`{"id":"a2","kind":"arrow","geometry":{"start":{"x":0,"y":0},"end":{"x":1,"y":1}},"style":{"arrowStyle":"double"}}`,
		`{"id":"t","kind":"text","geometry":{"position":{"x":0.5,"y":0.5},"text":"hi"},"style":{"fontSizePx":20,"textStyle":"mono"}}`,
		`{"id":"f","kind":"freehand","geometry":{"points":[{"x":0,"y":0},{"x":1,"y":1}]},"style":{"smoothing":false}}`,
		`{"id":"f2","kind":"freehand","geometry":{"points":[{"x":0,"y":0},{"x":1,"y":1}]},"style":{"smoothing":true}}`,
	}
	canon, reds, err := ParseAnnotations(raws(t, want...))
	if err != nil {
		t.Fatal(err)
	}
	if len(reds) != 0 {
		t.Fatalf("highlight, spotlight, blur and pixelate must never become redactions, got %d", len(reds))
	}
	for i := range want {
		if got := strings.TrimSpace(string(canon[i])); got != want[i] {
			t.Errorf("canonical %d = %s, want %s", i, got, want[i])
		}
	}
	again, _, err := ParseAnnotations(canon)
	if err != nil {
		t.Fatal(err)
	}
	for i := range canon {
		if string(canon[i]) != string(again[i]) {
			t.Errorf("canonical form unstable: %s vs %s", canon[i], again[i])
		}
	}
}

func TestParseAnnotationsEffectsKeepOrderAndDefaults(t *testing.T) {
	set, err := parseAnnotationSet(raws(t,
		`{"id":"p","kind":"pixelate","geometry":{"x":0,"y":0,"width":0.5,"height":0.5}}`,
		`{"id":"r","kind":"redact","mode":"solid","geometry":{"x":0,"y":0,"width":0.1,"height":0.1}}`,
		`{"id":"b","kind":"blur","geometry":{"x":0.5,"y":0.5,"width":0.5,"height":0.5}}`,
		`{"id":"b2","kind":"blur","geometry":{"x":0,"y":0.5,"width":0.25,"height":0.5},"style":{"effectSizePx":8}}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Redactions) != 1 {
		t.Fatalf("redactions = %d, want 1", len(set.Redactions))
	}
	got := make([]string, len(set.Effects))
	for i, e := range set.Effects {
		got[i] = fmt.Sprintf("%s=%d", e.Kind, e.SizePx)
	}
	if strings.Join(got, ",") != "pixelate=16,blur=12,blur=8" {
		t.Fatalf("effects = %v", got)
	}
}

func TestParseAnnotationsRejectsMalformedAdvancedFields(t *testing.T) {
	rect := `"geometry":{"x":0.1,"y":0.1,"width":0.3,"height":0.2}`
	seg := `{"start":{"x":0,"y":0},"end":{"x":1,"y":1}`
	cases := map[string]string{
		"opacity below range":      `{"id":"a","kind":"highlight",` + rect + `,"style":{"opacity":0.09}}`,
		"opacity above range":      `{"id":"a","kind":"highlight",` + rect + `,"style":{"opacity":0.81}}`,
		"opacity on rectangle":     `{"id":"a","kind":"rectangle",` + rect + `,"style":{"opacity":0.5}}`,
		"opacity on spotlight":     `{"id":"a","kind":"spotlight",` + rect + `,"style":{"opacity":0.5}}`,
		"opacity not a number":     `{"id":"a","kind":"highlight",` + rect + `,"style":{"opacity":"high"}}`,
		"effect size 1":            `{"id":"a","kind":"blur",` + rect + `,"style":{"effectSizePx":1}}`,
		"effect size 65":           `{"id":"a","kind":"blur",` + rect + `,"style":{"effectSizePx":65}}`,
		"effect size fractional":   `{"id":"a","kind":"pixelate",` + rect + `,"style":{"effectSizePx":8.5}}`,
		"effect size on highlight": `{"id":"a","kind":"highlight",` + rect + `,"style":{"effectSizePx":8}}`,
		"effect size on redact":    `{"id":"a","kind":"redact","mode":"solid",` + rect + `,"style":{"effectSizePx":8}}`,
		"arrow style unknown":      `{"id":"a","kind":"arrow","geometry":` + seg + `},"style":{"arrowStyle":"wavy"}}`,
		"arrow style on line":      `{"id":"a","kind":"line","geometry":` + seg + `},"style":{"arrowStyle":"open"}}`,
		"text style unknown":       `{"id":"a","kind":"text","geometry":{"position":{"x":0,"y":0},"text":"x"},"style":{"textStyle":"neon"}}`,
		"text style on arrow":      `{"id":"a","kind":"arrow","geometry":` + seg + `},"style":{"textStyle":"bold"}}`,
		"smoothing on text":        `{"id":"a","kind":"text","geometry":{"position":{"x":0,"y":0},"text":"x"},"style":{"smoothing":true}}`,
		"smoothing not a boolean":  `{"id":"a","kind":"freehand","geometry":{"points":[{"x":0,"y":0},{"x":1,"y":1}]},"style":{"smoothing":"yes"}}`,
		"control on line":          `{"id":"a","kind":"line","geometry":` + seg + `,"control":{"x":0.5,"y":0.5}}}`,
		"control above range":      `{"id":"a","kind":"arrow","geometry":` + seg + `,"control":{"x":1.1,"y":0.5}}}`,
		"control missing y":        `{"id":"a","kind":"arrow","geometry":` + seg + `,"control":{"x":0.5}}}`,
		"control on freehand":      `{"id":"a","kind":"freehand","geometry":{"points":[{"x":0,"y":0},{"x":1,"y":1}],"control":{"x":0,"y":0}}}`,
		"blur with mode":           `{"id":"a","kind":"blur","mode":"solid",` + rect + `}`,
		"highlight with mode":      `{"id":"a","kind":"highlight","mode":"solid",` + rect + `}`,
		"blur zero width":          `{"id":"a","kind":"blur","geometry":{"x":0,"y":0,"width":0,"height":0.1}}`,
		"pixelate beyond edge":     `{"id":"a","kind":"pixelate","geometry":{"x":0.9,"y":0,"width":0.2,"height":0.1}}`,
		"spotlight arrow geometry": `{"id":"a","kind":"spotlight","geometry":` + seg + `}}`,
		"unknown style field":      `{"id":"a","kind":"highlight",` + rect + `,"style":{"opacityy":0.5}}`,
	}
	for name, raw := range cases {
		if _, _, err := ParseAnnotations(raws(t, raw)); err == nil {
			t.Errorf("%s was accepted: %s", name, raw)
		}
	}
}

func TestDocumentSchemaVersionSelectsAdvancedOnlyWhenNeeded(t *testing.T) {
	task := Task{Kind: "bug", Intent: "analyze-only", Title: "t", Description: "d", ExpectedResult: "e", ActualResult: "a"}
	prose := Task{Kind: "question", Intent: "analyze-only", Title: "t"}
	rect := `"geometry":{"x":0.1,"y":0.1,"width":0.3,"height":0.2}`
	seg := `"geometry":{"start":{"x":0,"y":0},"end":{"x":1,"y":1}}`
	cases := []struct {
		name   string
		source string
		task   Task
		raw    []string
		want   string
	}{
		{"legacy kinds and styles", SourceDesktop, task, []string{
			`{"id":"a","kind":"arrow",` + seg + `,"style":{"stroke":"#ff0000","strokeWidthPx":3}}`,
			`{"id":"t","kind":"text","geometry":{"position":{"x":0,"y":0},"text":"x"},"style":{"fontSizePx":20}}`,
		}, SchemaVersionLegacy},
		{"line stays 0.2.0", SourceDesktop, task, []string{`{"id":"l","kind":"line",` + seg + `}`}, SchemaVersion},
		{"highlight", SourceDesktop, task, []string{`{"id":"h","kind":"highlight",` + rect + `}`}, SchemaVersionAdvanced},
		{"spotlight", SourceViewport, task, []string{`{"id":"h","kind":"spotlight",` + rect + `}`}, SchemaVersionAdvanced},
		{"blur", SourceDesktop, task, []string{`{"id":"h","kind":"blur",` + rect + `}`}, SchemaVersionAdvanced},
		{"pixelate", SourceDesktop, task, []string{`{"id":"h","kind":"pixelate",` + rect + `}`}, SchemaVersionAdvanced},
		{"control point", SourceDesktop, task, []string{`{"id":"a","kind":"arrow","geometry":{"start":{"x":0,"y":0},"end":{"x":1,"y":1},"control":{"x":0.5,"y":0}}}`}, SchemaVersionAdvanced},
		{"arrow style", SourceDesktop, task, []string{`{"id":"a","kind":"arrow",` + seg + `,"style":{"arrowStyle":"filled"}}`}, SchemaVersionAdvanced},
		{"text style", SourceDesktop, task, []string{`{"id":"t","kind":"text","geometry":{"position":{"x":0,"y":0},"text":"x"},"style":{"textStyle":"outline"}}`}, SchemaVersionAdvanced},
		{"smoothing false", SourceDesktop, task, []string{`{"id":"f","kind":"freehand","geometry":{"points":[{"x":0,"y":0},{"x":1,"y":1}]},"style":{"smoothing":false}}`}, SchemaVersionAdvanced},
		{"drawing with blur", SourceDrawing, task, []string{`{"id":"h","kind":"blur",` + rect + `}`}, SchemaVersionAdvanced},
		{"prose-free with highlight", SourceDesktop, prose, []string{`{"id":"h","kind":"highlight",` + rect + `}`}, SchemaVersionAdvanced},
		{"drawing without advanced", SourceDrawing, task, nil, SchemaVersion},
	}
	for _, c := range cases {
		canon, _, err := ParseAnnotations(raws(t, c.raw...))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := documentSchemaVersion(c.source, canon, c.task); got != c.want {
			t.Errorf("%s: version = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestEffectPixelRectFloorsOriginAndCeilsFarEdge(t *testing.T) {
	got := effectPixelRect(10, 10, normRect{X: 0.15, Y: 0.25, W: 0.5, H: 0.5})
	if got != image.Rect(1, 2, 7, 8) {
		t.Fatalf("rect = %v", got)
	}
	if got := effectPixelRect(10, 10, normRect{X: 0, Y: 0, W: 1, H: 1}); got != image.Rect(0, 0, 10, 10) {
		t.Fatalf("full rect = %v", got)
	}
	// Float noise beyond the far edge is clamped to the image.
	if got := effectPixelRect(10, 10, normRect{X: 0.5, Y: 0.5, W: 0.5000000005, H: 0.5}); got != image.Rect(5, 5, 10, 10) {
		t.Fatalf("clamped rect = %v", got)
	}
}

func rowImage(values ...uint8) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, len(values), 1))
	for i, v := range values {
		img.SetRGBA(i, 0, color.RGBA{R: v, G: v, B: v, A: 255})
	}
	return img
}

func redChannel(img *image.RGBA) []uint8 {
	out := make([]uint8, img.Bounds().Dx())
	for i := range out {
		out[i] = img.RGBAAt(i, 0).R
	}
	return out
}

func TestBlurAndPixelateGoldenValues(t *testing.T) {
	// Radius 1 box blur with edge clamping; a single row makes the vertical pass the identity.
	img := rowImage(0, 0, 255, 0, 0)
	if err := applyEffects(img, []effectOp{{Kind: "blur", Rect: normRect{X: 0, Y: 0, W: 1, H: 1}, SizePx: 1}}, 0); err != nil {
		t.Fatal(err)
	}
	if got := redChannel(img); !bytes.Equal(got, []uint8{0, 85, 85, 85, 0}) {
		t.Fatalf("blur = %v", got)
	}
	// Blocks of 2 anchored at the left edge; the cut last block averages itself; halves round up.
	img = rowImage(10, 11, 0)
	if err := applyEffects(img, []effectOp{{Kind: "pixelate", Rect: normRect{X: 0, Y: 0, W: 1, H: 1}, SizePx: 2}}, 0); err != nil {
		t.Fatal(err)
	}
	if got := redChannel(img); !bytes.Equal(got, []uint8{11, 11, 0}) {
		t.Fatalf("pixelate = %v", got)
	}
	// A blur never reads or writes outside its rectangle.
	img = rowImage(255, 255, 0, 0, 0, 0, 255, 255)
	if err := applyEffects(img, []effectOp{{Kind: "blur", Rect: normRect{X: 0.25, Y: 0, W: 0.5, H: 1}, SizePx: 2}}, 0); err != nil {
		t.Fatal(err)
	}
	if got := redChannel(img); !bytes.Equal(got, []uint8{255, 255, 0, 0, 0, 0, 255, 255}) {
		t.Fatalf("blur leaked across its rectangle: %v", got)
	}
}

// naiveBlur is the specification written without running sums.
func naiveBlur(img *image.RGBA, r image.Rectangle, radius int) {
	pass := func(horizontal bool) {
		n, lines := r.Dx(), r.Dy()
		if !horizontal {
			n, lines = r.Dy(), r.Dx()
		}
		at := func(line, i int) int {
			if horizontal {
				return (r.Min.Y+line)*img.Stride + (r.Min.X+i)*4
			}
			return (r.Min.Y+i)*img.Stride + (r.Min.X+line)*4
		}
		for line := range lines {
			for c := range 3 {
				src := make([]int, n)
				for i := range n {
					src[i] = int(img.Pix[at(line, i)+c])
				}
				for i := range n {
					sum := 0
					for k := i - radius; k <= i+radius; k++ {
						sum += src[min(max(k, 0), n-1)]
					}
					w := 2*radius + 1
					img.Pix[at(line, i)+c] = uint8((2*sum + w) / (2 * w))
				}
			}
		}
	}
	pass(true)
	pass(false)
}

func noisyImage(rng *rand.Rand, w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = uint8(rng.IntN(256)), uint8(rng.IntN(256)), uint8(rng.IntN(256)), 255
	}
	return img
}

func TestRunningSumBlurMatchesTheNaiveDefinition(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	for range 40 {
		w, h := 1+rng.IntN(40), 1+rng.IntN(30)
		img := noisyImage(rng, w, h)
		ref := image.NewRGBA(img.Bounds())
		copy(ref.Pix, img.Pix)
		x0, y0 := rng.IntN(w), rng.IntN(h)
		r := image.Rect(x0, y0, x0+1+rng.IntN(w-x0), y0+1+rng.IntN(h-y0))
		radius := 2 + rng.IntN(63)
		blurRect(img, r, radius)
		naiveBlur(ref, r, radius)
		if !bytes.Equal(img.Pix, ref.Pix) {
			t.Fatalf("blur %v radius %d in %dx%d differs from the naive definition", r, radius, w, h)
		}
	}
}

func TestShareBaseNeverBleedsARedactedSecretThroughAnEffect(t *testing.T) {
	build := func() *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, 160, 100))
		for i := 0; i < len(img.Pix); i += 4 {
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 200, 200, 200, 255
		}
		for y := 30; y < 70; y++ {
			for x := 40; x < 100; x++ {
				img.SetRGBA(x, y, secret)
			}
		}
		return img
	}
	redaction := []normRect{{X: 0.2, Y: 0.25, W: 0.5, H: 0.5}}
	whole := normRect{X: 0.1, Y: 0.1, W: 0.8, H: 0.8}
	for _, kind := range []string{"blur", "pixelate"} {
		for _, size := range []int{2, 8, 33, 64} {
			img := build()
			effects := []effectOp{{Kind: kind, Rect: whole, SizePx: size}, {Kind: kind, Rect: normRect{X: 0.15, Y: 0.2, W: 0.4, H: 0.3}, SizePx: size}}
			if err := shareBase(img, redaction, effects, 0); err != nil {
				t.Fatal(err)
			}
			px := pixelRects(160, 100, redaction)
			if !verifyRedactions(img, px) {
				t.Fatalf("%s %d: the redaction is not opaque black", kind, size)
			}
			for y := range 100 {
				for x := range 160 {
					c := img.RGBAAt(x, y)
					// Only grey and black exist outside the secret, so any tint is bleed.
					if c.R != c.G || c.G != c.B || c.A != 255 {
						t.Fatalf("%s %d: pixel (%d,%d) = %v carries secret colour", kind, size, x, y, c)
					}
				}
			}
		}
	}
}

func TestShareBaseWithoutEffectsIsTheLegacyRedaction(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 5))
	a := noisyImage(rng, 50, 40)
	b := image.NewRGBA(a.Bounds())
	copy(b.Pix, a.Pix)
	reds := []normRect{{X: 0.1, Y: 0.1, W: 0.3, H: 0.4}}
	if err := shareBase(a, reds, nil, 0); err != nil {
		t.Fatal(err)
	}
	applyRedactions(b, reds)
	if !bytes.Equal(a.Pix, b.Pix) {
		t.Fatal("a draft without blur or pixelate must keep the exact legacy output")
	}
}

func TestEffectBudgetIsOptIn(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 400, 200))
	ops := make([]effectOp, 3)
	for i := range ops {
		ops[i] = effectOp{Kind: "blur", Rect: normRect{X: 0, Y: 0, W: 1, H: 1}, SizePx: 8}
	}
	// No budget: three full-image blurs are processed.
	if err := applyEffects(img, ops, 0); err != nil {
		t.Fatalf("effects refused without a configured budget: %v", err)
	}
	// A configured budget is an exact boundary and refuses before touching any pixel.
	const area = 400 * 200 * 3
	fresh := image.NewRGBA(image.Rect(0, 0, 400, 200))
	if err := applyEffects(fresh, ops, area-1); err == nil || asError(err).Code != CodeInvalidRequest {
		t.Fatalf("err = %v, want a 400 invalid request", err)
	}
	if !bytes.Equal(fresh.Pix[:16], make([]byte, 16)) {
		t.Fatal("pixels were modified although the workload was refused")
	}
	if err := applyEffects(fresh, ops, area); err != nil {
		t.Fatalf("effects at the budget refused: %v", err)
	}
}

func hasKind(t *testing.T, annots []json.RawMessage, kind string) bool {
	t.Helper()
	for _, raw := range annots {
		var a struct{ Kind string }
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatal(err)
		}
		if a.Kind == kind {
			return true
		}
	}
	return false
}

func decodeRGBA(t *testing.T, data []byte) *image.RGBA {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := image.NewRGBA(img.Bounds())
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			out.Set(x, y, img.At(x, y))
		}
	}
	return out
}

func TestPublishAppliesEffectsToTheSharedBaseOnlyAndKeepsRedactionsSolid(t *testing.T) {
	e := newEnv(t)
	capID := e.capture(humanP, secretPNG(t))
	d := e.draft(humanP, capID)
	blur := map[string]any{"id": "b1", "kind": "blur", "geometry": map[string]any{"x": 0.1, "y": 0.1, "width": 0.8, "height": 0.8}, "style": map[string]any{"effectSizePx": 8}}
	pixelate := map[string]any{"id": "p1", "kind": "pixelate", "geometry": map[string]any{"x": 0.05, "y": 0.6, "width": 0.3, "height": 0.3}, "style": map[string]any{"effectSizePx": 4}}
	d = e.annotated(humanP, d, redactAnnotation("r1"), blur, pixelate, rectAnnotation("a1"))

	var detail DraftDetail
	e.do(&humanP, "GET", "/api/v1/feedback/"+d.FeedbackID+"/draft", nil).want(200).into(&detail)
	if detail.RedactionCount != 1 {
		t.Fatalf("redactionCount = %d: blur and pixelate are not redactions", detail.RedactionCount)
	}
	preview := e.do(&humanP, "GET", "/api/v1/feedback/"+d.FeedbackID+"/draft/preview/"+ArtifactRedacted, nil).want(200).body()

	pf := e.publish(humanP, d)
	if pf.SchemaVersion != SchemaVersionAdvanced {
		t.Fatalf("schemaVersion = %s, want %s", pf.SchemaVersion, SchemaVersionAdvanced)
	}
	base := "/api/v1/feedback/" + pf.FeedbackID
	var man Manifest
	e.do(&humanP, "GET", base+"/manifest?revision=1", nil).want(200).into(&man)
	var envDoc environmentDoc
	e.do(&humanP, "GET", base+"/artifacts/"+ArtifactEnv+"?revision=1", nil).want(200).into(&envDoc)
	var ad annotationsDoc
	e.do(&humanP, "GET", base+"/artifacts/"+ArtifactAnnots+"?revision=1", nil).want(200).into(&ad)
	if man.SchemaVersion != SchemaVersionAdvanced || envDoc.SchemaVersion != SchemaVersionAdvanced || ad.SchemaVersion != SchemaVersionAdvanced {
		t.Fatalf("versions differ: manifest %s, environment %s, annotations %s", man.SchemaVersion, envDoc.SchemaVersion, ad.SchemaVersion)
	}

	redacted := e.do(&humanP, "GET", base+"/artifacts/"+ArtifactRedacted+"?revision=1", nil).want(200).body()
	annotated := e.do(&humanP, "GET", base+"/artifacts/"+ArtifactAnnotated+"?revision=1", nil).want(200).body()
	if !bytes.Equal(preview, redacted) {
		t.Fatal("the pre-publish shared preview differs from the published redacted image")
	}
	for name, data := range map[string][]byte{ArtifactRedacted: redacted, ArtifactAnnotated: annotated} {
		if hasSecret(t, data) {
			t.Fatalf("%s still contains the secret", name)
		}
		img := decodeRGBA(t, data)
		for y := 25; y < 75; y++ {
			for x := 32; x < 112; x++ {
				if c := img.RGBAAt(x, y); c != (color.RGBA{A: 255}) {
					t.Fatalf("%s: redacted pixel (%d,%d) = %v, want opaque black", name, x, y, c)
				}
			}
		}
	}

	// The base image really is blurred next to the redaction: 200 grey mixed with pre-redacted black.
	base2 := decodeRGBA(t, redacted)
	if v := base2.RGBAAt(28, 50).R; v == 200 || v == 0 {
		t.Fatalf("blur did not change the base next to the redaction: %d", v)
	}
	if c := base2.RGBAAt(28, 50); c.R != c.G || c.G != c.B {
		t.Fatalf("blurred pixel %v carries the secret colour", c)
	}
	// The uploaded annotated PNG already contains its effects: it is re-redacted, never filtered again.
	if c := decodeRGBA(t, annotated).RGBAAt(28, 50); c != (color.RGBA{R: 200, G: 200, B: 200, A: 255}) {
		t.Fatalf("annotated pixel (28,50) = %v, want the uploaded value", c)
	}
}

func TestEffectOnlyDraftStillWarnsAboutMissingRedaction(t *testing.T) {
	e := newEnv(t)
	d := e.draft(humanP, e.capture(humanP, secretPNG(t)))
	blur := map[string]any{"id": "b1", "kind": "blur", "geometry": map[string]any{"x": 0.2, "y": 0.25, "width": 0.5, "height": 0.5}}
	d = e.annotated(humanP, d, blur)
	var detail DraftDetail
	e.do(&humanP, "GET", "/api/v1/feedback/"+d.FeedbackID+"/draft", nil).want(200).into(&detail)
	warned := false
	for _, w := range detail.Warnings {
		warned = warned || w == WarnNoRedaction
	}
	if detail.RedactionCount != 0 || !warned {
		t.Fatalf("a blur was reported as redaction: %+v", detail)
	}
}

func TestNewRevisionDoesNotFilterAnAlreadyFilteredBaseTwice(t *testing.T) {
	e := newEnv(t)
	d := e.draft(humanP, e.capture(humanP, secretPNG(t)))
	blur := map[string]any{"id": "b1", "kind": "blur", "geometry": map[string]any{"x": 0.1, "y": 0.1, "width": 0.8, "height": 0.8}}
	pixelate := map[string]any{"id": "p1", "kind": "pixelate", "geometry": map[string]any{"x": 0.1, "y": 0.1, "width": 0.2, "height": 0.2}}
	spot := map[string]any{"id": "s1", "kind": "spotlight", "geometry": map[string]any{"x": 0.1, "y": 0.1, "width": 0.2, "height": 0.2}}
	d = e.annotated(humanP, d, redactAnnotation("r1"), blur, pixelate, spot, rectAnnotation("a1"))
	pf := e.publish(humanP, d)

	e.do(&humanP, "POST", "/api/v1/feedback/"+pf.FeedbackID+"/revisions", nil).want(201)
	var detail DraftDetail
	e.do(&humanP, "GET", "/api/v1/feedback/"+pf.FeedbackID+"/draft", nil).want(200).into(&detail)
	if hasKind(t, detail.Annotations, "blur") || hasKind(t, detail.Annotations, "pixelate") {
		t.Fatalf("revision 2 inherits blur or pixelate that the base image already contains: %s", detail.Annotations)
	}
	for _, kind := range []string{"redact", "spotlight", "rectangle"} {
		if !hasKind(t, detail.Annotations, kind) {
			t.Fatalf("revision 2 lost its %s annotation: %s", kind, detail.Annotations)
		}
	}
}
