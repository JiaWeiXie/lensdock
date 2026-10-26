package feedback

import (
	"bytes"
	"testing"
)

// publishAs publishes one new feedback item as p and returns it.
func publishAs(t *testing.T, e *env, p Principal) PublishedFeedback {
	t.Helper()
	d := e.draft(p, e.capture(p, secretPNG(t)))
	d = e.annotated(p, d, rectAnnotation("a1"), redactAnnotation("r1"))
	return e.publish(p, d)
}

// listAll pages through GET /feedback with a deliberately tiny page size and
// returns every row it saw, oldest page last.
func listAll(t *testing.T, e *env, p Principal, query string) []FeedbackSummary {
	t.Helper()
	var all []FeedbackSummary
	cursor := ""
	for range 50 {
		url := "/api/v1/feedback?workspaceId=" + p.WorkspaceID + "&limit=2" + query
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		var l FeedbackList
		e.do(&p, "GET", url, nil).want(200).into(&l)
		all = append(all, l.Items...)
		if l.NextCursor == nil {
			return all
		}
		cursor = *l.NextCursor
	}
	t.Fatal("pagination did not terminate")
	return nil
}

func ids(items []FeedbackSummary) map[string]bool {
	m := map[string]bool{}
	for _, it := range items {
		m[it.FeedbackID] = true
	}
	return m
}

