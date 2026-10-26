package feedback

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/storage"
)

func drawingBody(w, h int) map[string]any {
	return map[string]any{"source": "drawing", "imageWidthPx": w, "imageHeightPx": h}
}

// drawing creates a standalone drawing through the HTTP API.
func (e *env) drawing(p Principal, w, h int) CaptureState {
	e.t.Helper()
	var st CaptureState
	e.do(&p, "POST", "/api/v1/workspaces/"+p.WorkspaceID+"/captures", drawingBody(w, h)).want(202).into(&st)
	return st
}

func whitePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	return encode(t, img)
}

// requireWhite fails unless data is an opaque all-white PNG of the given size.
func requireWhite(t *testing.T, data []byte, w, h int) {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if img.Bounds().Dx() != w || img.Bounds().Dy() != h {
		t.Fatalf("size = %v, want %dx%d", img.Bounds(), w, h)
	}
	for y := range h {
		for x := range w {
			if c := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA); c != (color.RGBA{R: 255, G: 255, B: 255, A: 255}) {
				t.Fatalf("pixel (%d,%d) = %v, want opaque white", x, y, c)
			}
		}
	}
}

// annotatedWith patches annots and binds preview as the draft preview.
func (e *env) annotatedWith(p Principal, d Draft, preview []byte, annots ...map[string]any) Draft {
	e.t.Helper()
	var out Draft
	e.do(&p, "PATCH", "/api/v1/feedback/"+d.FeedbackID, map[string]any{"annotations": annots}, "If-Match", quote(d.ETag)).want(200).into(&out)
	var dd DraftDetail
	e.png(&p, "PUT", "/api/v1/feedback/"+d.FeedbackID+"/draft/preview", preview, "If-Match", quote(out.ETag)).want(200).into(&dd)
	if !dd.PreviewReady {
		e.t.Fatalf("preview not bound: %+v", dd)
	}
	return out
}

func denied(t *testing.T, r resp) {
	t.Helper()
	if s := r.status(); s != 403 && s != 404 {
		t.Fatalf("status = %d, want 403 or 404; body: %s", s, r.body())
	}
}

func shapeAnnotations() []map[string]any {
	return []map[string]any{
		{"id": "e1", "kind": "ellipse", "comment": "oval", "geometry": map[string]any{"x": 0.1, "y": 0.1, "width": 0.3, "height": 0.2},
			"style": map[string]any{"stroke": "#112233", "fill": "#ffeedd", "strokeWidthPx": 2}},
		{"id": "d1", "kind": "diamond", "geometry": map[string]any{"x": 0.5, "y": 0.1, "width": 0.4, "height": 0.3}},
		{"id": "l1", "kind": "line", "geometry": map[string]any{"start": map[string]any{"x": 0.0, "y": 0.9}, "end": map[string]any{"x": 1.0, "y": 0.95}}},
		rectAnnotation("a1"),
		redactAnnotation("r1"),
	}
}

func TestDrawingCaptureIsReadyWithoutRuntime(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.RuntimeGeneration = func(string) string {
			t.Error("the runtime generation was consulted for a drawing")
			return "gen-x"
		}
		o.ViewportCapturer = func(context.Context, ViewportRequest) (ViewportIngest, error) {
			t.Error("the viewport capturer was called for a drawing")
			return ViewportIngest{}, errors.New("browser unavailable")
		}
		o.WorkspaceExists = func(id string) bool { return id == "ws1" }
	})
	raw := e.do(&humanP, "POST", "/api/v1/workspaces/ws1/captures", drawingBody(160, 100)).want(202)
	for _, want := range []string{`"runtimeGeneration":""`, `"targetId":null`, `"navigationId":null`, `"source":"drawing"`, `"provenance":"generated-canvas"`, `"status":"ready"`} {
		if !strings.Contains(string(raw.body()), want) {
			t.Fatalf("response lacks %s: %s", want, raw.body())
		}
	}
	for _, absent := range []string{`"page"`, `"parentTransform"`, `"feedbackId"`} {
		if strings.Contains(string(raw.body()), absent) {
			t.Fatalf("response must not carry %s: %s", absent, raw.body())
		}
	}
	var st CaptureState
	raw.into(&st)
	if st.Status != StatusReady || st.Source != SourceDrawing || st.Provenance != ProvGenerated || st.Consistency != ConsUnknown ||
		st.RuntimeGeneration != "" || st.TargetID != nil || st.NavigationID != nil || st.Page != nil || st.ParentTransform != nil ||
		st.ImageWidthPx != 160 || st.ImageHeightPx != 100 || st.CapturedAt == nil || st.ServerReceivedAt == nil {
		t.Fatalf("state = %+v", st)
	}
	if e.privateFiles() != 1 {
		t.Fatalf("private files = %d, want exactly the generated canvas", e.privateFiles())
	}

	// The owner opens the ready private PNG immediately: opaque white, exact size.
	img := e.do(&humanP, "GET", "/api/v1/captures/"+st.CaptureID+"/image", nil).want(200)
	requireWhite(t, img.body(), 160, 100)
	var again CaptureState
	e.do(&humanP, "GET", "/api/v1/captures/"+st.CaptureID, nil).want(200).into(&again)
	if again.CaptureID != st.CaptureID || again.Status != StatusReady {
		t.Fatalf("GET state = %+v", again)
	}

	// Private to the owner: Agents, viewers, other humans and other workspaces see nothing.
	for _, path := range []string{"/api/v1/captures/" + st.CaptureID, "/api/v1/captures/" + st.CaptureID + "/image"} {
		e.do(&agentP, "GET", path, nil).wantErr(403, CodeForbidden)
		e.do(&viewerP, "GET", path, nil).wantErr(403, CodeForbidden)
		e.do(&human2P, "GET", path, nil).wantErr(404, CodeNotFound)
		e.do(&foreignP, "GET", path, nil).wantErr(404, CodeNotFound)
		e.do(nil, "GET", path, nil).wantErr(401, CodeUnauthenticated)
	}
}

