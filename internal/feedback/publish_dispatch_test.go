package feedback

import (
	"strings"
	"testing"
)

// rosterEnv serves a mutable roster for ws1 and a distinct agent for ws2.
func rosterEnv(t *testing.T, ws1 *AgentRoster) *env {
	t.Helper()
	return newEnv(t, func(o *Options) {
		o.Agents = func(ws string) AgentRoster {
			switch ws {
			case "ws1":
				return *ws1
			case "ws2":
				return AgentRoster{AgentIDs: []string{"agent-3"}}
			}
			return AgentRoster{}
		}
	})
}

func annotatedDraft(t *testing.T, e *env) Draft {
	t.Helper()
	d := e.draft(humanP, e.capture(humanP, secretPNG(t)))
	return e.annotated(humanP, d, rectAnnotation("a1"), redactAnnotation("r1"))
}

func sendBody(dispatch bool) map[string]any {
	return map[string]any{"humanApproved": true, "dispatch": dispatch}
}

func TestPublishWithDispatchQueuesAtomicallyForTheConfiguredAgent(t *testing.T) {
	r := AgentRoster{AgentIDs: []string{"agent-1"}}
	e := rosterEnv(t, &r)
	d := annotatedDraft(t, e)
	base := "/api/v1/feedback/" + d.FeedbackID

	var pf PublishedFeedback
	e.do(&humanP, "POST", base+"/publish", sendBody(true), "If-Match", quote(d.ETag)).want(201).into(&pf)
	if pf.Revision != 1 || pf.FeedbackID != d.FeedbackID || !pf.SharePolicy.HumanApproved || pf.SharePolicy.IncludesRawImage {
		t.Fatalf("published = %+v", pf)
	}

	var detail DeliveryDetail
	e.do(&humanP, "GET", base+"/delivery?revision=1", nil).want(200).into(&detail)
	if detail.State != StateQueued || detail.AgentID != "agent-1" || detail.QueuedAt == nil || len(detail.History) != 2 {
		t.Fatalf("delivery = %+v", detail)
	}
	h := detail.History
	if h[0].From != "DRAFT" || h[0].To != StateReady || h[1].From != StateReady || h[1].To != StateQueued || h[1].ActorID != humanP.ID {
		t.Fatalf("history = %+v", h)
	}

	// The assigned agent reads exactly this revision and its artifacts; nobody else does.
	var got PublishedFeedback
	e.do(&agentP, "GET", base+"?revision=1", nil).want(200).into(&got)
	for _, a := range got.Artifacts {
		if body := e.do(&agentP, "GET", base+"/artifacts/"+a.Name+"?revision=1", nil).want(200).body(); sha256Hex(body) != a.SHA256 {
			t.Fatalf("artifact %s hash mismatch", a.Name)
		}
	}
	var list FeedbackList
	e.do(&agentP, "GET", "/api/v1/feedback?workspaceId=ws1", nil).want(200).into(&list)
	if len(list.Items) != 1 || list.Items[0].DeliveryState != StateQueued {
		t.Fatalf("agent list = %+v", list)
	}
	e.do(&agent2P, "GET", base+"?revision=1", nil).wantErr(404, CodeNotFound)
	e.do(&agentP, "GET", "/api/v1/captures/"+got.Capture.ID+"/image", nil).wantErr(403, CodeForbidden)

	// The agent's own events carry the dispatch, and the claim stops at QUEUED.
	evs, err := e.svc.ListEvents(ctxFor(agentP), "ws1", 0, 100)
	if err != nil || len(evs) != 1 || evs[0].Action != "delivery.dispatched" {
		t.Fatalf("agent events = %+v, %v", evs, err)
	}
	if got, err := e.svc.ListEvents(ctxFor(agent2P), "ws1", 0, 100); err != nil || len(got) != 0 {
		t.Fatalf("agent-2 events = %+v, %v", got, err)
	}
	var dl Delivery
	e.do(&agentP, "POST", base+"/ack", map[string]any{"revision": 1}).want(200).into(&dl)
	if dl.State != StateAcknowledged {
		t.Fatalf("ack = %+v", dl)
	}
}

func TestPublishWithoutDispatchStaysReady(t *testing.T) {
	r := AgentRoster{AgentIDs: []string{"agent-1"}}
	e := rosterEnv(t, &r)
	d := annotatedDraft(t, e)
	base := "/api/v1/feedback/" + d.FeedbackID
	e.do(&humanP, "POST", base+"/publish", sendBody(false), "If-Match", quote(d.ETag)).want(201)
	var detail DeliveryDetail
	e.do(&humanP, "GET", base+"/delivery?revision=1", nil).want(200).into(&detail)
	if detail.State != StateReady || detail.AgentID != "" || len(detail.History) != 1 {
		t.Fatalf("delivery = %+v", detail)
	}
	e.do(&agentP, "GET", base+"?revision=1", nil).wantErr(404, CodeNotFound)
}

