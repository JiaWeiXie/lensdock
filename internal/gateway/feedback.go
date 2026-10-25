package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/capture"
	"github.com/JiaWeiXie/lensdock/internal/feedback"
	"github.com/JiaWeiXie/lensdock/internal/storage"
)

func principal(p Identity) feedback.Principal {
	return feedback.Principal{ID: p.ID, Role: p.Role, WorkspaceID: p.WorkspaceID}
}
func (g *Gateway) OpenFeedback(ctx context.Context) (func() error, error) {
	st, err := storage.Open(ctx, storage.Config{DBPath: filepath.Join(g.config.DataDir, "db", "lensdock.db"), PrivateDir: filepath.Join(g.config.DataDir, "private"), ArtifactDir: filepath.Join(g.config.DataDir, "artifacts")})
	if err != nil {
		return nil, err
	}
	s, err := feedback.New(ctx, st, feedback.Options{WorkspaceExists: func(id string) bool { return g.runtimes[id] != nil }, Agents: func(ws string) feedback.AgentRoster { return agentRoster(g.config, ws) }, ViewportCapturer: g.captureViewport, RuntimeGeneration: g.runtimeGeneration})
	if err != nil {
		st.Close()
		return nil, err
	}
	versions, err := storage.SchemaVersions(ctx, st.DB)
	if err != nil {
		s.Close()
		st.Close()
		return nil, err
	}
	g.schema = versions
	handler := feedback.NewHandler(s)
	events := feedback.NewEventsHandler(s)
	g.integrations = Integrations{
		FeedbackStats: func(ctx context.Context, p Identity) (feedback.Stats, error) {
			return s.Stats(feedback.WithPrincipal(ctx, principal(p)), p.WorkspaceID)
		},
		Feedback: func(w http.ResponseWriter, r *http.Request, p Identity) {
			r = r.WithContext(feedback.WithPrincipal(r.Context(), principal(p)))
			switch r.URL.Path {
			case "/api/v1/events":
				// A subscription ends only when its client leaves, which would pin
				// http.Server.Shutdown to its deadline. Bound to the stream group it
				// ends on drain, and storage stays open until this handler returns.
				bound, release, ok := g.streams.admit(w, r)
				if !ok {
					return
				}
				defer release()
				events.ServeHTTP(w, bound)
			case "/api/v1/audit":
				events.ServeHTTP(w, r)
			default:
				handler.ServeHTTP(w, r)
			}
		},
		BeforeAgentGrant: func(ctx context.Context, p Identity, id string, revision int) (int, error) {
			return s.AuthorizeAgentLease(feedback.WithPrincipal(ctx, principal(p)), id, revision)
		},
		AgentGranted: func(ctx context.Context, p Identity, id string, revision int, l apiwire.Lease) error {
			_, err := s.BeginWork(feedback.WithPrincipal(ctx, principal(p)), id, revision, l.LeaseID)
			return err
		},
		Audit: func(ctx context.Context, p Identity, action, subject string) error {
			category, result := "lease", "ok"
			if action == "access.denied" {
				category, result = "auth", "denied"
			}
			return s.RecordEvent(feedback.WithPrincipal(ctx, principal(p)), feedback.AuditInput{WorkspaceID: p.WorkspaceID, Category: category, Action: action, Result: result, SubjectType: "workspace", SubjectID: subject})
		},
	}
	// The sweep reads and writes the database and object directories, so it
	// must have returned before the closer below releases them, even when the
	// server stopped without a signal.
	sweepCtx, stopSweep := context.WithCancel(ctx)
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		s.RunJanitor(sweepCtx, time.Minute)
	}()
	return func() error {
		stopSweep()
		<-sweepDone
		a := s.Close()
		b := st.Close()
		return errors.Join(a, b)
	}, nil
}
func (g *Gateway) runtimeGeneration(ws string) string {
	e := g.runtimes[ws]
	if e == nil {
		return "unknown"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	status, _, b, err := e.call(ctx, "GET", "/api/v1/workspaces/"+ws, e.operator, "", nil, nil)
	if err != nil || status != 200 {
		return "unknown"
	}
	var state apiwire.Workspace
	if json.Unmarshal(b, &state) != nil {
		return "unknown"
	}
	return state.RuntimeGeneration
}

func (g *Gateway) captureViewport(ctx context.Context, req feedback.ViewportRequest) (feedback.ViewportIngest, error) {
	p, ws, targetID, leaseID := req.Principal, req.WorkspaceID, req.TargetID, req.LeaseID
	var out feedback.ViewportIngest
	actor := Identity{ID: p.ID, Role: p.Role, WorkspaceID: p.WorkspaceID}
	e := g.runtimes[ws]
	if e == nil || actor.WorkspaceID != ws || !human(actor) {
		return out, errors.New("capture workspace unauthorized")
	}
	allow := req.AllowInterrupt
	if leaseID != "" && !allow {
		held, ok := g.leases.forActor(actor)
		if !ok || held.Grant.Lease.LeaseID != leaseID || held.Grant.Lease.Mode != "HUMAN" {
			return out, &feedback.Error{Status: 409, Code: "LEASE_CONFLICT", Message: "capture requires the owned human lease or explicit interruption consent"}
		}
		allow = true
	}
	body, _ := json.Marshal(map[string]any{"targetId": targetID, "allowInterrupt": allow, "expectedLeaseId": leaseID, "activate": false})
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	key, err := randomToken()
	if err != nil {
		return out, err
	}
	headers.Set("Idempotency-Key", key)
	path := "/api/v1/workspaces/" + ws + "/captures/viewport"
	status, _, b, err := e.call(ctx, "POST", path, e.operator, actor.ID, body, headers)
	if err != nil {
		return out, &feedback.Error{Status: 502, Code: "UPSTREAM_UNAVAILABLE", Message: "runtime capture unavailable"}
	}
	if status != 201 {
		var problem struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &problem)
		if status < 400 || status > 599 {
			status = 502
		}
		if problem.Code == "" {
			problem.Code = "CAPTURE_FAILED"
			problem.Message = "runtime refused capture"
		}
		return out, &feedback.Error{Status: status, Code: problem.Code, Message: problem.Message}
	}
	var meta capture.Result
	if err := json.Unmarshal(b, &meta); err != nil || meta.CaptureID == "" {
		return out, errors.New("invalid runtime capture metadata")
	}
	var restored *apiwire.Lease
	if err := json.Unmarshal(meta.Control.RestoredLease, &restored); err != nil {
		return out, errors.New("invalid capture restoration metadata")
	}
	if restored != nil {
		owner := Identity{ID: restored.HolderID, WorkspaceID: ws}
		if held, ok := g.leases.forActor(owner); ok {
			held.Grant.Lease = *restored
			g.leases.put(held)
		}
	}
	imagePath := path + "/" + url.PathEscape(meta.CaptureID) + "/image"
	status, _, png, err := e.call(ctx, "GET", imagePath, e.operator, actor.ID, nil, nil)
	if err != nil || status != 200 {
		return out, errors.New("runtime capture image unavailable")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _, _, _ = e.call(cleanup, "DELETE", imagePath, e.operator, actor.ID, nil, nil)
	}()
	out = feedback.ViewportIngest{PNG: png, ExpectedSHA256: meta.Image.SHA256, TargetID: meta.TargetID, NavigationID: meta.NavigationID, Consistency: meta.Consistency, RuntimeGeneration: g.runtimeGeneration(ws), PriorLeaseID: leaseID, CapturedAt: meta.CapturedAt, Page: feedback.PageInfo{URLSanitized: meta.Post.URL, TitleSanitized: meta.Post.Title, ViewportWidthCSS: meta.Post.Viewport.CSSWidth, ViewportHeightCSS: meta.Post.Viewport.CSSHeight, PixelsPerCSSX: meta.Scale.PixelsPerCSSX, PixelsPerCSSY: meta.Scale.PixelsPerCSSY, ScrollXCSS: meta.Post.Scroll.X, ScrollYCSS: meta.Post.Scroll.Y, BrowserVersion: meta.BrowserVersion}}
	if restored != nil {
		out.LeaseID = restored.LeaseID
	}
	return out, nil
}