func TestDrawingCaptureValidationAndAuthorization(t *testing.T) {
	e := newEnv(t)
	post := func(body any) resp { return e.do(&humanP, "POST", "/api/v1/workspaces/ws1/captures", body) }

	for name, body := range map[string]any{
		"zero width":              drawingBody(0, 100),
		"negative height":         drawingBody(100, -1),
		"missing both":            map[string]any{"source": "drawing"},
		"missing height":          map[string]any{"source": "drawing", "imageWidthPx": 100},
		"fractional width":        map[string]any{"source": "drawing", "imageWidthPx": 100.5, "imageHeightPx": 100},
		"string width":            map[string]any{"source": "drawing", "imageWidthPx": "100", "imageHeightPx": 100},
		"target":                  map[string]any{"source": "drawing", "imageWidthPx": 100, "imageHeightPx": 100, "targetId": "t"},
		"lease":                   map[string]any{"source": "drawing", "imageWidthPx": 100, "imageHeightPx": 100, "leaseId": "l"},
		"allowInterrupt":          map[string]any{"source": "drawing", "imageWidthPx": 100, "imageHeightPx": 100, "allowInterrupt": true},
		"desktop with size":       map[string]any{"source": "desktop", "imageWidthPx": 100, "imageHeightPx": 100},
		"viewport with size":      map[string]any{"source": "page-viewport", "targetId": "t", "leaseId": "l", "imageWidthPx": 100, "imageHeightPx": 100},
		"unknown source":          map[string]any{"source": "canvas", "imageWidthPx": 100, "imageHeightPx": 100},
		"unknown field":           map[string]any{"source": "drawing", "imageWidthPx": 100, "imageHeightPx": 100, "background": "#000000"},
		"page-full is not a kind": map[string]any{"source": "page-full", "imageWidthPx": 100, "imageHeightPx": 100},
	} {
		t.Run(name, func(t *testing.T) { post(body).wantErr(400, CodeInvalidRequest) })
	}
	if e.privateFiles() != 0 {
		t.Fatalf("a rejected drawing left %d private files", e.privateFiles())
	}

	// Any positive size is accepted, never clipped or defaulted: below 64
	// and above 4096 per side included.
	for _, sz := range [][2]int{{1, 1}, {63, 100}, {4097, 10}, {10, 5000}, {64, 64}} {
		var st CaptureState
		post(drawingBody(sz[0], sz[1])).want(202).into(&st)
		if st.ImageWidthPx != sz[0] || st.ImageHeightPx != sz[1] {
			t.Fatalf("size = %dx%d, want %dx%d", st.ImageWidthPx, st.ImageHeightPx, sz[0], sz[1])
		}
	}
	// A size whose RGBA buffer cannot be represented is refused before any allocation.
	post(drawingBody(1<<31-1, 1<<31-1)).wantErr(413, CodeImageTooLarge)
	if e.privateFiles() != 5 {
		t.Fatalf("private files = %d, want exactly the 5 accepted canvases", e.privateFiles())
	}

	// Only a human of the workspace may create one.
	e.do(&agentP, "POST", "/api/v1/workspaces/ws1/captures", drawingBody(100, 100)).wantErr(403, CodeForbidden)
	e.do(&viewerP, "POST", "/api/v1/workspaces/ws1/captures", drawingBody(100, 100)).wantErr(403, CodeForbidden)
	e.do(&foreignP, "POST", "/api/v1/workspaces/ws1/captures", drawingBody(100, 100)).wantErr(403, CodeForbidden)
	e.do(nil, "POST", "/api/v1/workspaces/ws1/captures", drawingBody(100, 100)).wantErr(401, CodeUnauthenticated)
}

func TestDrawingCaptureHonoursSharedImageAndQuotaLimits(t *testing.T) {
	small := newEnv(t, func(o *Options) { o.Limits.MaxPixels = 10_000 })
	small.do(&humanP, "POST", "/api/v1/workspaces/ws1/captures", drawingBody(128, 128)).wantErr(413, CodeImageTooLarge)
	small.do(&humanP, "POST", "/api/v1/workspaces/ws1/captures", drawingBody(100, 100)).want(202)

	narrow := newEnv(t, func(o *Options) { o.Limits.MaxDimension = 100 })
	narrow.do(&humanP, "POST", "/api/v1/workspaces/ws1/captures", drawingBody(128, 64)).wantErr(413, CodeImageTooLarge)
	narrow.do(&humanP, "POST", "/api/v1/workspaces/ws1/captures", drawingBody(100, 64)).want(202)

	// The encoded PNG counts against the shared image size limit.
	tiny := newEnv(t, func(o *Options) { o.Limits.MaxImageBytes = 50 })
	tiny.do(&humanP, "POST", "/api/v1/workspaces/ws1/captures", drawingBody(64, 64)).wantErr(413, CodeImageTooLarge)
	if tiny.privateFiles() != 0 {
		t.Fatalf("private files = %d after an oversized drawing", tiny.privateFiles())
	}

	open := newEnv(t, func(o *Options) { o.Limits.MaxOpenCaptures = 1 })
	first := open.drawing(humanP, 100, 100)
	open.do(&humanP, "POST", "/api/v1/workspaces/ws1/captures", drawingBody(100, 100)).wantErr(429, CodeQuotaExceeded)
	if open.privateFiles() != 1 {
		t.Fatalf("a quota-blocked drawing must not leave its canvas behind, private files = %d", open.privateFiles())
	}
	requireWhite(t, open.do(&humanP, "GET", "/api/v1/captures/"+first.CaptureID+"/image", nil).want(200).body(), 100, 100)

	bytesQuota := newEnv(t, func(o *Options) { o.Limits.MaxPrivateBytes = 10 })
	bytesQuota.do(&humanP, "POST", "/api/v1/workspaces/ws1/captures", drawingBody(100, 100)).wantErr(429, CodeQuotaExceeded)
	if bytesQuota.privateFiles() != 0 {
		t.Fatalf("private files = %d after a byte-quota rejection", bytesQuota.privateFiles())
	}
	var rows int
	if err := bytesQuota.st.DB.SQL().QueryRow(`SELECT COUNT(*) FROM captures`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("captures = %d, %v after a quota rejection", rows, err)
	}
}