func TestPublishDispatchWithoutRecipientKeepsTheDraft(t *testing.T) {
	cases := []struct {
		name   string
		roster AgentRoster
		code   string
	}{
		{"no agent", AgentRoster{}, CodeAgentNotConfigured},
		{"ambiguous", AgentRoster{AgentIDs: []string{"agent-1", "agent-2"}}, CodeAgentAmbiguous},
		{"default is not an agent of the workspace", AgentRoster{AgentIDs: []string{"agent-1"}, DefaultAgentID: "agent-3"}, CodeAgentNotConfigured},
		{"default without agents", AgentRoster{DefaultAgentID: "agent-1"}, CodeAgentNotConfigured},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.roster
			e := rosterEnv(t, &r)
			d := annotatedDraft(t, e)
			base := "/api/v1/feedback/" + d.FeedbackID
			privateBefore := e.privateFiles()

			resp := e.do(&humanP, "POST", base+"/publish", sendBody(true), "If-Match", quote(d.ETag), "Idempotency-Key", "send-1").wantErr(409, tc.code)
			if msg := string(resp.body()); !strings.Contains(msg, "administrator") || strings.Contains(msg, `"agentId"`) {
				t.Fatalf("message must instruct the administrator and not ask for an AgentID: %s", msg)
			}
			// Nothing was published: still a draft, raw evidence intact, no revision.
			e.do(&humanP, "GET", base+"/draft", nil).want(200)
			e.do(&humanP, "GET", base+"?revision=1", nil).wantErr(404, CodeNotFound)
			if e.privateFiles() != privateBefore {
				t.Fatalf("private files %d -> %d", privateBefore, e.privateFiles())
			}

			// Fixing the configuration lets the very same draft ship with the very same key.
			r.AgentIDs, r.DefaultAgentID = []string{"agent-2"}, ""
			e.do(&humanP, "POST", base+"/publish", sendBody(true), "If-Match", quote(d.ETag), "Idempotency-Key", "send-1").want(201)
			var detail DeliveryDetail
			e.do(&humanP, "GET", base+"/delivery?revision=1", nil).want(200).into(&detail)
			if detail.State != StateQueued || detail.AgentID != "agent-2" {
				t.Fatalf("delivery = %+v", detail)
			}
		})
	}
}

func TestPublishDispatchUsesTheAdministrativeDefault(t *testing.T) {
	r := AgentRoster{AgentIDs: []string{"agent-1", "agent-2"}, DefaultAgentID: "agent-2"}
	e := rosterEnv(t, &r)
	d := annotatedDraft(t, e)
	base := "/api/v1/feedback/" + d.FeedbackID
	e.do(&humanP, "POST", base+"/publish", sendBody(true), "If-Match", quote(d.ETag)).want(201)
	var detail DeliveryDetail
	e.do(&humanP, "GET", base+"/delivery?revision=1", nil).want(200).into(&detail)
	if detail.State != StateQueued || detail.AgentID != "agent-2" {
		t.Fatalf("delivery = %+v", detail)
	}
	e.do(&agentP, "GET", base+"?revision=1", nil).wantErr(404, CodeNotFound)
	e.do(&agent2P, "GET", base+"?revision=1", nil).want(200)
}

func TestPublishDispatchRetryQueuesOnce(t *testing.T) {
	r := AgentRoster{AgentIDs: []string{"agent-1"}}
	e := rosterEnv(t, &r)
	d := annotatedDraft(t, e)
	base := "/api/v1/feedback/" + d.FeedbackID

	var a, b PublishedFeedback
	e.do(&humanP, "POST", base+"/publish", sendBody(true), "If-Match", quote(d.ETag), "Idempotency-Key", "send-1").want(201).into(&a)
	// A retry may even arrive after the configuration changed: it replays the stored result.
	r.AgentIDs = nil
	e.do(&humanP, "POST", base+"/publish", sendBody(true), "If-Match", quote(d.ETag), "Idempotency-Key", "send-1").want(201).into(&b)
	if a.Revision != 1 || b.Revision != 1 || !a.PublishedAt.Equal(b.PublishedAt) || a.Capture.RedactedImageSHA256 != b.Capture.RedactedImageSHA256 {
		t.Fatalf("replay differs: %+v vs %+v", a, b)
	}
	var detail DeliveryDetail
	e.do(&humanP, "GET", base+"/delivery?revision=1", nil).want(200).into(&detail)
	if detail.State != StateQueued || len(detail.History) != 2 {
		t.Fatalf("delivery = %+v", detail)
	}
	var list FeedbackList
	e.do(&humanP, "GET", "/api/v1/feedback?workspaceId=ws1", nil).want(200).into(&list)
	if len(list.Items) != 1 {
		t.Fatalf("list = %+v", list)
	}
}