func TestClearHistorySingleAndBulkBeyondOnePage(t *testing.T) {
	e := newEnv(t)
	var pubs []PublishedFeedback
	for range 5 {
		pubs = append(pubs, publishAs(t, e, humanP))
	}
	other := publishAs(t, e, foreignP)

	// Pubs[0] is dispatched to agent-1; pubs[1] carries an open second-revision draft.
	e.do(&humanP, "POST", "/api/v1/feedback/"+pubs[0].FeedbackID+"/dispatch", map[string]any{"revision": 1, "agentId": "agent-1"}).want(202)
	e.do(&humanP, "POST", "/api/v1/feedback/"+pubs[1].FeedbackID+"/revisions", nil).want(201)

	if got := listAll(t, e, humanP, "&history=visible"); len(got) != 5 {
		t.Fatalf("visible history = %d rows, want 5", len(got))
	}

	// One revision.
	one := "/api/v1/feedback/" + pubs[2].FeedbackID + "/history/clear"
	var r ClearHistoryResult
	e.do(&humanP, "POST", one, map[string]any{"revision": 1}).want(200).into(&r)
	if r.Cleared != 1 {
		t.Fatalf("single clear = %+v", r)
	}
	e.do(&humanP, "POST", one, map[string]any{"revision": 1}).want(200).into(&r)
	if r.Cleared != 0 {
		t.Fatalf("clearing an already cleared revision reported %+v", r)
	}
	visible := listAll(t, e, humanP, "&history=visible")
	if len(visible) != 4 || ids(visible)[pubs[2].FeedbackID] {
		t.Fatalf("visible after single clear = %+v", visible)
	}
	// The default listing (CLI, MCP, evidence lookups) is unchanged.
	if got := listAll(t, e, humanP, ""); len(got) != 5 {
		t.Fatalf("default listing = %d rows, want 5", len(got))
	}

	// Clearing the dispatched revision hides it from history only: the Agent
	// queue, its read access and the delivery are exactly what they were.
	e.do(&humanP, "POST", "/api/v1/feedback/"+pubs[0].FeedbackID+"/history/clear", map[string]any{"revision": 1}).want(200).into(&r)
	if r.Cleared != 1 {
		t.Fatalf("clear of dispatched revision = %+v", r)
	}
	agentList := listAll(t, e, agentP, "")
	if len(agentList) != 1 || agentList[0].FeedbackID != pubs[0].FeedbackID || agentList[0].DeliveryState != StateQueued {
		t.Fatalf("agent list after clear = %+v", agentList)
	}
	var dl Delivery
	e.do(&agentP, "POST", "/api/v1/feedback/"+pubs[0].FeedbackID+"/ack", map[string]any{"revision": 1}).want(200).into(&dl)
	if dl.State != StateAcknowledged {
		t.Fatalf("agent ack after clear = %+v", dl)
	}

	// Bulk: every remaining published revision, beyond one page, whatever the state.
	var all ClearHistoryResult
	e.do(&humanP, "POST", "/api/v1/feedback/history/clear", map[string]any{"workspaceId": "ws1"}).want(200).into(&all)
	if all.Cleared != 3 {
		t.Fatalf("bulk clear = %+v, want the 3 still visible", all)
	}
	if got := listAll(t, e, humanP, "&history=visible"); len(got) != 0 {
		t.Fatalf("visible after bulk clear = %+v", got)
	}
	e.do(&humanP, "POST", "/api/v1/feedback/history/clear", map[string]any{"workspaceId": "ws1"}).want(200).into(&all)
	if all.Cleared != 0 {
		t.Fatalf("second bulk clear = %+v", all)
	}
	if got := listAll(t, e, humanP, ""); len(got) != 5 {
		t.Fatalf("default listing after bulk clear = %d rows, want 5", len(got))
	}
	// Another workspace is untouched.
	if got := listAll(t, e, foreignP, "&history=visible"); len(got) != 1 || got[0].FeedbackID != other.FeedbackID {
		t.Fatalf("foreign workspace history = %+v", got)
	}

	// Published evidence and the open draft are still there.
	for _, pf := range pubs {
		base := "/api/v1/feedback/" + pf.FeedbackID
		e.do(&humanP, "GET", base+"?revision=1", nil).want(200)
		e.do(&humanP, "GET", base+"/bundle?revision=1", nil).want(200)
		e.do(&humanP, "GET", base+"/artifacts/"+ArtifactRedacted+"?revision=1", nil).want(200)
		e.do(&humanP, "GET", base+"/delivery?revision=1", nil).want(200)
	}
	e.do(&humanP, "GET", "/api/v1/feedback/"+pubs[1].FeedbackID+"/draft", nil).want(200)
	var detail DeliveryDetail
	e.do(&humanP, "GET", "/api/v1/feedback/"+pubs[0].FeedbackID+"/delivery?revision=1", nil).want(200).into(&detail)
	if detail.State != StateAcknowledged {
		t.Fatalf("delivery after clear = %+v", detail)
	}

	// The clear persists across a restart, and newly published revisions are visible.
	e.restart()
	if got := listAll(t, e, humanP, "&history=visible"); len(got) != 0 {
		t.Fatalf("visible after restart = %+v", got)
	}
	fresh := publishAs(t, e, humanP)
	if got := listAll(t, e, humanP, "&history=visible"); len(got) != 1 || got[0].FeedbackID != fresh.FeedbackID {
		t.Fatalf("visible after new publication = %+v", got)
	}
	// A new revision of a cleared feedback is a new, visible history row.
	var d2 Draft
	e.do(&humanP, "POST", "/api/v1/feedback/"+pubs[3].FeedbackID+"/revisions", nil).want(201).into(&d2)
	d2 = e.annotated(humanP, d2, rectAnnotation("a2"), redactAnnotation("r2"))
	if pf2 := e.publish(humanP, d2); pf2.Revision != 2 {
		t.Fatalf("revision = %d", pf2.Revision)
	}
	visible = listAll(t, e, humanP, "&history=visible")
	if len(visible) != 2 {
		t.Fatalf("visible with a revision 2 = %+v", visible)
	}
}