func TestDrawingCaptureIdempotency(t *testing.T) {
	e := newEnv(t)
	url := "/api/v1/workspaces/ws1/captures"
	var a, b CaptureState
	e.do(&humanP, "POST", url, drawingBody(160, 100), "Idempotency-Key", "dk-1").want(202).into(&a)
	e.do(&humanP, "POST", url, drawingBody(160, 100), "Idempotency-Key", "dk-1").want(202).into(&b)
	if a.CaptureID == "" || a.CaptureID != b.CaptureID {
		t.Fatalf("replay created another capture: %s vs %s", a.CaptureID, b.CaptureID)
	}
	var rows int
	if err := e.st.DB.SQL().QueryRow(`SELECT COUNT(*) FROM captures`).Scan(&rows); err != nil || rows != 1 || e.privateFiles() != 1 {
		t.Fatalf("captures = %d (%v), private files = %d after a replay", rows, err, e.privateFiles())
	}
	e.do(&humanP, "POST", url, drawingBody(161, 100), "Idempotency-Key", "dk-1").wantErr(409, CodeIdemKeyReused)
	e.do(&humanP, "POST", url, map[string]any{"source": "desktop"}, "Idempotency-Key", "dk-1").wantErr(409, CodeIdemKeyReused)
	e.do(&humanP, "POST", url, drawingBody(160, 100), "Idempotency-Key", "").wantErr(400, CodeIdemKeyRequired)
	// A different key is a new drawing.
	var c CaptureState
	e.do(&humanP, "POST", url, drawingBody(160, 100), "Idempotency-Key", "dk-2").want(202).into(&c)
	if c.CaptureID == a.CaptureID {
		t.Fatal("a new key must create a new drawing")
	}
}

