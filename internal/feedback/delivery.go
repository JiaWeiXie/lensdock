package feedback

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/JiaWeiXie/lensdock/internal/storage"
)

type deliveryRow struct {
	Seq                                                   int64
	FeedbackID, WorkspaceID, State, AgentID               string
	Revision                                              int
	Queued, Acked, InProg, Resolved, Verified, Cancelled  sql.NullInt64
	LeaseID, Summary, Verification, ResultIDs, LastReason string
	Rejections                                            int
	Created, Updated                                      int64
}

const deliveryCols = `seq, feedback_id, revision, workspace_id, state, agent_id, queued_at, acknowledged_at, in_progress_at, resolved_at,
	verified_at, cancelled_at, lease_id, summary, verification, result_ids_json, last_reason, rejection_count, created_at, updated_at`

func scanDelivery(sc scanner) (*deliveryRow, error) {
	var d deliveryRow
	err := sc.Scan(&d.Seq, &d.FeedbackID, &d.Revision, &d.WorkspaceID, &d.State, &d.AgentID, &d.Queued, &d.Acked, &d.InProg, &d.Resolved,
		&d.Verified, &d.Cancelled, &d.LeaseID, &d.Summary, &d.Verification, &d.ResultIDs, &d.LastReason, &d.Rejections, &d.Created, &d.Updated)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func getDelivery(ctx context.Context, q storage.Querier, fbID string, rev int) (*deliveryRow, error) {
	d, err := scanDelivery(q.QueryRowContext(ctx, `SELECT `+deliveryCols+` FROM deliveries WHERE feedback_id = ? AND revision = ?`, fbID, rev))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

func (d *deliveryRow) delivery() Delivery {
	return Delivery{FeedbackID: d.FeedbackID, Revision: d.Revision, AgentID: d.AgentID, State: d.State}
}

// agentVisible reports whether an assigned agent may still read the revision.
func agentVisible(state string) bool {
	switch state {
	case StateQueued, StateAcknowledged, StateInProgress, StateResolved:
		return true
	}
	return false
}

func validState(s string) bool {
	switch s {
	case StateReady, StateQueued, StateAcknowledged, StateInProgress, StateResolved, StateVerified, StateCancelled:
		return true
	}
	return false
}

// authRead resolves and authorizes a published revision for reading.
// Viewers, humans and admins read any published revision of their workspace;
// an agent reads only the exact revision dispatched to it while the delivery
// is active. Everything else is NOT_FOUND (existence is not confirmed) and
// audited. revision 0 selects the latest authorized revision.
func (s *Service) authRead(ctx context.Context, action, fbID string, rev int) (Principal, *revisionRow, *deliveryRow, error) {
	p, err := requirePrincipal(ctx)
	if err != nil {
		return p, nil, nil, err
	}
	if fbID == "" || len(fbID) > 128 || rev < 0 {
		return p, nil, nil, ErrNotFound
	}
	if !p.canReadPublished() && p.Role != RoleAgent {
		return p, nil, nil, ErrForbidden
	}
	if rev == 0 {
		var n sql.NullInt64
		var err error
		if p.Role == RoleAgent {
			err = s.db().QueryRowContext(ctx, `SELECT MAX(revision) FROM deliveries WHERE feedback_id = ? AND agent_id = ? AND state IN ('QUEUED','ACKNOWLEDGED','IN_PROGRESS','RESOLVED')`, fbID, p.ID).Scan(&n)
		} else {
			err = s.db().QueryRowContext(ctx, `SELECT MAX(revision) FROM revisions WHERE feedback_id = ? AND status = 'PUBLISHED'`, fbID).Scan(&n)
		}
		if err != nil {
			return p, nil, nil, s.internal(action, err)
		}
		if !n.Valid {
			return p, nil, nil, ErrNotFound
		}
		rev = int(n.Int64)
	}
	r, err := getRevision(ctx, s.db(), fbID, rev)
	if err != nil {
		return p, nil, nil, s.internalNF(action, err)
	}
	if r.Status != revisionPublished {
		return p, nil, nil, ErrNotFound
	}
	d, err := getDelivery(ctx, s.db(), fbID, rev)
	if err != nil {
		return p, nil, nil, s.internalNF(action, err)
	}
	allowed := p.InWorkspace(r.WorkspaceID)
	if allowed && p.Role == RoleAgent {
		allowed = d.AgentID == p.ID && agentVisible(d.State)
	}
	if !allowed {
		s.auditDenied(ctx, p, action, r.WorkspaceID, "feedback", fbID)
		return p, nil, nil, ErrNotFound
	}
	return p, r, d, nil
}

// internalNF passes NOT_FOUND through and wraps anything else.
func (s *Service) internalNF(op string, err error) error {
	if errors.Is(err, ErrNotFound) {
		return ErrNotFound
	}
	return s.internal(op, err)
}

// GetPublished returns an authorized published revision (feedback.json).
func (s *Service) GetPublished(ctx context.Context, fbID string, rev int) (PublishedFeedback, error) {
	_, r, _, err := s.authRead(ctx, "feedback.read", fbID, rev)
	if err != nil {
		return PublishedFeedback{}, err
	}
	data, err := s.readArtifact(ctx, r, ArtifactFeedback)
	if err != nil {
		return PublishedFeedback{}, err
	}
	var pf PublishedFeedback
	if err := json.Unmarshal(data.Data, &pf); err != nil {
		return PublishedFeedback{}, s.internal("feedback.read", err)
	}
	return pf, nil
}

// ArtifactData is one verified artifact file.
type ArtifactData struct {
	Name     string
	MimeType string
	SHA256   string
	Size     int64
	Data     []byte
}

func (s *Service) readArtifact(ctx context.Context, r *revisionRow, name string) (*ArtifactData, error) {
	if !validArtifactName(name) {
		return nil, ErrNotFound
	}
	var a ArtifactData
	err := s.db().QueryRowContext(ctx, `SELECT name, mime_type, sha256, size_bytes FROM artifacts WHERE feedback_id = ? AND revision = ? AND name = ?`,
		r.FeedbackID, r.Revision, name).Scan(&a.Name, &a.MimeType, &a.SHA256, &a.Size)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, s.internal("artifact.read", err)
	}
	data, err := s.st.Artifacts.Read(r.FeedbackID, r.Revision, name, s.lim.MaxImageBytes+1<<20)
	if err != nil {
		s.artifactFailure(ctx, r.WorkspaceID, "artifact.read_failed", r.FeedbackID, r.Revision)
		return nil, s.internal("artifact.read", err)
	}
	if int64(len(data)) != a.Size || sha256Hex(data) != a.SHA256 {
		s.artifactFailure(ctx, r.WorkspaceID, "artifact.integrity_failed", r.FeedbackID, r.Revision)
		return nil, s.internal("artifact.read", errors.New("artifact integrity check failed"))
	}
	a.Data = data
	return &a, nil
}

// ReadArtifact returns one immutable share artifact of an authorized
// revision, verified against its recorded hash and size. Raw or private
// images are not artifacts and can never be named here.
func (s *Service) ReadArtifact(ctx context.Context, fbID string, rev int, name string) (*ArtifactData, error) {
	_, r, _, err := s.authRead(ctx, "artifact.read", fbID, rev)
	if err != nil {
		return nil, err
	}
	return s.readArtifact(ctx, r, name)
}

// ReadManifest returns manifest.json of an authorized revision.
func (s *Service) ReadManifest(ctx context.Context, fbID string, rev int) (*ArtifactData, error) {
	return s.ReadArtifact(ctx, fbID, rev, ArtifactManifest)
}

// GetDelivery returns the delivery record and history of an authorized
// revision.
func (s *Service) GetDelivery(ctx context.Context, fbID string, rev int) (DeliveryDetail, error) {
	_, r, d, err := s.authRead(ctx, "delivery.read", fbID, rev)
	if err != nil {
		return DeliveryDetail{}, err
	}
	rows, err := s.db().QueryContext(ctx, `SELECT from_state, to_state, actor_id, actor_role, at, reason FROM delivery_history
		WHERE feedback_id = ? AND revision = ? ORDER BY id`, r.FeedbackID, r.Revision)
	if err != nil {
		return DeliveryDetail{}, s.internal("delivery.read", err)
	}
	defer rows.Close()
	out := DeliveryDetail{
		FeedbackID: d.FeedbackID, Revision: d.Revision, WorkspaceID: d.WorkspaceID, State: d.State, AgentID: d.AgentID,
		QueuedAt: ptrTime(d.Queued), AcknowledgedAt: ptrTime(d.Acked), InProgressAt: ptrTime(d.InProg), ResolvedAt: ptrTime(d.Resolved),
		VerifiedAt: ptrTime(d.Verified), CancelledAt: ptrTime(d.Cancelled), LeaseID: d.LeaseID, LastReason: d.LastReason,
		RejectionCount: d.Rejections, History: []Transition{},
	}
	if d.Summary != "" {
		res := &ResolveResult{Summary: d.Summary, Verification: d.Verification, ResultFeedbackIDs: []string{}}
		_ = json.Unmarshal([]byte(d.ResultIDs), &res.ResultFeedbackIDs)
		out.Result = res
	}
	for rows.Next() {
		var (
			t  Transition
			at int64
		)
		if err := rows.Scan(&t.From, &t.To, &t.ActorID, &t.ActorRole, &at, &t.Reason); err != nil {
			return DeliveryDetail{}, s.internal("delivery.read", err)
		}
		t.At = fromMS(at)
		out.History = append(out.History, t)
	}
	return out, s.internal("delivery.read", rows.Err())
}

func (s *Service) summaries(ctx context.Context, p Principal, workspaceID, feedbackID, state string, cursor int64, limit int) (FeedbackList, error) {
	where := "d.workspace_id = ?"
	args := []any{workspaceID}
	if feedbackID != "" {
		where += " AND d.feedback_id = ?"
		args = append(args, feedbackID)
	}
	if state != "" {
		where += " AND d.state = ?"
		args = append(args, state)
	}
	if cursor > 0 {
		where += " AND d.seq < ?"
		args = append(args, cursor)
	}
	if p.Role == RoleAgent {
		where += " AND d.agent_id = ? AND d.state IN ('QUEUED','ACKNOWLEDGED','IN_PROGRESS','RESOLVED')"
		args = append(args, p.ID)
	}
	args = append(args, limit+1)
	rows, err := s.db().QueryContext(ctx, `SELECT d.seq, d.feedback_id, d.revision, d.workspace_id, r.title, d.state, d.agent_id, r.published_at, d.updated_at
		FROM deliveries d JOIN revisions r ON r.feedback_id = d.feedback_id AND r.revision = d.revision
		WHERE `+where+` ORDER BY d.seq DESC LIMIT ?`, args...)
	if err != nil {
		return FeedbackList{}, s.internal("feedback.list", err)
	}
	defer rows.Close()
	out := FeedbackList{Items: []FeedbackSummary{}}
	var lastSeq int64
	for rows.Next() {
		var (
			seq      int64
			it       FeedbackSummary
			pub, upd sql.NullInt64
		)
		if err := rows.Scan(&seq, &it.FeedbackID, &it.Revision, &it.WorkspaceID, &it.Title, &it.DeliveryState, &it.AgentID, &pub, &upd); err != nil {
			return FeedbackList{}, s.internal("feedback.list", err)
		}
		if len(out.Items) == limit {
			c := strconv.FormatInt(lastSeq, 10)
			out.NextCursor = &c
			break
		}
		it.PublishedAt, it.UpdatedAt = fromMS(pub.Int64), fromMS(upd.Int64)
		out.Items = append(out.Items, it)
		lastSeq = seq
	}
	return out, s.internal("feedback.list", rows.Err())
}

// ListPublished lists published revisions (one row per revision) of a
// workspace, newest first. Agents only see revisions assigned to them.
func (s *Service) ListPublished(ctx context.Context, q ListQuery) (FeedbackList, error) {
	p, err := requirePrincipal(ctx)
	if err != nil {
		return FeedbackList{}, err
	}
	if !p.canReadPublished() && p.Role != RoleAgent {
		return FeedbackList{}, ErrForbidden
	}
	if err := s.workspaceOK(p, q.WorkspaceID); err != nil {
		return FeedbackList{}, err
	}
	if q.State != "" && !validState(q.State) {
		return FeedbackList{}, badRequest("unknown state filter")
	}
	var cursor int64
	if q.Cursor != "" {
		cursor, err = strconv.ParseInt(q.Cursor, 10, 64)
		if err != nil || cursor < 1 {
			return FeedbackList{}, badRequest("invalid cursor")
		}
	}
	limit := q.Limit
	if limit < 1 || limit > 100 {
		limit = 50
	}
	return s.summaries(ctx, p, q.WorkspaceID, "", q.State, cursor, limit)
}

// ListRevisions lists every authorized revision of one feedback item, newest
// first.
func (s *Service) ListRevisions(ctx context.Context, fbID string) (FeedbackList, error) {
	p, err := requirePrincipal(ctx)
	if err != nil {
		return FeedbackList{}, err
	}
	if !p.canReadPublished() && p.Role != RoleAgent {
		return FeedbackList{}, ErrForbidden
	}
	var ws string
	err = s.db().QueryRowContext(ctx, `SELECT workspace_id FROM feedback WHERE id = ?`, fbID).Scan(&ws)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !p.InWorkspace(ws)) {
		return FeedbackList{}, ErrNotFound
	}
	if err != nil {
		return FeedbackList{}, s.internal("revision.list", err)
	}
	return s.summaries(ctx, p, ws, fbID, "", 0, 100)
}

