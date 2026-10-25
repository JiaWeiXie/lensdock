// Package feedback implements LensDock captures, private drafts, immutable
// published revisions and the agent delivery state machine on top of
// internal/storage.
//
// Authorization is explicit: the Gateway authenticates the request and passes
// a Principal through WithPrincipal. Without a valid principal every
// operation fails with ErrUnauthenticated.
//
// Privacy rules enforced here:
//   - Raw captures and drafts are visible only to their human owner.
//   - Shared images are re-encoded and solid redactions are re-applied by the
//     server at publish time; the raw capture is deleted when a revision is
//     published.
//   - Agents read only the exact revision dispatched to them, and never raw
//     data.
//   - Published revisions and artifacts are immutable (also in the database).
package feedback

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/storage"
)

// Limits are the product limits; zero values select the defaults.
type Limits struct {
	MaxImageBytes          int64         // 32 MiB, per upload and per shared image
	MaxPixels              int           // 24,000,000
	MaxDimension           int           // 32768 per side
	MaxJSONBody            int64         // 8 MiB
	DraftTTL               time.Duration // 24h from the last modification
	ReservationTTL         time.Duration // 15m for an awaiting-upload desktop capture
	ExpiryWarning          time.Duration // 2h: DRAFT_EXPIRING warning window
	CaptureSkew            time.Duration // 30s tolerated client clock skew
	MaxDraftsPerWorkspace  int           // 50 open drafts
	MaxOpenCaptures        int           // 100 captures not yet published
	MaxPrivateBytes        int64         // 1 GiB of private images per workspace
	MaxSharedBytes         int64         // 4 GiB of published artifacts per workspace
	ImageWorkers           int           // 2 concurrent image operations
	IdempotencyRetention   time.Duration // 48h
	OrphanGrace            time.Duration // 10m before unreferenced files are deleted
	FailedCaptureRetention time.Duration // 1h
}

func (l Limits) withDefaults() Limits {
	if l.MaxImageBytes <= 0 {
		l.MaxImageBytes = defaultMaxImageBytes
	}
	if l.MaxPixels <= 0 {
		l.MaxPixels = defaultMaxPixels
	}
	if l.MaxDimension <= 0 {
		l.MaxDimension = defaultMaxDimension
	}
	if l.MaxJSONBody <= 0 {
		l.MaxJSONBody = 8 << 20
	}
	if l.DraftTTL <= 0 {
		l.DraftTTL = 24 * time.Hour
	}
	if l.ReservationTTL <= 0 {
		l.ReservationTTL = 15 * time.Minute
	}
	if l.ExpiryWarning <= 0 {
		l.ExpiryWarning = 2 * time.Hour
	}
	if l.CaptureSkew <= 0 {
		l.CaptureSkew = 30 * time.Second
	}
	if l.MaxDraftsPerWorkspace <= 0 {
		l.MaxDraftsPerWorkspace = 50
	}
	if l.MaxOpenCaptures <= 0 {
		l.MaxOpenCaptures = 100
	}
	if l.MaxPrivateBytes <= 0 {
		l.MaxPrivateBytes = 1 << 30
	}
	if l.MaxSharedBytes <= 0 {
		l.MaxSharedBytes = 4 << 30
	}
	if l.ImageWorkers <= 0 {
		l.ImageWorkers = 2
	}
	if l.IdempotencyRetention <= 0 {
		l.IdempotencyRetention = 48 * time.Hour
	}
	if l.OrphanGrace <= 0 {
		l.OrphanGrace = 10 * time.Minute
	}
	if l.FailedCaptureRetention <= 0 {
		l.FailedCaptureRetention = time.Hour
	}
	return l
}

// Options configure a Service.
type Options struct {
	Now             func() time.Time
	Logger          *slog.Logger
	WorkspaceExists func(id string) bool
	// Agents returns the administratively configured agents of a workspace.
	// Without it no agent is configured, so no revision can be dispatched.
	Agents            func(workspaceID string) AgentRoster
	RuntimeGeneration func(workspaceID string) string
	ViewportCapturer  ViewportCapturer
	Limits            Limits
}

// Service is the feedback module.
type Service struct {
	st     *storage.Store
	opt    Options
	lim    Limits
	now    func() time.Time
	log    *slog.Logger
	locks  keyedLocks
	imgSem chan struct{}
}

// Migrate applies the feedback schema to db. It touches only the database
// (no object reconciliation), so it is safe on a cloned database without the
// private and artifact directories.
func Migrate(ctx context.Context, db *storage.DB) error {
	return db.Migrate(ctx, "feedback", migrations)
}

// DatabaseSchemaVersion is the highest feedback migration version. A release
// that ships this Gateway must declare it as schema.gatewayDb in its release
// manifest (lensdock release manifest --gateway-db, GATEWAY_DB in
// tools/release/platform-evidence.sh); an older Gateway refuses a database at
// this version, so rolling back needs the pre-update snapshot.
const DatabaseSchemaVersion = 2