func TestDrawingPublishesVectorShapesAndStaysPrivateFromAgents(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.RuntimeGeneration = func(string) string { return "gen-1" }
	})
	st := e.drawing(humanP, 160, 100)
	d := e.draft(humanP, st.CaptureID)
	base := "/api/v1/feedback/" + d.FeedbackID

	// Before publication the Agent reaches neither the capture nor the draft nor its previews.
	e.do(&agentP, "GET", "/api/v1/captures/"+st.CaptureID, nil).wantErr(403, CodeForbidden)
	e.do(&agentP, "GET", "/api/v1/captures/"+st.CaptureID+"/image", nil).wantErr(403, CodeForbidden)
	e.do(&agentP, "GET", base+"/draft", nil).wantErr(403, CodeForbidden)
	denied(t, e.do(&agentP, "GET", base+"/draft/preview/"+ArtifactRedacted, nil))
	denied(t, e.do(&human2P, "GET", base+"/draft", nil))
	denied(t, e.do(&human2P, "GET", base+"?revision=1", nil))
	e.do(&agentP, "PATCH", base, map[string]any{"annotations": shapeAnnotations()}, "If-Match", quote(d.ETag)).wantErr(403, CodeForbidden)
	e.do(&agentP, "POST", base+"/publish", map[string]any{"humanApproved": true}, "If-Match", quote(d.ETag)).wantErr(403, CodeForbidden)

	// The new vectors are saved canonically in the private draft.
	d = e.annotatedWith(humanP, d, secretPNG(t), shapeAnnotations()...)
	var dd DraftDetail
	e.do(&humanP, "GET", base+"/draft", nil).want(200).into(&dd)
	if dd.CaptureID != st.CaptureID || len(dd.Annotations) != 5 || dd.RedactionCount != 1 {
		t.Fatalf("draft = %+v", dd)
	}
	for i, kind := range []string{"ellipse", "diamond", "line", "rectangle", "redact"} {
		var a struct{ Kind string }
		if err := json.Unmarshal(dd.Annotations[i], &a); err != nil || a.Kind != kind {
			t.Fatalf("annotation %d = %s (%v), want kind %s", i, dd.Annotations[i], err, kind)
		}
	}
	if !strings.Contains(string(dd.Annotations[0]), `"fill":"#ffeedd"`) {
		t.Fatalf("ellipse fill was not kept: %s", dd.Annotations[0])
	}

	pf := e.publish(humanP, d)
	if pf.SchemaVersion != SchemaVersion || pf.Capture.Source != SourceDrawing || pf.Capture.Provenance != ProvGenerated ||
		pf.Capture.RuntimeGeneration != "" || pf.Capture.TargetID != nil || pf.Capture.NavigationID != nil || pf.Capture.Page != nil ||
		pf.Capture.ImageWidthPx != 160 || pf.Capture.ImageHeightPx != 100 || len(pf.Annotations) != 5 {
		t.Fatalf("published = %+v", pf)
	}
	if !pf.SharePolicy.HumanApproved || !pf.SharePolicy.RedactionsApplied || pf.SharePolicy.IncludesRawImage {
		t.Fatalf("share policy = %+v", pf.SharePolicy)
	}
	if e.privateFiles() != 0 {
		t.Fatalf("the generated canvas and preview must be deleted after publish, %d remain", e.privateFiles())
	}
	e.do(&humanP, "GET", "/api/v1/captures/"+st.CaptureID+"/image", nil).wantErr(409, CodeCaptureNotReady)

	// The bundle documents carry the same version and an explicit "no runtime" capture context.
	var env environmentDoc
	envRaw := e.do(&humanP, "GET", base+"/artifacts/"+ArtifactEnv+"?revision=1", nil).want(200)
	envRaw.into(&env)
	if env.SchemaVersion != SchemaVersion || env.Source != SourceDrawing || env.Provenance != ProvGenerated || env.RuntimeGeneration != "" ||
		env.TargetID != nil || env.NavigationID != nil || env.Page != nil {
		t.Fatalf("environment = %+v", env)
	}
	if !strings.Contains(string(envRaw.body()), `"targetId":null`) {
		t.Fatalf("environment.json must state targetId null: %s", envRaw.body())
	}
	var ad annotationsDoc
	e.do(&humanP, "GET", base+"/artifacts/"+ArtifactAnnots+"?revision=1", nil).want(200).into(&ad)
	if ad.SchemaVersion != SchemaVersion || len(ad.Annotations) != 5 || ad.ImageWidthPx != 160 {
		t.Fatalf("annotations.json = %+v", ad)
	}
	var man Manifest
	e.do(&humanP, "GET", base+"/manifest?revision=1", nil).want(200).into(&man)
	if man.SchemaVersion != SchemaVersion {
		t.Fatalf("manifest schemaVersion = %q", man.SchemaVersion)
	}
	for _, name := range []string{ArtifactRedacted, ArtifactAnnotated} {
		if hasSecret(t, e.do(&humanP, "GET", base+"/artifacts/"+name+"?revision=1", nil).want(200).body()) {
			t.Fatalf("%s leaks the secret under the redaction", name)
		}
	}

	// Agent isolation survives publication: invisible until dispatched, then only the assigned Agent.
	e.do(&agentP, "GET", base+"?revision=1", nil).wantErr(404, CodeNotFound)
	var list FeedbackList
	e.do(&agentP, "GET", "/api/v1/feedback?workspaceId=ws1", nil).want(200).into(&list)
	if len(list.Items) != 0 {
		t.Fatalf("agent sees undispatched drawing feedback: %+v", list)
	}
	e.do(&humanP, "POST", base+"/dispatch", map[string]any{"revision": 1, "agentId": "agent-1"}).want(202)
	var got PublishedFeedback
	e.do(&agentP, "GET", base+"?revision=1", nil).want(200).into(&got)
	if got.Capture.Source != SourceDrawing || got.SchemaVersion != SchemaVersion {
		t.Fatalf("agent read = %+v", got)
	}
	for _, a := range got.Artifacts {
		r := e.do(&agentP, "GET", base+"/artifacts/"+a.Name+"?revision=1", nil).want(200)
		if sha256Hex(r.body()) != a.SHA256 {
			t.Fatalf("artifact %s hash mismatch", a.Name)
		}
	}
	e.do(&agent2P, "GET", base+"?revision=1", nil).wantErr(404, CodeNotFound)
	e.do(&agent2P, "GET", base+"/artifacts/"+ArtifactRedacted+"?revision=1", nil).wantErr(404, CodeNotFound)
	e.do(&agentP, "GET", "/api/v1/captures/"+got.Capture.ID, nil).wantErr(403, CodeForbidden)
	e.do(&agentP, "GET", "/api/v1/captures/"+got.Capture.ID+"/image", nil).wantErr(403, CodeForbidden)
	e.do(&agentP, "GET", base+"/draft", nil).wantErr(403, CodeForbidden)
}

func TestPublishedSchemaVersionIsPerDocument(t *testing.T) {
	// Old content keeps the legacy version so 0.1.0 clients still read it.
	e := newEnv(t)
	_, legacy := publishedFlow(t, e)
	if legacy.SchemaVersion != SchemaVersionLegacy {
		t.Fatalf("legacy feedback version = %q, want %q", legacy.SchemaVersion, SchemaVersionLegacy)
	}
	var env environmentDoc
	e.do(&humanP, "GET", "/api/v1/feedback/"+legacy.FeedbackID+"/artifacts/"+ArtifactEnv+"?revision=1", nil).want(200).into(&env)
	var man Manifest
	e.do(&humanP, "GET", "/api/v1/feedback/"+legacy.FeedbackID+"/manifest?revision=1", nil).want(200).into(&man)
	if env.SchemaVersion != SchemaVersionLegacy || man.SchemaVersion != SchemaVersionLegacy {
		t.Fatalf("legacy documents = %q / %q", env.SchemaVersion, man.SchemaVersion)
	}

	// A screenshot with an ellipse needs the new version.
	capID := e.capture(humanP, secretPNG(t))
	d := e.draft(humanP, capID)
	d = e.annotated(humanP, d, shapeAnnotations()[0], redactAnnotation("r1"))
	pf := e.publish(humanP, d)
	if pf.SchemaVersion != SchemaVersion || pf.Capture.Source != SourceDesktop {
		t.Fatalf("ellipse on a desktop capture = %q / %q", pf.SchemaVersion, pf.Capture.Source)
	}

	cases := []struct {
		source string
		kinds  []string
		want   string
	}{
		{SourceDesktop, []string{"rectangle", "arrow", "freehand", "text", "pin", "redact"}, SchemaVersionLegacy},
		{SourceViewport, nil, SchemaVersionLegacy},
		{SourceDesktop, []string{"rectangle", "line"}, SchemaVersion},
		{SourceViewport, []string{"diamond"}, SchemaVersion},
		{SourceDesktop, []string{"ellipse"}, SchemaVersion},
		{SourceDrawing, nil, SchemaVersion},
	}
	for _, c := range cases {
		var annots []json.RawMessage
		for _, k := range c.kinds {
			annots = append(annots, json.RawMessage(fmt.Sprintf(`{"id":"x","kind":%q,"geometry":{}}`, k)))
		}
		if got := documentSchemaVersion(c.source, annots, Task{Kind: "bug", Intent: "analyze-only", Title: "t", Description: "d", ExpectedResult: "e", ActualResult: "a"}); got != c.want {
			t.Errorf("documentSchemaVersion(%s, %v) = %s, want %s", c.source, c.kinds, got, c.want)
		}
	}
	if got := documentSchemaVersion(SourceDesktop, nil, Task{Kind: "question", Intent: "analyze-only"}); got != SchemaVersion {
		t.Errorf("a task without a description on a screenshot = %s, want %s", got, SchemaVersion)
	}
}

