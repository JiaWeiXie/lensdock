package feedback

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStatsProjectionAndArtifactFailures(t *testing.T) {
	e := newEnv(t)
	_, pf := publishedFlow(t, e)
	base := "/api/v1/feedback/" + pf.FeedbackID
	e.do(&humanP, "POST", base+"/dispatch", map[string]any{"revision": 1, "agentId": "agent-1"}).want(202)

	st, err := e.svc.Stats(ctxFor(humanP), "ws1")
	if err != nil {
		t.Fatal(err)
	}
	if st.QueueDepth != 1 || st.Deliveries[StateQueued] != 1 || st.Deliveries[StateReady] != 0 || st.OldestQueuedAt == nil || st.OpenDrafts != 0 {
		t.Fatalf("stats after dispatch = %+v", st)
	}
	if st.PrivateBytes != 0 || st.SharedBytes == 0 || st.Limits.MaxDrafts != 50 || st.ArtifactFailures != 0 {
		t.Fatalf("stats bytes/limits = %+v", st)
	}
	if st.CaptureLatency.Count != 1 {
		t.Fatalf("capture latency = %+v", st.CaptureLatency)
	}

	e.clock.Advance(0)
	e.do(&agentP, "POST", base+"/ack", map[string]any{"revision": 1}).want(200)
	st, err = e.svc.Stats(ctxFor(viewerP), "ws1")
	if err != nil {
		t.Fatal(err)
	}
	if st.QueueDepth != 0 || st.Deliveries[StateAcknowledged] != 1 || st.AckLatency.Count != 1 {
		t.Fatalf("stats after ack = %+v", st)
	}

	// Agents and foreign principals get nothing.
	if _, err := e.svc.Stats(ctxFor(agentP), "ws1"); err == nil {
		t.Fatal("agent read stats")
	}
	if _, err := e.svc.Stats(ctxFor(foreignP), "ws1"); err == nil {
		t.Fatal("foreign workspace read stats")
	}
	if _, err := e.svc.Stats(ctxFor(humanP), ""); err == nil {
		t.Fatal("a scoped principal read all workspaces")
	}
	if all, err := e.svc.Stats(ctxFor(adminP), ""); err != nil || all.Deliveries[StateAcknowledged] != 1 {
		t.Fatalf("admin all = %+v, %v", all, err)
	}

	// A tampered artifact is detected, refused and counted.
	path := filepath.Join(e.cfg.ArtifactDir, pf.FeedbackID, "r1", ArtifactRedacted)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.do(&humanP, "GET", base+"/artifacts/"+ArtifactRedacted+"?revision=1", nil).wantErr(500, CodeInternal)
	st, err = e.svc.Stats(ctxFor(humanP), "ws1")
	if err != nil || st.ArtifactFailures != 1 {
		t.Fatalf("artifact failures = %+v, %v", st, err)
	}
}

// Crops and revision bases are derived from an uploaded desktop capture and
// inherit its server_received_at with a newer created_at. They must not be
// counted as desktop captures, and workspace scoping must be unchanged.
func TestStatsDesktopCaptureLatencyIgnoresDerivedCaptures(t *testing.T) {
	e := newEnv(t)
	upload := func(p Principal, wait time.Duration) string {
		t.Helper()
		var st CaptureState
		e.do(&p, "POST", "/api/v1/workspaces/"+p.WorkspaceID+"/captures", map[string]any{"source": "desktop"}).want(202).into(&st)
		e.clock.Advance(wait)
		e.png(&p, "PUT", "/api/v1/captures/"+st.CaptureID+"/image", secretPNG(t)).want(200).into(&st)
		return st.CaptureID
	}
	capID := upload(humanP, 3*time.Second)
	upload(foreignP, 5*time.Second)

	e.clock.Advance(7 * time.Second)
	var crop CaptureState
	e.do(&humanP, "POST", "/api/v1/captures/"+capID+"/crop", map[string]any{"x": 40, "y": 30, "width": 60, "height": 40}).want(201).into(&crop)
	if crop.ParentTransform == nil || crop.ParentTransform.Kind != TransformCrop {
		t.Fatalf("crop = %+v", crop)
	}

	d := e.draft(humanP, capID)
	d = e.annotated(humanP, d, rectAnnotation("a1"), redactAnnotation("r1"))
	pf := e.publish(humanP, d)
	e.clock.Advance(11 * time.Second)
	var rev Draft
	e.do(&humanP, "POST", "/api/v1/feedback/"+pf.FeedbackID+"/revisions", nil).want(201).into(&rev)
	var dd DraftDetail
	e.do(&humanP, "GET", "/api/v1/feedback/"+pf.FeedbackID+"/draft", nil).want(200).into(&dd)
	var base CaptureState
	e.do(&humanP, "GET", "/api/v1/captures/"+dd.CaptureID, nil).want(200).into(&base)
	if base.ParentTransform == nil || base.ParentTransform.Kind != TransformRevision {
		t.Fatalf("revision base = %+v", base)
	}

	want := func(who string, p Principal, ws string, count int, avg, max int64) {
		t.Helper()
		st, err := e.svc.Stats(ctxFor(p), ws)
		if err != nil {
			t.Fatalf("%s: %v", who, err)
		}
		got := st.CaptureLatency
		if got.Count != count || got.AvgMS != avg || got.MaxMS != max {
			t.Fatalf("%s desktop capture latency = %+v, want n=%d avg=%d max=%d", who, got, count, avg, max)
		}
	}
	want("human ws1", humanP, "ws1", 1, 3000, 3000)
	want("viewer ws1", viewerP, "ws1", 1, 3000, 3000)
	want("foreign ws2", foreignP, "ws2", 1, 5000, 5000)
	want("admin all", adminP, "", 2, 4000, 5000)
}