func TestPublishIdempotencyKeyConflictsAcrossModes(t *testing.T) {
	r := AgentRoster{AgentIDs: []string{"agent-1"}}

	// Used as a plain publish first, then replayed as a send.
	e := rosterEnv(t, &r)
	d := annotatedDraft(t, e)
	base := "/api/v1/feedback/" + d.FeedbackID
	e.do(&humanP, "POST", base+"/publish", sendBody(false), "If-Match", quote(d.ETag), "Idempotency-Key", "k-1").want(201)
	e.do(&humanP, "POST", base+"/publish", sendBody(true), "If-Match", quote(d.ETag), "Idempotency-Key", "k-1").wantErr(409, CodeIdemKeyReused)
	var detail DeliveryDetail
	e.do(&humanP, "GET", base+"/delivery?revision=1", nil).want(200).into(&detail)
	if detail.State != StateReady || detail.AgentID != "" {
		t.Fatalf("a conflicting key must not queue: %+v", detail)
	}

	// And the other way round.
	e2 := rosterEnv(t, &r)
	d2 := annotatedDraft(t, e2)
	base2 := "/api/v1/feedback/" + d2.FeedbackID
	e2.do(&humanP, "POST", base2+"/publish", sendBody(true), "If-Match", quote(d2.ETag), "Idempotency-Key", "k-2").want(201)
	e2.do(&humanP, "POST", base2+"/publish", sendBody(false), "If-Match", quote(d2.ETag), "Idempotency-Key", "k-2").wantErr(409, CodeIdemKeyReused)
	// Omitting the flag is the legacy mode.
	e2.do(&humanP, "POST", base2+"/publish", map[string]any{"humanApproved": true}, "If-Match", quote(d2.ETag), "Idempotency-Key", "k-2").wantErr(409, CodeIdemKeyReused)
}

func TestDispatchResolvesRecipientAndScopesExplicitAgents(t *testing.T) {
	r := AgentRoster{AgentIDs: []string{"agent-1", "agent-2"}}
	e := rosterEnv(t, &r)
	_, pf := publishedFlow(t, e)
	base := "/api/v1/feedback/" + pf.FeedbackID
	rev := map[string]any{"revision": 1}

	// Several agents and no default: the server will not guess.
	e.do(&humanP, "POST", base+"/dispatch", rev).wantErr(409, CodeAgentAmbiguous)
	// An agent configured for another workspace, or unknown, is not a recipient.
	e.do(&humanP, "POST", base+"/dispatch", map[string]any{"revision": 1, "agentId": "agent-3"}).wantErr(400, CodeInvalidRequest)
	e.do(&humanP, "POST", base+"/dispatch", map[string]any{"revision": 1, "agentId": "human-1"}).wantErr(400, CodeInvalidRequest)
	var detail DeliveryDetail
	e.do(&humanP, "GET", base+"/delivery?revision=1", nil).want(200).into(&detail)
	if detail.State != StateReady || detail.AgentID != "" {
		t.Fatalf("a refused dispatch must change nothing: %+v", detail)
	}

	// An administrative default resolves an omitted recipient.
	r.DefaultAgentID = "agent-2"
	var dl Delivery
	e.do(&humanP, "POST", base+"/dispatch", rev).want(202).into(&dl)
	if dl.State != StateQueued || dl.AgentID != "agent-2" {
		t.Fatalf("dispatch = %+v", dl)
	}
	e.do(&agentP, "GET", base+"?revision=1", nil).wantErr(404, CodeNotFound)
	e.do(&agent2P, "GET", base+"?revision=1", nil).want(200)
}

func TestDispatchWithSoleAgentAndMisconfiguredDefault(t *testing.T) {
	r := AgentRoster{AgentIDs: []string{"agent-1"}}
	e := rosterEnv(t, &r)
	_, pf := publishedFlow(t, e)
	base := "/api/v1/feedback/" + pf.FeedbackID
	rev := map[string]any{"revision": 1}

	r.DefaultAgentID = "agent-3"
	e.do(&humanP, "POST", base+"/dispatch", rev).wantErr(409, CodeAgentNotConfigured)
	r.DefaultAgentID = ""
	r.AgentIDs = nil
	e.do(&humanP, "POST", base+"/dispatch", rev).wantErr(409, CodeAgentNotConfigured)
	r.AgentIDs = []string{"agent-1"}
	var dl Delivery
	e.do(&humanP, "POST", base+"/dispatch", rev).want(202).into(&dl)
	if dl.AgentID != "agent-1" || dl.State != StateQueued {
		t.Fatalf("dispatch = %+v", dl)
	}
	e.do(&agentP, "GET", base+"?revision=1", nil).want(200)
}
