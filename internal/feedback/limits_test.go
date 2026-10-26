package feedback

import (
	"context"
	"encoding/binary"
	"errors"
	"image"
	"strings"
	"testing"
	"time"
)

// paddedPatch is a valid PATCH body of exactly size bytes: the JSON stays
// small, the rest is insignificant whitespace.
func paddedPatch(size int) []byte {
	const head, tail = `{"annotations":[]`, `}`
	return []byte(head + strings.Repeat(" ", size-len(head)-len(tail)) + tail)
}

// headerOnlyPNG is a PNG signature and IHDR (8-bit RGBA) with no image data:
// enough for DecodeConfig, never decodable.
func headerOnlyPNG(w, h uint32) []byte {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], w)
	binary.BigEndian.PutUint32(ihdr[4:8], h)
	ihdr[8], ihdr[9] = 8, 6
	chunk := []byte{0, 0, 0, 13, 'I', 'H', 'D', 'R'}
	chunk = append(chunk, ihdr...)
	var crc [4]byte
	binary.BigEndian.PutUint32(crc[:], crc32PNG(chunk[4:]))
	return append(append([]byte{}, pngSignature...), append(chunk, crc[:]...)...)
}

func TestNoUsageLimitsByDefault(t *testing.T) {
	e := newEnv(t)

	// 33000 pixels wide: above the former 32768 per-side cap, still a small file.
	wide := encode(t, image.NewRGBA(image.Rect(0, 0, 33000, 1)))
	var reserved, st CaptureState
	e.do(&humanP, "POST", "/api/v1/workspaces/ws1/captures", map[string]any{"source": "desktop"}).want(202).into(&reserved)
	id := reserved.CaptureID
	e.png(&humanP, "PUT", "/api/v1/captures/"+id+"/image", wide).want(200).into(&st)
	if st.ImageWidthPx != 33000 || st.ImageHeightPx != 1 {
		t.Fatalf("stored size = %dx%d", st.ImageWidthPx, st.ImageHeightPx)
	}
	if st.ExpiresAt != nil {
		t.Fatalf("a ready capture expires at %v without a configured draft TTL", st.ExpiresAt)
	}

	// A JSON body of 9 MiB: above the former 8 MiB cap.
	d := e.draft(humanP, id)
	var patched Draft
	e.do(&humanP, "PATCH", "/api/v1/feedback/"+d.FeedbackID, paddedPatch(9<<20), "Content-Type", "application/json", "If-Match", quote(d.ETag)).want(200).into(&patched)
	if patched.ETag == d.ETag {
		t.Fatal("the large body was accepted but not applied")
	}

	// More open drafts and captures than the former 50 and 100.
	var last string
	for i := range 105 {
		last = e.capture(humanP, secretPNG(t))
		if i < 55 {
			e.draft(humanP, last)
		}
	}
	stats, err := e.svc.Stats(ctxFor(humanP), "ws1")
	if err != nil {
		t.Fatal(err)
	}
	if stats.OpenDrafts != 56 || stats.Captures.Ready < 106 {
		t.Fatalf("drafts = %d, ready captures = %d", stats.OpenDrafts, stats.Captures.Ready)
	}
}

func TestDraftsAndCapturesNeverExpireByDefault(t *testing.T) {
	e := newEnv(t)
	bound := e.capture(humanP, secretPNG(t))
	d := e.draft(humanP, bound)
	e.annotated(humanP, d, redactAnnotation("r1"))
	loose := e.capture(humanP, secretPNG(t))

	e.clock.Advance(30 * 24 * time.Hour)
	if err := e.svc.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := e.do(&humanP, "GET", "/api/v1/feedback/"+d.FeedbackID+"/draft", nil).want(200)
	if strings.Contains(string(r.body()), "expiresAt") || strings.Contains(string(r.body()), WarnExpiring) {
		t.Fatalf("draft advertises an expiry: %s", r.body())
	}
	e.do(&humanP, "GET", "/api/v1/captures/"+loose+"/image", nil).want(200)
	if e.privateFiles() != 3 {
		t.Fatalf("private files = %d, want 3 (capture, preview, loose capture)", e.privateFiles())
	}
	evs, err := e.svc.ListEvents(ctxFor(humanP), "ws1", 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Action == "draft.expiring" || ev.Action == "draft.expired" {
			t.Fatalf("unexpected retention event %s", ev.Action)
		}
	}
}

