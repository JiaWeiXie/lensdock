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
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/storage"
)

// Limits are the optional, user-visible restrictions. The zero value of every
// field means no limit: arbitrary image size, dimensions, pixel counts, JSON
// body size, storage, draft and capture counts, and drafts that never expire.
// A positive value is enforced exactly; a negative value is refused by New.
type Limits struct {
	MaxImageBytes         int64 `json:"maxImageBytes,omitempty"`         // per upload and per shared image
	MaxPixels             int   `json:"maxPixels,omitempty"`             // width times height of one image
	MaxDimension          int   `json:"maxDimension,omitempty"`          // per side
	MaxJSONBody           int64 `json:"maxJsonBody,omitempty"`           // bytes of a JSON request
	DraftTTLSeconds       int64 `json:"draftTtlSeconds,omitempty"`       // lifetime from the last modification of a draft or unbound capture
	MaxDraftsPerWorkspace int   `json:"maxDraftsPerWorkspace,omitempty"` // open drafts
	MaxOpenCaptures       int   `json:"maxOpenCaptures,omitempty"`       // captures not yet published
	MaxPrivateBytes       int64 `json:"maxPrivateBytes,omitempty"`       // private image bytes per workspace
	MaxSharedBytes        int64 `json:"maxSharedBytes,omitempty"`        // published artifact bytes per workspace
	MaxEffectPixels       int64 `json:"maxEffectPixels,omitempty"`       // summed area of all blur and pixelate regions of one render
}

// Environment variables that opt in to a feedback limit; see LimitsFromEnv.
const (
	EnvMaxImageBytes         = "LENSDOCK_FEEDBACK_MAX_IMAGE_BYTES"
	EnvMaxPixels             = "LENSDOCK_FEEDBACK_MAX_PIXELS"
	EnvMaxDimension          = "LENSDOCK_FEEDBACK_MAX_DIMENSION"
	EnvMaxJSONBody           = "LENSDOCK_FEEDBACK_MAX_JSON_BYTES"
	EnvDraftTTLSeconds       = "LENSDOCK_FEEDBACK_DRAFT_TTL_SECONDS"
	EnvMaxDraftsPerWorkspace = "LENSDOCK_FEEDBACK_MAX_DRAFTS"
	EnvMaxOpenCaptures       = "LENSDOCK_FEEDBACK_MAX_OPEN_CAPTURES"
	EnvMaxPrivateBytes       = "LENSDOCK_FEEDBACK_MAX_PRIVATE_BYTES"
	EnvMaxSharedBytes        = "LENSDOCK_FEEDBACK_MAX_SHARED_BYTES"
	EnvMaxEffectPixels       = "LENSDOCK_FEEDBACK_MAX_EFFECT_PIXELS"
)

// LimitsFromEnv overlays every limit set in the environment onto base. An
// unset or empty variable keeps base; "0" explicitly removes a limit; anything
// that is not a non-negative integer is an error naming the variable.
func LimitsFromEnv(base Limits, getenv func(string) string) (Limits, error) {
	set := func(name string, dst any) error {
		v := getenv(name)
		if v == "" {
			return nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return fmt.Errorf("%s must be a non-negative integer, got %q", name, v)
		}
		switch p := dst.(type) {
		case *int64:
			*p = n
		case *int:
			if n > math.MaxInt32 {
				return fmt.Errorf("%s is too large", name)
			}
			*p = int(n)
		}
		return nil
	}
	for _, f := range []struct {
		name string
		dst  any
	}{
		{EnvMaxImageBytes, &base.MaxImageBytes}, {EnvMaxPixels, &base.MaxPixels}, {EnvMaxDimension, &base.MaxDimension},
		{EnvMaxJSONBody, &base.MaxJSONBody}, {EnvDraftTTLSeconds, &base.DraftTTLSeconds},
		{EnvMaxDraftsPerWorkspace, &base.MaxDraftsPerWorkspace}, {EnvMaxOpenCaptures, &base.MaxOpenCaptures},
		{EnvMaxPrivateBytes, &base.MaxPrivateBytes}, {EnvMaxSharedBytes, &base.MaxSharedBytes}, {EnvMaxEffectPixels, &base.MaxEffectPixels},
	} {
		if err := set(f.name, f.dst); err != nil {
			return Limits{}, err
		}
	}
	return base, nil
}

func (l Limits) validate() error {
	if l.MaxImageBytes < 0 || l.MaxPixels < 0 || l.MaxDimension < 0 || l.MaxJSONBody < 0 || l.DraftTTLSeconds < 0 ||
		l.MaxDraftsPerWorkspace < 0 || l.MaxOpenCaptures < 0 || l.MaxPrivateBytes < 0 || l.MaxSharedBytes < 0 || l.MaxEffectPixels < 0 {
		return errors.New("feedback: limits must not be negative (use 0 for no limit)")
	}
	if l.DraftTTLSeconds > math.MaxInt64/int64(time.Second) {
		return errors.New("feedback: draft TTL is too large")
	}
	return nil
}

// draftTTL is the lifetime of a draft or unbound capture; zero means never.
func (l Limits) draftTTL() time.Duration { return time.Duration(l.DraftTTLSeconds) * time.Second }

// Operations are operational protocol deadlines, retention and concurrency
// settings. They protect the service itself and are not user restrictions: a
// zero field selects the documented default.
type Operations struct {
	ReservationTTL         time.Duration // 15m for an awaiting-upload desktop capture
	ExpiryWarning          time.Duration // 2h: DRAFT_EXPIRING warning window (only with a draft TTL)
	CaptureSkew            time.Duration // 30s tolerated client clock skew
	ImageWorkers           int           // 2 concurrent image operations (decode memory)
	IdempotencyRetention   time.Duration // 48h
	OrphanGrace            time.Duration // 10m before unreferenced files are deleted
	FailedCaptureRetention time.Duration // 1h
}

func (o Operations) withDefaults() Operations {
	if o.ReservationTTL <= 0 {
		o.ReservationTTL = 15 * time.Minute
	}
	if o.ExpiryWarning <= 0 {
		o.ExpiryWarning = 2 * time.Hour
	}
	if o.CaptureSkew <= 0 {
		o.CaptureSkew = 30 * time.Second
	}
	if o.ImageWorkers <= 0 {
		o.ImageWorkers = 2
	}
	if o.IdempotencyRetention <= 0 {
		o.IdempotencyRetention = 48 * time.Hour
	}
	if o.OrphanGrace <= 0 {
		o.OrphanGrace = 10 * time.Minute
	}
	if o.FailedCaptureRetention <= 0 {
		o.FailedCaptureRetention = time.Hour
	}
	return o
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
	Operations        Operations
}

// Service is the feedback module.
type Service struct {
	st     *storage.Store
	opt    Options
	lim    Limits
	ops    Operations
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
const DatabaseSchemaVersion = 3

// New applies the feedback migrations, removes unreferenced files left by a
// crash and returns the service. The caller owns (and closes) the store.
func New(ctx context.Context, st *storage.Store, o Options) (*Service, error) {
	if st == nil {
		return nil, errors.New("feedback: nil store")
	}
	if err := Migrate(ctx, st.DB); err != nil {
		return nil, err
	}
	if err := o.Limits.validate(); err != nil {
		return nil, err
	}
	s := &Service{st: st, opt: o, lim: o.Limits, ops: o.Operations.withDefaults(), now: o.Now, log: o.Logger}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	s.imgSem = make(chan struct{}, s.ops.ImageWorkers)
	if err := s.alignDraftExpiry(ctx); err != nil {
		return nil, err
	}
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