func TestClearHistoryReplayDoesNotHideLaterPublications(t *testing.T) {
	e := newEnv(t)
	publishAs(t, e, humanP)
	publishAs(t, e, humanP)

	body := map[string]any{"workspaceId": "ws1"}
	var r ClearHistoryResult
	e.do(&humanP, "POST", "/api/v1/feedback/history/clear", body, "Idempotency-Key", "clear-1").want(200).into(&r)
	if r.Cleared != 2 {
		t.Fatalf("first clear = %+v", r)
	}
	later := publishAs(t, e, humanP)

	// The retry returns the stored answer and must not sweep up the new revision.
	e.do(&humanP, "POST", "/api/v1/feedback/history/clear", body, "Idempotency-Key", "clear-1").want(200).into(&r)
	if r.Cleared != 2 {
		t.Fatalf("replayed clear = %+v", r)
	}
	if got := listAll(t, e, humanP, "&history=visible"); len(got) != 1 || got[0].FeedbackID != later.FeedbackID {
		t.Fatalf("visible after replay = %+v", got)
	}
	e.do(&humanP, "POST", "/api/v1/feedback/history/clear", body, "Idempotency-Key", "").wantErr(400, CodeIdemKeyRequired)

	// Single clear replays the same way.
	url := "/api/v1/feedback/" + later.FeedbackID + "/history/clear"
	e.do(&humanP, "POST", url, map[string]any{"revision": 1}, "Idempotency-Key", "one-1").want(200).into(&r)
	if r.Cleared != 1 {
		t.Fatalf("single clear = %+v", r)
	}
	e.do(&humanP, "POST", url, map[string]any{"revision": 1}, "Idempotency-Key", "one-1").want(200).into(&r)
	if r.Cleared != 1 {
		t.Fatalf("replayed single clear = %+v", r)
	}
	e.do(&humanP, "POST", url, map[string]any{"revision": 2}, "Idempotency-Key", "one-1").wantErr(409, CodeIdemKeyReused)
}