func TestCropAndRevisionKeepDrawingOrigin(t *testing.T) {
	e := newEnv(t)
	parent := e.drawing(humanP, 160, 100)

	// A crop may be smaller than a new drawing may be, and is still a drawing.
	var child CaptureState
	e.do(&humanP, "POST", "/api/v1/captures/"+parent.CaptureID+"/crop", map[string]any{"x": 10, "y": 20, "width": 40, "height": 30}).want(201).into(&child)
	if child.Source != SourceDrawing || child.Provenance != ProvGenerated || child.RuntimeGeneration != "" || child.TargetID != nil ||
		child.NavigationID != nil || child.Page != nil || child.ImageWidthPx != 40 || child.ImageHeightPx != 30 {
		t.Fatalf("cropped drawing = %+v", child)
	}
	if pt := child.ParentTransform; pt == nil || pt.Kind != TransformCrop || pt.ParentCaptureID != parent.CaptureID || pt.CropXPx != 10 || pt.CropYPx != 20 {
		t.Fatalf("transform = %+v", pt)
	}
	requireWhite(t, e.do(&humanP, "GET", "/api/v1/captures/"+child.CaptureID+"/image", nil).want(200).body(), 40, 30)
	var untouched CaptureState
	e.do(&humanP, "GET", "/api/v1/captures/"+parent.CaptureID, nil).want(200).into(&untouched)
	if untouched.ImageWidthPx != 160 || untouched.Source != SourceDrawing {
		t.Fatalf("parent changed: %+v", untouched)
	}

	// Revision 1 on the crop, then a new revision from the published base.
	d := e.draft(humanP, child.CaptureID)
	d = e.annotatedWith(humanP, d, whitePNG(t, 40, 30), shapeAnnotations()[0])
	pf1 := e.publish(humanP, d)
	if pf1.Capture.Source != SourceDrawing || pf1.Capture.Provenance != ProvGenerated || pf1.SchemaVersion != SchemaVersion {
		t.Fatalf("revision 1 = %+v", pf1)
	}
	base := "/api/v1/feedback/" + pf1.FeedbackID
	var d2 Draft
	e.do(&humanP, "POST", base+"/revisions", nil).want(201).into(&d2)
	var dd DraftDetail
	e.do(&humanP, "GET", base+"/draft", nil).want(200).into(&dd)
	var base2 CaptureState
	e.do(&humanP, "GET", "/api/v1/captures/"+dd.CaptureID, nil).want(200).into(&base2)
	if base2.Source != SourceDrawing || base2.Provenance != ProvGenerated || base2.RuntimeGeneration != "" || base2.TargetID != nil || base2.Page != nil ||
		base2.ParentTransform == nil || base2.ParentTransform.Kind != TransformRevision {
		t.Fatalf("revision base = %+v", base2)
	}
	d2 = e.annotatedWith(humanP, d2, whitePNG(t, 40, 30), shapeAnnotations()[1])
	pf2 := e.publish(humanP, d2)
	if pf2.Revision != 2 || pf2.Capture.Source != SourceDrawing || pf2.Capture.Provenance != ProvGenerated || pf2.Capture.RuntimeGeneration != "" || pf2.Capture.Page != nil {
		t.Fatalf("revision 2 = %+v", pf2)
	}
	// Alternatively a fresh drawing may serve as the base of a later revision.
	fresh := e.drawing(humanP, 64, 64)
	var d3 Draft
	e.do(&humanP, "POST", base+"/revisions", map[string]any{"captureId": fresh.CaptureID}).want(201).into(&d3)
	if d3.Revision != 3 {
		t.Fatalf("revision = %d", d3.Revision)
	}
}