// ---- transitions -----------------------------------------------------------

func (s *Service) loadDeliveryFor(ctx context.Context, p Principal, action, fbID string, rev int, agentAction bool) (*deliveryRow, error) {
	if fbID == "" || len(fbID) > 128 || rev < 1 {
		return nil, badRequest("feedback id and revision are required")
	}
	d, err := getDelivery(ctx, s.db(), fbID, rev)
	if err != nil {
		return nil, s.internalNF(action, err)
	}
	if !p.InWorkspace(d.WorkspaceID) {
		s.auditDenied(ctx, p, action, d.WorkspaceID, "feedback", fbID)
		return nil, ErrNotFound
	}
	if agentAction {
		if p.Role != RoleAgent {
			s.auditDenied(ctx, p, action, d.WorkspaceID, "feedback", fbID)
			return nil, ErrForbidden
		}
		if d.AgentID != p.ID {
			// Only the assigned agent may act; others cannot even tell the
			// revision exists.
			s.auditDenied(ctx, p, action, d.WorkspaceID, "feedback", fbID)
			return nil, ErrNotFound
		}
	} else if !p.IsHuman() {
		s.auditDenied(ctx, p, action, d.WorkspaceID, "feedback", fbID)
		return nil, ErrForbidden
	}
	return d, nil
}