func TestClearHistoryAuthorizationAndValidation(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.WorkspaceExists = func(ws string) bool { return ws == "ws1" || ws == "ws2" }
	})
	pf := publishAs(t, e, humanP)
	d := e.annotated(humanP, e.draft(humanP, e.capture(humanP, secretPNG(t))), rectAnnotation("a1"))
	one := "/api/v1/feedback/" + pf.FeedbackID + "/history/clear"
	const all = "/api/v1/feedback/history/clear"

	// Unauthenticated, agent and viewer are refused for both operations.
	e.do(nil, "POST", one, map[string]any{"revision": 1}).wantErr(401, CodeUnauthenticated)
	e.do(nil, "POST", all, map[string]any{"workspaceId": "ws1"}).wantErr(401, CodeUnauthenticated)
	for _, p := range []Principal{agentP, viewerP} {
		p := p
		e.do(&p, "POST", one, map[string]any{"revision": 1}).wantErr(403, CodeForbidden)
		e.do(&p, "POST", all, map[string]any{"workspaceId": "ws1"}).wantErr(403, CodeForbidden)
	}
	// Another workspace's human cannot reach ws1: the revision is not found, the workspace forbidden.
	e.do(&foreignP, "POST", one, map[string]any{"revision": 1}).wantErr(404, CodeNotFound)
	e.do(&foreignP, "POST", all, map[string]any{"workspaceId": "ws1"}).wantErr(403, CodeForbidden)
	// Unknown feedback, unpublished revision, unknown workspace.
	e.do(&humanP, "POST", "/api/v1/feedback/fb_missing/history/clear", map[string]any{"revision": 1}).wantErr(404, CodeNotFound)
	e.do(&humanP, "POST", one, map[string]any{"revision": 2}).wantErr(404, CodeNotFound)
	e.do(&humanP, "POST", "/api/v1/feedback/"+d.FeedbackID+"/history/clear", map[string]any{"revision": 1}).wantErr(404, CodeNotFound)
	e.do(&adminP, "POST", all, map[string]any{"workspaceId": "ghost"}).wantErr(404, CodeNotFound)
	// Malformed bodies.
	e.do(&humanP, "POST", one, map[string]any{}).wantErr(400, CodeInvalidRequest)
	e.do(&humanP, "POST", one, map[string]any{"revision": 0}).wantErr(400, CodeInvalidRequest)
	e.do(&humanP, "POST", one, map[string]any{"revision": 1, "extra": true}).wantErr(400, CodeInvalidRequest)
	e.do(&humanP, "POST", all, map[string]any{}).wantErr(400, CodeInvalidRequest)
	e.do(&humanP, "POST", all, map[string]any{"workspaceId": "ws1", "extra": 1}).wantErr(400, CodeInvalidRequest)
	e.do(&humanP, "GET", all, nil).wantErr(405, CodeMethodNotAllowed)
	// Nothing above cleared anything.
	if got := listAll(t, e, humanP, "&history=visible"); len(got) != 1 {
		t.Fatalf("a refused clear hid history: %+v", got)
	}

	// Invalid history filters are rejected; the default listing is unchanged.
	for _, v := range []string{"all", "hidden", "VISIBLE", "1"} {
		e.do(&humanP, "GET", "/api/v1/feedback?workspaceId=ws1&history="+v, nil).wantErr(400, CodeInvalidRequest)
	}
	e.do(&humanP, "GET", "/api/v1/feedback?workspaceId=ws1&history=", nil).want(200)

	// An administrator clears another workspace's history; only that workspace changes.
	other := publishAs(t, e, foreignP)
	var r ClearHistoryResult
	e.do(&adminP, "POST", all, map[string]any{"workspaceId": "ws2"}).want(200).into(&r)
	if r.Cleared != 1 {
		t.Fatalf("admin clear = %+v", r)
	}
	if got := listAll(t, e, foreignP, "&history=visible"); len(got) != 0 {
		t.Fatalf("ws2 visible = %+v", got)
	}
	if got := listAll(t, e, humanP, "&history=visible"); len(got) != 1 {
		t.Fatalf("ws1 visible = %+v", got)
	}
	e.do(&foreignP, "GET", "/api/v1/feedback/"+other.FeedbackID+"?revision=1", nil).want(200)

	// The clear is recorded for event-driven refresh and audit, scoped to the workspace.
	evs, err := e.svc.ListEvents(ctxFor(humanP), "ws1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Action == "history.cleared" {
			t.Fatalf("a ws1 human sees a ws2 clear event: %+v", ev)
		}
	}
	e.do(&humanP, "POST", all, map[string]any{"workspaceId": "ws1"}).want(200).into(&r)
	evs, err = e.svc.ListEvents(ctxFor(humanP), "ws1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range evs {
		found = found || ev.Action == "history.cleared"
	}
	if !found {
		t.Fatalf("no history.cleared event for ws1: %+v", evs)
	}
	if evs, err := e.svc.ListEvents(ctxFor(agentP), "ws1", 0, 100); err != nil {
		t.Fatal(err)
	} else {
		for _, ev := range evs {
			if ev.Action == "history.cleared" {
				t.Fatalf("the agent sees a history event: %+v", ev)
			}
		}
	}
}

func TestClearHistoryKeepsPublishedRowsImmutable(t *testing.T) {
	e := newEnv(t)
	pf := publishAs(t, e, humanP)
	base := "/api/v1/feedback/" + pf.FeedbackID
	before := e.do(&humanP, "GET", base+"/bundle?revision=1", nil).want(200).body()
	e.do(&humanP, "POST", base+"/history/clear", map[string]any{"revision": 1}).want(200)
	if !bytes.Equal(before, e.do(&humanP, "GET", base+"/bundle?revision=1", nil).want(200).body()) {
		t.Fatal("clearing history changed the published bundle")
	}
	var n int
	if err := e.st.DB.SQL().QueryRow(`SELECT COUNT(*) FROM revisions WHERE status = 'PUBLISHED'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("published revisions = %d, %v", n, err)
	}
	if err := e.st.DB.SQL().QueryRow(`SELECT COUNT(*) FROM artifacts`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("artifacts = %d, %v", n, err)
	}
}