// New applies the feedback migrations, removes unreferenced files left by a
// crash and returns the service. The caller owns (and closes) the store.
func New(ctx context.Context, st *storage.Store, o Options) (*Service, error) {
	if st == nil {
		return nil, errors.New("feedback: nil store")
	}
	if err := Migrate(ctx, st.DB); err != nil {
		return nil, err
	}
	s := &Service{st: st, opt: o, lim: o.Limits.withDefaults(), now: o.Now, log: o.Logger}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	s.imgSem = make(chan struct{}, s.lim.ImageWorkers)
	if err := s.reconcile(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Close releases nothing today; the store is closed by its owner.
func (s *Service) Close() error { return nil }

func (s *Service) ms(t time.Time) int64 { return t.UnixMilli() }

func (s *Service) nowUTC() time.Time { return s.now().UTC().Truncate(time.Millisecond) }

func fromMS(n int64) time.Time { return time.UnixMilli(n).UTC() }

func ptrTime(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := fromMS(n.Int64)
	return &t
}

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func strPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}

func (s *Service) db() *sql.DB { return s.st.DB.SQL() }

// internal wraps an unexpected failure: the cause is logged, never returned.
func (s *Service) internal(op string, err error) error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	s.log.Error("feedback internal error", slog.String("op", op), slog.String("error", err.Error()))
	return ErrInternal
}

func (s *Service) workspaceOK(p Principal, workspaceID string) error {
	if workspaceID == "" || len(workspaceID) > 128 {
		return badRequest("workspaceId is required")
	}
	if !p.InWorkspace(workspaceID) {
		return ErrForbidden
	}
	if s.opt.WorkspaceExists != nil && !s.opt.WorkspaceExists(workspaceID) {
		return ErrNotFound
	}
	return nil
}

func (s *Service) generation(workspaceID string) string {
	if s.opt.RuntimeGeneration != nil {
		if g := s.opt.RuntimeGeneration(workspaceID); g != "" && len(g) <= 128 {
			return g
		}
	}
	return "unknown"
}