// step applies one guarded state change inside tx: the UPDATE only matches
// the expected current state, so racing transitions cannot both win. set is
// an extra "col = ?, ..." clause with its args.
func (s *Service) step(ctx context.Context, tx *sql.Tx, p Principal, d *deliveryRow, from []string, to, event, reason, set string, args ...any) error {
	now := s.nowUTC()
	ph := ""
	for i := range from {
		if i > 0 {
			ph += ","
		}
		ph += "?"
	}
	q := `UPDATE deliveries SET state = ?, updated_at = ?`
	a := []any{to, s.ms(now)}
	if set != "" {
		q += ", " + set
		a = append(a, args...)
	}
	q += ` WHERE feedback_id = ? AND revision = ? AND state IN (` + ph + `)`
	a = append(a, d.FeedbackID, d.Revision)
	for _, f := range from {
		a = append(a, f)
	}
	res, err := tx.ExecContext(ctx, q, a...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return invalidState("the delivery is not in a state that allows this transition")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_history (feedback_id, revision, from_state, to_state, actor_id, actor_role, at, reason) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.FeedbackID, d.Revision, d.State, to, p.ID, p.Role, s.ms(now), reason); err != nil {
		return err
	}
	ev := evSpec{Workspace: d.WorkspaceID, Category: "delivery", Action: event, SubjectType: "feedback", SubjectID: d.FeedbackID, Revision: d.Revision, Visibility: storage.VisWorkspace}
	if d.AgentID != "" {
		ev.Visibility = storage.VisAssigned
		ev.AgentID = d.AgentID
	}
	return s.emit(ctx, tx, p, ev)
}

