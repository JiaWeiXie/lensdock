package feedback

import (
	"context"
	"database/sql"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/storage"
)

// LatencyStats summarizes a duration over rows still in the database.
type LatencyStats struct {
	Count int   `json:"count"`
	AvgMS int64 `json:"avgMs"`
	MaxMS int64 `json:"maxMs"`
}

// CaptureStats counts captures by lifecycle status. Ready counts captures
// whose private image still exists (published captures no longer hold one).
type CaptureStats struct {
	AwaitingUpload int `json:"awaitingUpload"`
	Capturing      int `json:"capturing"`
	Ready          int `json:"ready"`
	Failed         int `json:"failed"`
}

// StatsLimits are the effective quotas next to the usage they bound.
type StatsLimits struct {
	MaxDrafts       int   `json:"maxDrafts"`
	MaxOpenCaptures int   `json:"maxOpenCaptures"`
	MaxPrivateBytes int64 `json:"maxPrivateBytes"`
	MaxSharedBytes  int64 `json:"maxSharedBytes"`
}

// Stats is the operational projection of the feedback module, computed from
// database aggregates only.
type Stats struct {
	WorkspaceID      string         `json:"workspaceId"`
	GeneratedAt      time.Time      `json:"generatedAt"`
	Deliveries       map[string]int `json:"deliveries"`
	QueueDepth       int            `json:"queueDepth"`
	OldestQueuedAt   *time.Time     `json:"oldestQueuedAt,omitempty"`
	RejectionsTotal  int            `json:"rejectionsTotal"`
	OpenDrafts       int            `json:"openDrafts"`
	Captures         CaptureStats   `json:"captures"`
	PrivateBytes     int64          `json:"privateBytes"`
	SharedBytes      int64          `json:"sharedBytes"`
	Limits           StatsLimits    `json:"limits"`
	CaptureLatency   LatencyStats   `json:"desktopCaptureLatency"`
	AckLatency       LatencyStats   `json:"ackLatency"`
	ArtifactFailures int            `json:"artifactFailures"`
	Events           int            `json:"events"`
}

// artifactFailure records an artifact write/read/integrity failure for the
// stats projection and the audit trail (admin visible, no content).
func (s *Service) artifactFailure(ctx context.Context, workspaceID, action, fbID string, rev int) {
	p, ok := PrincipalFrom(ctx)
	if !ok || !p.Valid() {
		p = Principal{ID: "system", Role: "system"}
	}
	err := s.emit(ctx, s.db(), p, evSpec{Workspace: workspaceID, Category: "artifact", Action: action, Result: "error",
		SubjectType: "feedback", SubjectID: fbID, Revision: rev, Visibility: storage.VisAdmin})
	if err == nil {
		s.wake()
	}
}