func TestExpiriesFromAnEarlierTTLAreReleasedWithoutOne(t *testing.T) {
	e := newEnv(t)
	bound := e.capture(humanP, secretPNG(t))
	d := e.draft(humanP, bound)
	loose := e.capture(humanP, secretPNG(t))
	// Rows written under the old 24 hour default carry a finite expiry.
	past := e.clock.Now().Add(-time.Hour).UnixMilli()
	for _, q := range []string{`UPDATE revisions SET expires_at = ?`, `UPDATE captures SET expires_at = ?`} {
		if _, err := e.svc.db().Exec(q, past); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.svc.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.do(&humanP, "GET", "/api/v1/feedback/"+d.FeedbackID+"/draft", nil).want(200)
	e.do(&humanP, "GET", "/api/v1/captures/"+loose+"/image", nil).want(200)
}

func TestDraftTTLAppliesToDraftsThatNeverExpired(t *testing.T) {
	e := newEnv(t)
	bound := e.capture(humanP, secretPNG(t))
	d := e.draft(humanP, bound)

	e.svc.lim.DraftTTLSeconds = 3600
	if err := e.svc.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	var dd DraftDetail
	e.do(&humanP, "GET", "/api/v1/feedback/"+d.FeedbackID+"/draft", nil).want(200).into(&dd)
	if dd.ExpiresAt == nil || !dd.ExpiresAt.Equal(e.clock.Now().Add(time.Hour)) {
		t.Fatalf("expiresAt = %v, want one hour from now", dd.ExpiresAt)
	}
	e.clock.Advance(2 * time.Hour)
	if err := e.svc.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.do(&humanP, "GET", "/api/v1/feedback/"+d.FeedbackID+"/draft", nil).wantErr(404, CodeNotFound)
}

func TestConfiguredImageBytesLimitRefusesJustAboveTheBoundary(t *testing.T) {
	data := secretPNG(t)
	reserve := func(e *env) string {
		var st CaptureState
		e.do(&humanP, "POST", "/api/v1/workspaces/ws1/captures", map[string]any{"source": "desktop"}).want(202).into(&st)
		return st.CaptureID
	}

	tight := newEnv(t, func(o *Options) { o.Limits.MaxImageBytes = int64(len(data)) - 1 })
	tight.png(&humanP, "PUT", "/api/v1/captures/"+reserve(tight)+"/image", data).wantErr(413, CodeBodyTooLarge)
	if tight.privateFiles() != 0 {
		t.Fatalf("private files = %d after a refused upload", tight.privateFiles())
	}

	roomy := newEnv(t, func(o *Options) { o.Limits.MaxImageBytes = 10 * int64(len(data)) })
	roomy.png(&humanP, "PUT", "/api/v1/captures/"+reserve(roomy)+"/image", data).want(200)
}

func TestConfiguredJSONBodyLimitIsAnExactBoundary(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Limits.MaxJSONBody = 1000 })
	d := e.draft(humanP, e.capture(humanP, secretPNG(t)))
	patch := func(size int) resp {
		return e.do(&humanP, "PATCH", "/api/v1/feedback/"+d.FeedbackID, paddedPatch(size), "Content-Type", "application/json", "If-Match", quote(d.ETag))
	}
	patch(1001).wantErr(413, CodeBodyTooLarge)
	patch(1000).want(200)
}

func TestConfiguredSharedBytesLimitRefusesPublication(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Limits.MaxSharedBytes = 1 })
	d := e.draft(humanP, e.capture(humanP, secretPNG(t)))
	d = e.annotated(humanP, d, redactAnnotation("r1"))
	e.do(&humanP, "POST", "/api/v1/feedback/"+d.FeedbackID+"/publish", map[string]any{"humanApproved": true}, "If-Match", quote(d.ETag)).wantErr(429, CodeQuotaExceeded)
}

func TestDecodeRefusesPixelCountsThatOverflowTheBuffer(t *testing.T) {
	// The standard PNG decoder may reject the dimensions before our RGBA
	// allocation guard. Either image error must reject the unrepresentable size.
	_, err := decodePNG(headerOnlyPNG(1<<31-1, 1<<31-1), Limits{})
	if !errors.Is(err, ErrImageTooLarge) && !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("overflowing dimensions were not rejected: %v", err)
	}
	if _, err := decodePNG(headerOnlyPNG(0, 5), Limits{}); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("zero width: %v", err)
	}
}

func TestDrawingRejectsMultiplicationOverflow(t *testing.T) {
	e := newEnv(t)
	e.do(&humanP, "POST", "/api/v1/workspaces/ws1/captures", drawingBody(1<<52, 4096)).wantErr(413, CodeImageTooLarge)
	if e.privateFiles() != 0 {
		t.Fatal("overflowing drawing left an image behind")
	}
}

func TestLimitsValidationAndEnvironment(t *testing.T) {
	if err := (Limits{MaxPixels: -1}).validate(); err == nil {
		t.Fatal("negative limit accepted")
	}
	if err := (Limits{DraftTTLSeconds: 1 << 62}).validate(); err == nil {
		t.Fatal("overflowing draft TTL accepted")
	}
	if err := (Limits{}).validate(); err != nil {
		t.Fatalf("zero limits refused: %v", err)
	}

	env := map[string]string{EnvMaxImageBytes: "1048576", EnvMaxDraftsPerWorkspace: "7", EnvDraftTTLSeconds: "3600", EnvMaxPixels: ""}
	got, err := LimitsFromEnv(Limits{MaxPixels: 99, MaxSharedBytes: 5}, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	want := Limits{MaxImageBytes: 1 << 20, MaxDraftsPerWorkspace: 7, DraftTTLSeconds: 3600, MaxPixels: 99, MaxSharedBytes: 5}
	if got != want {
		t.Fatalf("limits = %+v, want %+v", got, want)
	}
	cleared, err := LimitsFromEnv(Limits{MaxPixels: 99}, func(k string) string {
		if k == EnvMaxPixels {
			return "0"
		}
		return ""
	})
	if err != nil || cleared.MaxPixels != 0 {
		t.Fatalf("explicit 0 must remove a configured limit: %+v, %v", cleared, err)
	}
	for _, bad := range []string{"-1", "many", "1.5"} {
		if _, err := LimitsFromEnv(Limits{}, func(k string) string {
			if k == EnvMaxDimension {
				return bad
			}
			return ""
		}); err == nil || !strings.Contains(err.Error(), EnvMaxDimension) {
			t.Errorf("value %q: err = %v", bad, err)
		}
	}
}