// acquireImage bounds concurrent image operations (decode memory).
func (s *Service) acquireImage(ctx context.Context) (func(), error) {
	select {
	case s.imgSem <- struct{}{}:
		return func() { <-s.imgSem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ---- idempotency -----------------------------------------------------------

type keyedLocks struct {
	mu sync.Mutex
	m  map[string]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedLocks) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyedLock{}
	}
	l := k.m[key]
	if l == nil {
		l = &keyedLock{}
		k.m[key] = l
	}
	l.refs++
	k.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}

type idemRec struct {
	scope, key, fp string
	now            time.Time
}

// save stores the response of the operation inside its transaction so state
// change and idempotency record commit together.
func (r *idemRec) save(ctx context.Context, tx *sql.Tx, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return storage.IdemStore(ctx, tx, r.scope, r.key, r.fp, b, r.now)
}

// idempotent runs fn at most once per (principal, operation, resource, key).
// Replays return the stored response; the same key with different input fails
// with ErrIdemKeyReused. Identical concurrent requests are serialised.
func idempotent[T any](ctx context.Context, s *Service, p Principal, op, resource, key string, input []byte, fn func(*idemRec) (T, error)) (T, error) {
	var zero T
	if !validIdemKey(key) {
		return zero, ErrIdemKeyRequired
	}
	scope := p.ID + "|" + op + "|" + resource
	fp := sha256Hex([]byte(op + "\x00" + resource + "\x00" + string(input)))
	unlock := s.locks.lock(scope + "\x00" + key)
	defer unlock()
	rec, err := storage.IdemLookup(ctx, s.db(), scope, key)
	if err != nil {
		return zero, s.internal(op, err)
	}
	if rec != nil {
		if rec.Fingerprint != fp {
			return zero, ErrIdemKeyReused
		}
		var out T
		if err := json.Unmarshal(rec.Response, &out); err != nil {
			return zero, s.internal(op, err)
		}
		return out, nil
	}
	return fn(&idemRec{scope: scope, key: key, fp: fp, now: s.nowUTC()})
}

// ---- events ----------------------------------------------------------------

type requestIDKey struct{}

// WithRequestID attaches a correlation ID that is stored on audit events.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

type evSpec struct {
	Workspace   string
	Category    string
	Action      string
	Result      string
	SubjectType string
	SubjectID   string
	Revision    int
	AgentID     string
	Visibility  string
	Detail      string
}

// emit appends an event through q. Call s.st.Events.Wake() after the
// surrounding transaction commits (wake does it for you).
func (s *Service) emit(ctx context.Context, q storage.Querier, p Principal, e evSpec) error {
	if e.Result == "" {
		e.Result = "ok"
	}
	_, err := s.st.Events.Append(ctx, q, storage.Event{
		WorkspaceID:   e.Workspace,
		ActorID:       p.ID,
		ActorRole:     p.Role,
		Category:      e.Category,
		Action:        e.Action,
		Result:        e.Result,
		SubjectType:   e.SubjectType,
		SubjectID:     e.SubjectID,
		Revision:      e.Revision,
		AgentID:       e.AgentID,
		Visibility:    e.Visibility,
		CorrelationID: requestIDFrom(ctx),
		Detail:        e.Detail,
	}, s.nowUTC())
	return err
}

func (s *Service) wake() { s.st.Events.Wake() }

// auditDenied records a refused access (admin-visible). It must not be called
// while a transaction is open. Failures are logged, never returned.
func (s *Service) auditDenied(ctx context.Context, p Principal, action, workspaceID, subjectType, subjectID string) {
	if workspaceID == "" {
		workspaceID = p.WorkspaceID
	}
	err := s.emit(ctx, s.db(), p, evSpec{
		Workspace: workspaceID, Category: "security", Action: action, Result: "denied",
		SubjectType: subjectType, SubjectID: subjectID, Visibility: storage.VisAdmin,
	})
	if err != nil {
		s.log.Warn("audit denied event failed", slog.String("error", err.Error()))
		return
	}
	s.wake()
}

// RecordEvent lets the Gateway append its own audit events (lease, takeover,
// runtime). Detail must not contain secrets; it is length-capped and stripped
// of control characters. Only the categories lease, runtime, auth and update
// are accepted.
func (s *Service) RecordEvent(ctx context.Context, in AuditInput) error {
	p, err := requirePrincipal(ctx)
	if err != nil {
		return err
	}
	switch in.Category {
	case "lease", "runtime", "auth", "update":
	default:
		return badRequest("unsupported audit category")
	}
	switch in.Result {
	case "ok", "denied", "error":
	default:
		return badRequest("unsupported audit result")
	}
	vis := storage.VisWorkspace
	if in.Result != "ok" {
		vis = storage.VisAdmin
	}
	err = s.emit(ctx, s.db(), p, evSpec{
		Workspace: in.WorkspaceID, Category: in.Category, Action: in.Action, Result: in.Result,
		SubjectType: orDefault(in.SubjectType, "workspace"), SubjectID: orDefault(in.SubjectID, in.WorkspaceID),
		Visibility: vis, Detail: in.Detail,
	})
	if err != nil {
		return badRequest(fmt.Sprintf("invalid audit event: %v", err))
	}
	s.wake()
	return nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// ListEvents returns events after afterSeq visible to the principal:
// administrators see everything of their workspace scope, humans and viewers
// see workspace events and their own private events, agents only events of
// deliveries assigned to them. workspaceID may be empty for an all-workspace
// admin.
func (s *Service) ListEvents(ctx context.Context, workspaceID string, afterSeq int64, limit int) ([]storage.Event, error) {
	p, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	q := storage.EventQuery{AfterSeq: afterSeq, Limit: limit, PrincipalID: p.ID, WorkspaceID: workspaceID}
	switch p.Role {
	case RoleAdmin:
		q.Scope = storage.ScopeAll
		if workspaceID == "" && p.WorkspaceID != "" && p.WorkspaceID != AllWorkspaces {
			q.WorkspaceID = p.WorkspaceID
		}
	case RoleHuman, RoleViewer:
		q.Scope = storage.ScopeMember
		q.WorkspaceID = p.WorkspaceID
	case RoleAgent:
		q.Scope = storage.ScopeAssigned
		q.WorkspaceID = p.WorkspaceID
	default:
		return nil, ErrForbidden
	}
	if workspaceID != "" && !p.InWorkspace(workspaceID) {
		return nil, ErrForbidden
	}
	if workspaceID != "" {
		q.WorkspaceID = workspaceID
	}
	return s.st.Events.Query(ctx, q)
}

// ListAudit returns every event (including denials) of a workspace; admin
// only.
func (s *Service) ListAudit(ctx context.Context, workspaceID string, afterSeq int64, limit int) ([]storage.Event, error) {
	p, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	if p.Role != RoleAdmin {
		s.auditDenied(ctx, p, "audit.read", p.WorkspaceID, "audit", "events")
		return nil, ErrForbidden
	}
	if workspaceID != "" && !p.InWorkspace(workspaceID) {
		return nil, ErrForbidden
	}
	return s.st.Events.Query(ctx, storage.EventQuery{WorkspaceID: workspaceID, AfterSeq: afterSeq, Limit: limit, Scope: storage.ScopeAll, PrincipalID: p.ID})
}

// WaitEvents blocks until an event after afterSeq may exist (or a 5 second
// poll interval passes). It returns the context error when ctx ends.
func (s *Service) WaitEvents(ctx context.Context, afterSeq int64) error {
	return s.st.Events.Wait(ctx, afterSeq)
}