// Stats returns the projection for one workspace, or for every workspace when
// the caller is an all-workspace admin and workspaceID is empty. Humans,
// viewers and admins may read it; agents may not.
func (s *Service) Stats(ctx context.Context, workspaceID string) (Stats, error) {
	p, err := requirePrincipal(ctx)
	if err != nil {
		return Stats{}, err
	}
	if p.Role == RoleAgent {
		return Stats{}, ErrForbidden
	}
	if workspaceID == "" {
		if !(p.Role == RoleAdmin && p.InWorkspace("x")) {
			return Stats{}, badRequest("workspaceId is required")
		}
	} else if !p.InWorkspace(workspaceID) {
		return Stats{}, ErrForbidden
	}
	ws := workspaceID
	out := Stats{
		WorkspaceID: ws, GeneratedAt: s.nowUTC(), Deliveries: map[string]int{},
		Limits: StatsLimits{MaxDrafts: s.lim.MaxDraftsPerWorkspace, MaxOpenCaptures: s.lim.MaxOpenCaptures, MaxPrivateBytes: s.lim.MaxPrivateBytes, MaxSharedBytes: s.lim.MaxSharedBytes},
	}
	for _, st := range []string{StateReady, StateQueued, StateAcknowledged, StateInProgress, StateResolved, StateVerified, StateCancelled} {
		out.Deliveries[st] = 0
	}
	db := s.db()
	fail := func(err error) (Stats, error) { return Stats{}, s.internal("stats", err) }

	rows, err := db.QueryContext(ctx, `SELECT state, COUNT(*) FROM deliveries WHERE (? = '' OR workspace_id = ?) GROUP BY state`, ws, ws)
	if err != nil {
		return fail(err)
	}
	for rows.Next() {
		var (
			st string
			n  int
		)
		if err := rows.Scan(&st, &n); err != nil {
			_ = rows.Close()
			return fail(err)
		}
		out.Deliveries[st] = n
	}
	if err := rows.Close(); err != nil {
		return fail(err)
	}
	out.QueueDepth = out.Deliveries[StateQueued]

	var oldest sql.NullInt64
	var rejections sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MIN(CASE WHEN state = 'QUEUED' THEN queued_at END), SUM(rejection_count) FROM deliveries WHERE (? = '' OR workspace_id = ?)`, ws, ws).Scan(&oldest, &rejections); err != nil {
		return fail(err)
	}
	out.OldestQueuedAt = ptrTime(oldest)
	out.RejectionsTotal = int(rejections.Int64)

	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM revisions r JOIN feedback f ON f.id = r.feedback_id
		WHERE r.status = 'DRAFT' AND (? = '' OR f.workspace_id = ?)`, ws, ws).Scan(&out.OpenDrafts); err != nil {
		return fail(err)
	}

	crows, err := db.QueryContext(ctx, `SELECT status, COUNT(*) FROM captures
		WHERE (? = '' OR workspace_id = ?) AND (status != 'ready' OR object_key IS NOT NULL) GROUP BY status`, ws, ws)
	if err != nil {
		return fail(err)
	}
	for crows.Next() {
		var (
			st string
			n  int
		)
		if err := crows.Scan(&st, &n); err != nil {
			_ = crows.Close()
			return fail(err)
		}
		switch st {
		case StatusAwaiting:
			out.Captures.AwaitingUpload = n
		case StatusCapturing:
			out.Captures.Capturing = n
		case StatusReady:
			out.Captures.Ready = n
		case StatusFailed:
			out.Captures.Failed = n
		}
	}
	if err := crows.Close(); err != nil {
		return fail(err)
	}

	if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes), 0) FROM captures WHERE object_key IS NOT NULL AND (? = '' OR workspace_id = ?)`, ws, ws).Scan(&out.PrivateBytes); err != nil {
		return fail(err)
	}
	var previews int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(r.preview_size), 0) FROM revisions r JOIN feedback f ON f.id = r.feedback_id
		WHERE r.status = 'DRAFT' AND (? = '' OR f.workspace_id = ?)`, ws, ws).Scan(&previews); err != nil {
		return fail(err)
	}
	out.PrivateBytes += previews
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(a.size_bytes), 0) FROM artifacts a JOIN feedback f ON f.id = a.feedback_id WHERE (? = '' OR f.workspace_id = ?)`, ws, ws).Scan(&out.SharedBytes); err != nil {
		return fail(err)
	}

	lat := func(query string) (LatencyStats, error) {
		var (
			n        int
			avg      sql.NullFloat64
			max      sql.NullInt64
			ls       LatencyStats
			queryErr error
		)
		queryErr = db.QueryRowContext(ctx, query, ws, ws).Scan(&n, &avg, &max)
		ls.Count, ls.AvgMS, ls.MaxMS = n, int64(avg.Float64), max.Int64
		return ls, queryErr
	}
	// Desktop capture latency is reservation (created_at) to the server
	// receiving the uploaded image (server_received_at). Crops and revision
	// bases are derived rows (parent_json set): they inherit the original's
	// server_received_at but get a fresh created_at, so they are not desktop
	// captures and would yield negative, meaningless durations.
	if out.CaptureLatency, err = lat(`SELECT COUNT(*), AVG(server_received_at - created_at), MAX(server_received_at - created_at) FROM captures
		WHERE source = 'desktop' AND status = 'ready' AND server_received_at IS NOT NULL AND parent_json IS NULL AND (? = '' OR workspace_id = ?)`); err != nil {
		return fail(err)
	}
	if out.AckLatency, err = lat(`SELECT COUNT(*), AVG(acknowledged_at - queued_at), MAX(acknowledged_at - queued_at) FROM deliveries
		WHERE acknowledged_at IS NOT NULL AND queued_at IS NOT NULL AND (? = '' OR workspace_id = ?)`); err != nil {
		return fail(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE category = 'artifact' AND result = 'error' AND (? = '' OR workspace_id = ?)`, ws, ws).Scan(&out.ArtifactFailures); err != nil {
		return fail(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE (? = '' OR workspace_id = ?)`, ws, ws).Scan(&out.Events); err != nil {
		return fail(err)
	}
	return out, nil
}