func (s *Service) runStep(ctx context.Context, op, fbID string, rec *idemRec, fn func(tx *sql.Tx) (Delivery, error)) (Delivery, error) {
	var out Delivery
	err := s.st.DB.InTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = fn(tx)
		if err != nil {
			return err
		}
		return rec.save(ctx, tx, out)
	})
	if err != nil {
		return Delivery{}, s.internal(op, err)
	}
	s.wake()
	return out, nil
}

// Dispatch queues a published revision for an agent (READY -> QUEUED). Only
// the latest published revision can be dispatched. An omitted agentId is
// resolved by the server (see resolveAgent); an explicit one must be a
// configured agent of the revision's workspace.
func (s *Service) Dispatch(ctx context.Context, fbID string, in DispatchInput, key string) (Delivery, error) {
	p, err := s.requireHuman(ctx, "delivery.dispatch")
	if err != nil {
		return Delivery{}, err
	}
	if in.AgentID != "" && !validAgentID(in.AgentID) {
		return Delivery{}, badRequest("agentId must be 1-128 printable ASCII characters")
	}
	input, _ := json.Marshal(in)
	return idempotent(ctx, s, p, "delivery.dispatch", fbID, key, input, func(rec *idemRec) (Delivery, error) {
		d, err := s.loadDeliveryFor(ctx, p, "delivery.dispatch", fbID, in.Revision, false)
		if err != nil {
			return Delivery{}, err
		}
		var latest sql.NullInt64
		if err := s.db().QueryRowContext(ctx, `SELECT MAX(revision) FROM revisions WHERE feedback_id = ? AND status = 'PUBLISHED'`, fbID).Scan(&latest); err != nil {
			return Delivery{}, s.internal("delivery.dispatch", err)
		}
		if d.State == StateReady && latest.Valid && int(latest.Int64) != d.Revision {
			return Delivery{}, ErrRevisionSuperseded
		}
		// Only a READY delivery can be queued; any other state fails the guarded
		// step below with INVALID_STATE regardless of the recipient.
		agentID := in.AgentID
		if d.State == StateReady {
			if agentID, err = s.resolveAgent(d.WorkspaceID, in.AgentID); err != nil {
				return Delivery{}, err
			}
		}
		return s.runStep(ctx, "delivery.dispatch", fbID, rec, func(tx *sql.Tx) (Delivery, error) {
			now := s.ms(s.nowUTC())
			d.AgentID = agentID
			if err := s.step(ctx, tx, p, d, []string{StateReady}, StateQueued, "delivery.dispatched", "", "agent_id = ?, queued_at = ?", agentID, now); err != nil {
				return Delivery{}, err
			}
			out := d.delivery()
			out.State = StateQueued
			return out, nil
		})
	})
}