func TestParseAnnotationsNewShapeKinds(t *testing.T) {
	in := raws(t,
		`{"id":"e","kind":"ellipse","geometry":{"x":0.1,"y":0.1,"width":0.5,"height":0.5},"style":{"fill":"#00ff00"}}`,
		`{"id":"d","kind":"diamond","geometry":{"x":0.5,"y":0.5,"width":0.5,"height":0.5},"comment":"c"}`,
		`{"id":"l","kind":"line","geometry":{"start":{"x":0,"y":0},"end":{"x":1,"y":1}}}`,
	)
	canon, reds, err := ParseAnnotations(in)
	if err != nil || len(canon) != 3 || len(reds) != 0 {
		t.Fatalf("canon = %d, reds = %d, err = %v", len(canon), len(reds), err)
	}
	want := []string{
		`{"id":"e","kind":"ellipse","geometry":{"x":0.1,"y":0.1,"width":0.5,"height":0.5},"style":{"fill":"#00ff00"}}`,
		`{"id":"d","kind":"diamond","geometry":{"x":0.5,"y":0.5,"width":0.5,"height":0.5},"comment":"c"}`,
		`{"id":"l","kind":"line","geometry":{"start":{"x":0,"y":0},"end":{"x":1,"y":1}}}`,
	}
	for i := range want {
		if got := strings.TrimSpace(string(canon[i])); got != want[i] {
			t.Fatalf("canonical %d = %s, want %s", i, got, want[i])
		}
	}
	again, _, err := ParseAnnotations(canon)
	if err != nil {
		t.Fatal(err)
	}
	for i := range canon {
		if string(canon[i]) != string(again[i]) {
			t.Fatalf("canonical form unstable: %s vs %s", canon[i], again[i])
		}
	}
	// A line is an arrow without a head: identical geometry, identical rules.
	arrow, _, err := ParseAnnotations(raws(t, `{"id":"l","kind":"arrow","geometry":{"start":{"x":0,"y":0},"end":{"x":1,"y":1}}}`))
	if err != nil || strings.Replace(string(arrow[0]), `"arrow"`, `"line"`, 1) != string(canon[2]) {
		t.Fatalf("arrow = %s (%v)", arrow, err)
	}
	// Zero-length lines are as valid as zero-length arrows.
	if _, _, err := ParseAnnotations(raws(t, `{"id":"z","kind":"line","geometry":{"start":{"x":0.3,"y":0.3},"end":{"x":0.3,"y":0.3}}}`)); err != nil {
		t.Fatal(err)
	}

	bad := map[string]string{
		"ellipse x+w > 1":      `{"id":"a","kind":"ellipse","geometry":{"x":0.6,"y":0,"width":0.5,"height":0.1}}`,
		"diamond y+h > 1":      `{"id":"a","kind":"diamond","geometry":{"x":0,"y":0.9,"width":0.1,"height":0.2}}`,
		"ellipse zero width":   `{"id":"a","kind":"ellipse","geometry":{"x":0,"y":0,"width":0,"height":0.1}}`,
		"diamond negative x":   `{"id":"a","kind":"diamond","geometry":{"x":-0.1,"y":0,"width":0.1,"height":0.1}}`,
		"ellipse width > 1":    `{"id":"a","kind":"ellipse","geometry":{"x":0,"y":0,"width":1.1,"height":0.1}}`,
		"ellipse with mode":    `{"id":"a","kind":"ellipse","mode":"solid","geometry":{"x":0,"y":0,"width":0.1,"height":0.1}}`,
		"ellipse bad fill":     `{"id":"a","kind":"ellipse","geometry":{"x":0,"y":0,"width":0.1,"height":0.1},"style":{"fill":"red"}}`,
		"ellipse arrow geom":   `{"id":"a","kind":"ellipse","geometry":{"start":{"x":0,"y":0},"end":{"x":1,"y":1}}}`,
		"ellipse extra field":  `{"id":"a","kind":"ellipse","geometry":{"x":0,"y":0,"width":0.1,"height":0.1,"rotation":3}}`,
		"line missing end":     `{"id":"a","kind":"line","geometry":{"start":{"x":0,"y":0}}}`,
		"line point > 1":       `{"id":"a","kind":"line","geometry":{"start":{"x":0,"y":0},"end":{"x":1.01,"y":1}}}`,
		"line rect geometry":   `{"id":"a","kind":"line","geometry":{"x":0,"y":0,"width":0.1,"height":0.1}}`,
		"line extra field":     `{"id":"a","kind":"line","geometry":{"start":{"x":0,"y":0},"end":{"x":1,"y":1},"head":"open"}}`,
		"line with mode":       `{"id":"a","kind":"line","mode":"solid","geometry":{"start":{"x":0,"y":0},"end":{"x":1,"y":1}}}`,
		"unknown polygon kind": `{"id":"a","kind":"polygon","geometry":{"x":0,"y":0,"width":0.1,"height":0.1}}`,
		"redact with fill":     `{"id":"a","kind":"redact","mode":"solid","geometry":{"x":0,"y":0,"width":0.1,"height":0.1},"style":{"fill":"#000000"}}`,
	}
	for name, raw := range bad {
		if _, _, err := ParseAnnotations(raws(t, raw)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// New shapes never become server redactions.
	_, reds, err = ParseAnnotations(raws(t, `{"id":"e","kind":"ellipse","geometry":{"x":0,"y":0,"width":1,"height":1},"style":{"fill":"#000000"}}`))
	if err != nil || len(reds) != 0 {
		t.Fatalf("redactions = %d, err = %v", len(reds), err)
	}
}

func dumpRows(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, v := range vals {
			fmt.Fprintf(&b, "%T:%v|", v, v)
		}
		out = append(out, b.String())
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameRows(t *testing.T, what string, before, after []string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("%s: %d rows before, %d after", what, len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("%s row %d changed:\n before %s\n after  %s", what, i, before[i], after[i])
		}
	}
}

// downgradeToV1 rebuilds the captures table exactly as migration 1 defines it
// and forgets migration 2, producing a database a v1 Gateway left behind.
func downgradeToV1(t *testing.T, db *storage.DB) {
	t.Helper()
	old := strings.Replace(migrations[0].Statements[0], "CREATE TABLE captures (", "CREATE TABLE captures_old (", 1)
	if old == migrations[0].Statements[0] {
		t.Fatal("migration 1 no longer starts with the captures table")
	}
	cols := `id, workspace_id, owner_id, source, provenance, status, consistency, runtime_generation,
		target_id, navigation_id, lease_id, prior_lease_id, captured_at, server_received_at, width, height,
		page_json, parent_json, object_key, sha256, size_bytes, feedback_id, created_at, updated_at, expires_at`
	err := db.InTx(context.Background(), func(tx *sql.Tx) error {
		for _, stmt := range []string{
			old,
			`INSERT INTO captures_old (` + cols + `) SELECT ` + cols + ` FROM captures`,
			`DROP TABLE captures`,
			`ALTER TABLE captures_old RENAME TO captures`,
			`CREATE INDEX captures_owner ON captures (workspace_id, owner_id)`,
			`DELETE FROM schema_migrations WHERE component = 'feedback' AND version = 2`,
		} {
			if _, err := tx.Exec(stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

const insertCaptureSQL = `INSERT INTO captures (id, workspace_id, owner_id, source, provenance, status, consistency, runtime_generation,
	target_id, navigation_id, lease_id, prior_lease_id, page_json, created_at, updated_at, expires_at)
	VALUES (?, 'ws1', 'human-1', ?, ?, 'ready', ?, ?, ?, ?, ?, ?, ?, 1, 1, 1)`

func TestMigrationV2PreservesV1DataAndWidensCaptureChecks(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, func(o *Options) {
		o.ViewportCapturer = func(context.Context, ViewportRequest) (ViewportIngest, error) {
			return ViewportIngest{
				PNG: secretPNG(t), TargetID: "target-1", NavigationID: "nav-1", Consistency: ConsStable, RuntimeGeneration: "gen-7",
				LeaseID: "lease-capture", PriorLeaseID: "lease-prior", CapturedAt: o.Now().Add(-time.Second),
				Page: PageInfo{URLSanitized: "https://app.test/path", TitleSanitized: "Dashboard", ViewportWidthCSS: 80, ViewportHeightCSS: 50,
					PixelsPerCSSX: 2, PixelsPerCSSY: 2, ScrollYCSS: 10, BrowserVersion: "Chromium 130"},
			}, nil
		}
	})

	// Real history written by the v1 code paths: a dispatched published revision with
	// artifacts and delivery history, an open page-capture draft, and an unused capture.
	_, pf := publishedFlow(t, e)
	base := "/api/v1/feedback/" + pf.FeedbackID
	e.do(&humanP, "POST", base+"/dispatch", map[string]any{"revision": 1, "agentId": "agent-1"}).want(202)
	var vp CaptureState
	e.do(&humanP, "POST", "/api/v1/workspaces/ws1/captures", map[string]any{"source": "page-viewport", "targetId": "target-1", "leaseId": "lease-1"}).want(202).into(&vp)
	vd := e.annotated(humanP, e.draft(humanP, vp.CaptureID), rectAnnotation("v1"), redactAnnotation("r1"))
	spare := e.capture(humanP, secretPNG(t))

	snapshot := func() map[string][]string {
		db := e.st.DB.SQL()
		return map[string][]string{
			"captures":         dumpRows(t, db, `SELECT * FROM captures ORDER BY id`),
			"feedback":         dumpRows(t, db, `SELECT * FROM feedback ORDER BY id`),
			"revisions":        dumpRows(t, db, `SELECT * FROM revisions ORDER BY feedback_id, revision`),
			"artifacts":        dumpRows(t, db, `SELECT * FROM artifacts ORDER BY feedback_id, revision, name`),
			"deliveries":       dumpRows(t, db, `SELECT * FROM deliveries ORDER BY seq`),
			"delivery_history": dumpRows(t, db, `SELECT * FROM delivery_history ORDER BY id`),
			"idempotency":      dumpRows(t, db, `SELECT * FROM idempotency ORDER BY scope, key`),
			"schema":           dumpRows(t, db, `SELECT type, name, tbl_name FROM sqlite_master WHERE type IN ('index','trigger') AND name NOT LIKE 'sqlite_%' ORDER BY name`),
		}
	}
	before := snapshot()
	publishedBefore := e.do(&humanP, "GET", base+"?revision=1", nil).want(200).body()
	bundleBefore := e.do(&humanP, "GET", base+"/bundle?revision=1", nil).want(200).body()
	draftBefore := e.do(&humanP, "GET", "/api/v1/feedback/"+vd.FeedbackID+"/draft", nil).want(200).body()
	spareBefore := e.do(&humanP, "GET", "/api/v1/captures/"+spare+"/image", nil).want(200).body()

	// Turn the database back into what a v1 Gateway leaves behind.
	if err := e.st.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := storage.OpenDB(e.cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	downgradeToV1(t, raw)
	if _, err := raw.SQL().Exec(insertCaptureSQL, "cap_old_drawing", SourceDrawing, ProvGenerated, ConsUnknown, "", nil, nil, nil, nil, nil); err == nil {
		t.Fatal("the downgraded database already accepts drawings: the test would prove nothing")
	}
	versions, err := storage.SchemaVersions(ctx, raw)
	if err != nil || versions["feedback"] != 1 {
		t.Fatalf("downgraded versions = %v, %v", versions, err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// The next start applies migration 2.
	e.open()
	versions, err = storage.SchemaVersions(ctx, e.st.DB)
	if err != nil || versions["feedback"] != 2 || versions["storage"] != 1 {
		t.Fatalf("versions = %v, %v", versions, err)
	}
	after := snapshot()
	for name := range before {
		sameRows(t, name, before[name], after[name])
	}
	// Re-running is a no-op, and a v1 binary refuses the migrated database.
	if err := Migrate(ctx, e.st.DB); err != nil {
		t.Fatal(err)
	}
	if err := e.st.DB.Migrate(ctx, "feedback", migrations[:1]); err == nil || !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("a v1 binary accepted the v2 database: %v", err)
	}

	// Published history is byte-identical and the old drafts are still usable.
	if !bytes.Equal(publishedBefore, e.do(&humanP, "GET", base+"?revision=1", nil).want(200).body()) {
		t.Fatal("published feedback changed across the migration")
	}
	if !bytes.Equal(bundleBefore, e.do(&humanP, "GET", base+"/bundle?revision=1", nil).want(200).body()) {
		t.Fatal("published bundle changed across the migration")
	}
	if !bytes.Equal(draftBefore, e.do(&humanP, "GET", "/api/v1/feedback/"+vd.FeedbackID+"/draft", nil).want(200).body()) {
		t.Fatal("open draft changed across the migration")
	}
	var pubCap PublishedFeedback
	e.do(&agentP, "GET", base+"?revision=1", nil).want(200).into(&pubCap)
	if pubCap.SchemaVersion != SchemaVersionLegacy || pubCap.Capture.Source != SourceDesktop {
		t.Fatalf("legacy revision = %q / %q", pubCap.SchemaVersion, pubCap.Capture.Source)
	}
	var vpAfter CaptureState
	e.do(&humanP, "GET", "/api/v1/captures/"+vp.CaptureID, nil).want(200).into(&vpAfter)
	if vpAfter.Source != SourceViewport || vpAfter.Provenance != ProvRuntimeCDP || vpAfter.RuntimeGeneration != "gen-7" ||
		vpAfter.Page == nil || vpAfter.Page.BrowserVersion != "Chromium 130" || vpAfter.TargetID == nil || *vpAfter.TargetID != "target-1" {
		t.Fatalf("page capture after migration = %+v", vpAfter)
	}
	pfVP := e.publish(humanP, vd)
	if pfVP.Capture.Source != SourceViewport || pfVP.SchemaVersion != SchemaVersionLegacy {
		t.Fatalf("published page capture = %+v", pfVP)
	}
	e.do(&humanP, "POST", base+"/revisions", nil).want(201)
	if !bytes.Equal(spareBefore, e.do(&humanP, "GET", "/api/v1/captures/"+spare+"/image", nil).want(200).body()) {
		t.Fatal("spare capture image changed across the migration")
	}

	// New drawings now work, and the database itself keeps them free of runtime context.
	st := e.drawing(humanP, 128, 96)
	requireWhite(t, e.do(&humanP, "GET", "/api/v1/captures/"+st.CaptureID+"/image", nil).want(200).body(), 128, 96)
	db := e.st.DB.SQL()
	ok := func(id, src, prov, cons, gen string, target, nav, lease, prior, page any) error {
		_, err := db.Exec(insertCaptureSQL, id, src, prov, cons, gen, target, nav, lease, prior, page)
		return err
	}
	if err := ok("cap_ok_drawing", SourceDrawing, ProvGenerated, ConsUnknown, "", nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("valid drawing row rejected: %v", err)
	}
	for name, err := range map[string]error{
		"drawing with client provenance":  ok("c1", SourceDrawing, ProvClientFB, ConsUnknown, "", nil, nil, nil, nil, nil),
		"drawing with cdp provenance":     ok("c2", SourceDrawing, ProvRuntimeCDP, ConsUnknown, "", nil, nil, nil, nil, nil),
		"desktop with canvas provenance":  ok("c3", SourceDesktop, ProvGenerated, ConsUnknown, "g", nil, nil, nil, nil, nil),
		"viewport with canvas provenance": ok("c4", SourceViewport, ProvGenerated, ConsStable, "g", "t", nil, nil, nil, "{}"),
		"drawing with a runtime":          ok("c5", SourceDrawing, ProvGenerated, ConsUnknown, "gen-1", nil, nil, nil, nil, nil),
		"drawing with a target":           ok("c6", SourceDrawing, ProvGenerated, ConsUnknown, "", "t", nil, nil, nil, nil),
		"drawing with a navigation":       ok("c7", SourceDrawing, ProvGenerated, ConsUnknown, "", nil, "n", nil, nil, nil),
		"drawing with a lease":            ok("c8", SourceDrawing, ProvGenerated, ConsUnknown, "", nil, nil, "l", nil, nil),
		"drawing with a prior lease":      ok("c9", SourceDrawing, ProvGenerated, ConsUnknown, "", nil, nil, nil, "l", nil),
		"drawing with a page":             ok("c10", SourceDrawing, ProvGenerated, ConsUnknown, "", nil, nil, nil, nil, "{}"),
		"drawing with page consistency":   ok("c11", SourceDrawing, ProvGenerated, ConsStable, "", nil, nil, nil, nil, nil),
		"unknown source":                  ok("c12", "canvas", ProvGenerated, ConsUnknown, "", nil, nil, nil, nil, nil),
		"unknown provenance":              ok("c13", SourceDesktop, "hand-drawn", ConsUnknown, "g", nil, nil, nil, nil, nil),
	} {
		if err == nil {
			t.Errorf("%s was accepted by the database", name)
		}
	}
	if err := ok("c14", SourceDesktop, ProvClientFB, ConsUnknown, "g", nil, nil, nil, nil, nil); err != nil {
		t.Errorf("legacy desktop row rejected: %v", err)
	}
}
