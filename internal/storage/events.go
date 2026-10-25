package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Event visibility classes. They decide who can read an event.
const (
	// VisPrivate events are visible to their actor and administrators.
	VisPrivate = "private"
	// VisWorkspace events are visible to human, viewer and admin principals of
	// the workspace.
	VisWorkspace = "workspace"
	// VisAssigned events are visible to workspace members and to the agent in
	// Event.AgentID.
	VisAssigned = "assigned"
	// VisAdmin events (access denials, security) are visible to admins only.
	VisAdmin = "admin"
)

// Event scopes for Query.
const (
	// ScopeAll reads every event of the workspace (administrators).
	ScopeAll = "all"
	// ScopeMember reads workspace and assigned events plus the principal's own
	// private events (humans, viewers).
	ScopeMember = "member"
	// ScopeAssigned reads only assigned events naming the principal (agents).
	ScopeAssigned = "assigned"
)

// Event is one audit/event record. It never carries secrets: fields are
// short identifiers and Detail is a capped, control-character-free note.
type Event struct {
	Seq           int64     `json:"seq"`
	ID            string    `json:"id"`
	Time          time.Time `json:"time"`
	WorkspaceID   string    `json:"workspaceId"`
	ActorID       string    `json:"actorId"`
	ActorRole     string    `json:"actorRole"`
	Category      string    `json:"category"`
	Action        string    `json:"action"`
	Result        string    `json:"result"`
	SubjectType   string    `json:"subjectType"`
	SubjectID     string    `json:"subjectId"`
	Revision      int       `json:"revision,omitempty"`
	AgentID       string    `json:"agentId,omitempty"`
	Visibility    string    `json:"-"`
	CorrelationID string    `json:"correlationId,omitempty"`
	Detail        string    `json:"detail,omitempty"`
}

// EventQuery selects events. Limit is clamped to 1..500.
type EventQuery struct {
	WorkspaceID string // empty = all workspaces (ScopeAll only)
	AfterSeq    int64
	Limit       int
	Scope       string
	PrincipalID string
}

// EventLog is the append-only event store with a wake-up channel for
// streaming readers.
type EventLog struct {
	db *DB
	mu sync.Mutex
	ch chan struct{}
}

// NewEventLog returns the log over db (whose storage migrations must be
// applied; Open does that).
func NewEventLog(db *DB) *EventLog {
	return &EventLog{db: db, ch: make(chan struct{})}
}

const maxDetail = 200

func newEventID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "evt_" + hex.EncodeToString(b[:]), nil
}

func validToken(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for _, r := range s {
		if r > unicode.MaxASCII || r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

func cleanDetail(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) {
			r = ' '
		}
		if n >= maxDetail {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// Append inserts e using q (normally the transaction of the state change it
// records) and returns its sequence number. Call Wake after the transaction
// commits.
func (l *EventLog) Append(ctx context.Context, q Querier, e Event, now time.Time) (int64, error) {
	if e.WorkspaceID == "" {
		e.WorkspaceID = "-"
	}
	if e.ActorID == "" {
		e.ActorID = "system"
	}
	if e.ActorRole == "" {
		e.ActorRole = "system"
	}
	for _, f := range []struct {
		name string
		v    string
		max  int
	}{
		{"workspace", e.WorkspaceID, 128}, {"actor", e.ActorID, 128}, {"role", e.ActorRole, 32},
		{"category", e.Category, 32}, {"action", e.Action, 64}, {"result", e.Result, 16},
		{"subject type", e.SubjectType, 32}, {"subject", e.SubjectID, 128},
	} {
		if !validToken(f.v, f.max) {
			return 0, fmt.Errorf("storage: invalid event %s", f.name)
		}
	}
	switch e.Visibility {
	case VisPrivate, VisWorkspace, VisAssigned, VisAdmin:
	default:
		return 0, errors.New("storage: invalid event visibility")
	}
	if e.Visibility == VisAssigned && e.AgentID == "" {
		return 0, errors.New("storage: assigned event needs an agent")
	}
	if e.AgentID != "" && !validToken(e.AgentID, 128) {
		return 0, errors.New("storage: invalid event agent")
	}
	if e.CorrelationID != "" && !validToken(e.CorrelationID, 128) {
		e.CorrelationID = ""
	}
	id, err := newEventID()
	if err != nil {
		return 0, err
	}
	res, err := q.ExecContext(ctx, `INSERT INTO events
		(id, ts, workspace_id, actor_id, actor_role, category, action, result, subject_type, subject_id, revision, agent_id, visibility, correlation_id, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, now.UnixMilli(), e.WorkspaceID, e.ActorID, e.ActorRole, e.Category, e.Action, e.Result, e.SubjectType, e.SubjectID,
		e.Revision, e.AgentID, e.Visibility, e.CorrelationID, cleanDetail(e.Detail))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Wake releases every Wait caller. Call it after a transaction that appended
// events commits.
func (l *EventLog) Wake() {
	l.mu.Lock()
	close(l.ch)
	l.ch = make(chan struct{})
	l.mu.Unlock()
}

func (l *EventLog) changed() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ch
}

// MaxSeq returns the highest sequence number, or 0.
func (l *EventLog) MaxSeq(ctx context.Context) (int64, error) {
	var n int64
	err := l.db.SQL().QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&n)
	return n, err
}

// Wait blocks until an event with a sequence above after exists, the context
// ends, or a 5 second poll interval passes (a cheap guard against a missed
// wake-up). It returns nil in the first and last cases.
func (l *EventLog) Wait(ctx context.Context, after int64) error {
	for {
		ch := l.changed()
		n, err := l.MaxSeq(ctx)
		if err != nil {
			return err
		}
		if n > after {
			return nil
		}
		t := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-ch:
			t.Stop()
		case <-t.C:
			return nil
		}
	}
}

// Query returns events in ascending sequence order, filtered by the scope.
// Denied/security events (visibility admin) only appear in ScopeAll.
func (l *EventLog) Query(ctx context.Context, q EventQuery) ([]Event, error) {
	limit := q.Limit
	if limit < 1 || limit > 500 {
		limit = 100
	}
	where := []string{"seq > ?"}
	args := []any{q.AfterSeq}
	if q.WorkspaceID != "" {
		where = append(where, "workspace_id = ?")
		args = append(args, q.WorkspaceID)
	}
	switch q.Scope {
	case ScopeAll:
	case ScopeMember:
		where = append(where, "(visibility IN ('workspace','assigned') OR (visibility = 'private' AND actor_id = ?))")
		args = append(args, q.PrincipalID)
	case ScopeAssigned:
		where = append(where, "visibility = 'assigned' AND agent_id = ?")
		args = append(args, q.PrincipalID)
	default:
		return nil, errors.New("storage: unknown event scope")
	}
	args = append(args, limit)
	rows, err := l.db.SQL().QueryContext(ctx, `SELECT seq, id, ts, workspace_id, actor_id, actor_role, category, action, result,
		subject_type, subject_id, revision, agent_id, visibility, correlation_id, detail
		FROM events WHERE `+strings.Join(where, " AND ")+` ORDER BY seq ASC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var (
			e  Event
			ms int64
		)
		if err := rows.Scan(&e.Seq, &e.ID, &ms, &e.WorkspaceID, &e.ActorID, &e.ActorRole, &e.Category, &e.Action, &e.Result,
			&e.SubjectType, &e.SubjectID, &e.Revision, &e.AgentID, &e.Visibility, &e.CorrelationID, &e.Detail); err != nil {
			return nil, err
		}
		e.Time = time.UnixMilli(ms).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}