func validAgentID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := range len(id) {
		if id[i] <= 0x20 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

// Ack records receipt by the assigned agent (QUEUED -> ACKNOWLEDGED). It
// proves nothing about processing; the state is a fact only the agent's own
// authenticated call can create.
func (s *Service) Ack(ctx context.Context, fbID string, in AckInput, key string) (Delivery, error) {
	p, err := requirePrincipal(ctx)
	if err != nil {
		return Delivery{}, err
	}
	input, _ := json.Marshal(in)
	return idempotent(ctx, s, p, "delivery.ack", fbID, key, input, func(rec *idemRec) (Delivery, error) {
		d, err := s.loadDeliveryFor(ctx, p, "delivery.ack", fbID, in.Revision, true)
		if err != nil {
			return Delivery{}, err
		}
		return s.runStep(ctx, "delivery.ack", fbID, rec, func(tx *sql.Tx) (Delivery, error) {
			if err := s.step(ctx, tx, p, d, []string{StateQueued}, StateAcknowledged, "delivery.acknowledged", "", "acknowledged_at = ?", s.ms(s.nowUTC())); err != nil {
				return Delivery{}, err
			}
			out := d.delivery()
			out.State = StateAcknowledged
			return out, nil
		})
	})
}

// AuthorizeAgentLease checks, before the Gateway grants an AGENT lease tied
// to feedback, that the calling agent is assigned and the revision is
// ACKNOWLEDGED (or already IN_PROGRESS for a re-lease). revision 0 selects
// the single active revision of the feedback assigned to the agent; more than
// one is ambiguous. It returns the resolved revision.
func (s *Service) AuthorizeAgentLease(ctx context.Context, fbID string, revision int) (int, error) {
	p, err := requirePrincipal(ctx)
	if err != nil {
		return 0, err
	}
	if p.Role != RoleAgent {
		s.auditDenied(ctx, p, "delivery.lease", p.WorkspaceID, "feedback", fbID)
		return 0, ErrForbidden
	}
	if fbID == "" || len(fbID) > 128 || revision < 0 {
		return 0, badRequest("feedback id and revision are required")
	}
	if revision == 0 {
		rows, err := s.db().QueryContext(ctx, `SELECT revision FROM deliveries WHERE feedback_id = ? AND agent_id = ? AND state IN ('ACKNOWLEDGED','IN_PROGRESS') ORDER BY revision`, fbID, p.ID)
		if err != nil {
			return 0, s.internal("delivery.lease", err)
		}
		var revs []int
		for rows.Next() {
			var n int
			if err := rows.Scan(&n); err != nil {
				_ = rows.Close()
				return 0, s.internal("delivery.lease", err)
			}
			revs = append(revs, n)
		}
		_ = rows.Close()
		switch len(revs) {
		case 0:
			s.auditDenied(ctx, p, "delivery.lease", p.WorkspaceID, "feedback", fbID)
			return 0, invalidState("no acknowledged revision is assigned to this agent")
		case 1:
			revision = revs[0]
		default:
			return 0, invalidState("several revisions are active; name the revision")
		}
	}
	d, err := s.loadDeliveryFor(ctx, p, "delivery.lease", fbID, revision, true)
	if err != nil {
		return 0, err
	}
	if d.State != StateAcknowledged && d.State != StateInProgress {
		return 0, invalidState("acknowledge the feedback before requesting a lease for it")
	}
	return revision, nil
}

// BeginWork moves ACKNOWLEDGED -> IN_PROGRESS after a real AGENT lease was
// granted for the feedback. It is a no-op for a revision that is already
// IN_PROGRESS under the same agent. leaseID is a non-secret lease identifier
// stored for audit. If it fails, the Gateway must release the lease.
func (s *Service) BeginWork(ctx context.Context, fbID string, revision int, leaseID string) (Delivery, error) {
	p, err := requirePrincipal(ctx)
	if err != nil {
		return Delivery{}, err
	}
	if len(leaseID) > 128 {
		return Delivery{}, badRequest("invalid lease id")
	}
	if revision == 0 {
		revision, err = s.AuthorizeAgentLease(ctx, fbID, 0)
		if err != nil {
			return Delivery{}, err
		}
	}
	d, err := s.loadDeliveryFor(ctx, p, "delivery.begin", fbID, revision, true)
	if err != nil {
		return Delivery{}, err
	}
	if d.State == StateInProgress {
		return d.delivery(), nil
	}
	var out Delivery
	err = s.st.DB.InTx(ctx, func(tx *sql.Tx) error {
		if err := s.step(ctx, tx, p, d, []string{StateAcknowledged}, StateInProgress, "delivery.in_progress", "", "in_progress_at = ?, lease_id = ?", s.ms(s.nowUTC()), leaseID); err != nil {
			return err
		}
		out = d.delivery()
		out.State = StateInProgress
		return nil
	})
	if err != nil {
		return Delivery{}, s.internal("delivery.begin", err)
	}
	s.wake()
	return out, nil
}

// Resolve submits the agent's result for human verification (IN_PROGRESS ->
// RESOLVED). It never verifies; only a human can.
func (s *Service) Resolve(ctx context.Context, fbID string, in ResolveInput, key string) (Delivery, error) {
	p, err := requirePrincipal(ctx)
	if err != nil {
		return Delivery{}, err
	}
	if n := len([]rune(in.Summary)); n < 1 || !validText(in.Summary, 12000, true) {
		return Delivery{}, badRequest("summary is required (max 12000 characters)")
	}
	if n := len([]rune(in.Verification)); n < 1 || !validText(in.Verification, 12000, true) {
		return Delivery{}, badRequest("verification is required (max 12000 characters)")
	}
	if len(in.ResultFeedbackIDs) > 10 {
		return Delivery{}, badRequest("at most 10 resultFeedbackIds")
	}
	seen := map[string]bool{}
	for _, id := range in.ResultFeedbackIDs {
		if id == "" || len(id) > 128 || seen[id] {
			return Delivery{}, ErrInvalidResultRef
		}
		seen[id] = true
	}
	ids := in.ResultFeedbackIDs
	if ids == nil {
		ids = []string{}
	}
	idsJSON, _ := json.Marshal(ids)
	input, _ := json.Marshal(in)
	return idempotent(ctx, s, p, "delivery.resolve", fbID, key, input, func(rec *idemRec) (Delivery, error) {
		d, err := s.loadDeliveryFor(ctx, p, "delivery.resolve", fbID, in.Revision, true)
		if err != nil {
			return Delivery{}, err
		}
		for _, id := range ids {
			var n int
			if err := s.db().QueryRowContext(ctx, `SELECT COUNT(*) FROM revisions r JOIN feedback f ON f.id = r.feedback_id
				WHERE r.feedback_id = ? AND f.workspace_id = ? AND r.status = 'PUBLISHED'`, id, d.WorkspaceID).Scan(&n); err != nil {
				return Delivery{}, s.internal("delivery.resolve", err)
			}
			if n == 0 {
				return Delivery{}, ErrInvalidResultRef
			}
		}
		return s.runStep(ctx, "delivery.resolve", fbID, rec, func(tx *sql.Tx) (Delivery, error) {
			if err := s.step(ctx, tx, p, d, []string{StateInProgress}, StateResolved, "delivery.resolved", "",
				"resolved_at = ?, summary = ?, verification = ?, result_ids_json = ?", s.ms(s.nowUTC()), in.Summary, in.Verification, string(idsJSON)); err != nil {
				return Delivery{}, err
			}
			out := d.delivery()
			out.State = StateResolved
			return out, nil
		})
	})
}

// Verify is the human decision on a resolved revision: outcome "verified"
// (RESOLVED -> VERIFIED) or "rejected" with a reason (RESOLVED -> QUEUED, the
// history is kept and the agent must acknowledge again).
func (s *Service) Verify(ctx context.Context, fbID string, in VerifyInput, key string) (Delivery, error) {
	p, err := s.requireHuman(ctx, "delivery.verify")
	if err != nil {
		return Delivery{}, err
	}
	switch in.Outcome {
	case "verified":
	case "rejected":
		if n := len([]rune(in.Reason)); n < 1 || !validText(in.Reason, 12000, true) {
			return Delivery{}, badRequest("a rejection requires a reason (max 12000 characters)")
		}
	default:
		return Delivery{}, badRequest("outcome must be verified or rejected")
	}
	if in.Outcome == "verified" && !validText(in.Reason, 12000, true) {
		return Delivery{}, badRequest("reason is too long")
	}
	input, _ := json.Marshal(in)
	return idempotent(ctx, s, p, "delivery.verify", fbID, key, input, func(rec *idemRec) (Delivery, error) {
		d, err := s.loadDeliveryFor(ctx, p, "delivery.verify", fbID, in.Revision, false)
		if err != nil {
			return Delivery{}, err
		}
		return s.runStep(ctx, "delivery.verify", fbID, rec, func(tx *sql.Tx) (Delivery, error) {
			now := s.ms(s.nowUTC())
			out := d.delivery()
			if in.Outcome == "verified" {
				if err := s.step(ctx, tx, p, d, []string{StateResolved}, StateVerified, "delivery.verified", in.Reason, "verified_at = ?, last_reason = ?", now, in.Reason); err != nil {
					return Delivery{}, err
				}
				out.State = StateVerified
				return out, nil
			}
			if err := s.step(ctx, tx, p, d, []string{StateResolved}, StateQueued, "delivery.rejected", in.Reason,
				"queued_at = ?, acknowledged_at = NULL, in_progress_at = NULL, resolved_at = NULL, lease_id = '', last_reason = ?, rejection_count = rejection_count + 1", now, in.Reason); err != nil {
				return Delivery{}, err
			}
			out.State = StateQueued
			return out, nil
		})
	})
}

// Cancel ends any non-terminal delivery (-> CANCELLED). It does not undo
// effects an agent already produced.
func (s *Service) Cancel(ctx context.Context, fbID string, in CancelInput, key string) (Delivery, error) {
	p, err := s.requireHuman(ctx, "delivery.cancel")
	if err != nil {
		return Delivery{}, err
	}
	if !validText(in.Reason, 12000, true) {
		return Delivery{}, badRequest("reason is too long")
	}
	input, _ := json.Marshal(in)
	return idempotent(ctx, s, p, "delivery.cancel", fbID, key, input, func(rec *idemRec) (Delivery, error) {
		d, err := s.loadDeliveryFor(ctx, p, "delivery.cancel", fbID, in.Revision, false)
		if err != nil {
			return Delivery{}, err
		}
		return s.runStep(ctx, "delivery.cancel", fbID, rec, func(tx *sql.Tx) (Delivery, error) {
			if err := s.step(ctx, tx, p, d, []string{StateReady, StateQueued, StateAcknowledged, StateInProgress, StateResolved}, StateCancelled,
				"delivery.cancelled", in.Reason, "cancelled_at = ?, last_reason = ?", s.ms(s.nowUTC()), in.Reason); err != nil {
				return Delivery{}, err
			}
			out := d.delivery()
			out.State = StateCancelled
			return out, nil
		})
	})
}
