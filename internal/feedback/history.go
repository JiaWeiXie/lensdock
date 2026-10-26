package feedback

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"

	"github.com/JiaWeiXie/lensdock/internal/storage"
)

// Clearing published history is visibility-only. It records the revision in
// history_hidden so the history listing (GET /feedback?history=visible) omits
// it. Published revisions, artifacts, deliveries, tasks, drafts and every
// direct read stay exactly as they were; nothing is cancelled or deleted. A
// revision published after a clear is not hidden, even when the clear is
// replayed with the same Idempotency-Key (the stored response is returned and
// no new selection runs).

// ClearHistory hides one published revision from the history listing. It
// returns Cleared 1, or 0 when the revision was already cleared.
func (s *Service) ClearHistory(ctx context.Context, fbID string, revision int, key string) (ClearHistoryResult, error) {
	p, err := s.requireHuman(ctx, "history.clear")
	if err != nil {
		return ClearHistoryResult{}, err
	}
	if fbID == "" || len(fbID) > 128 || revision < 1 {
		return ClearHistoryResult{}, badRequest("feedback id and revision are required")
	}
	input, _ := json.Marshal(struct {
		Revision int `json:"revision"`
	}{revision})
	return idempotent(ctx, s, p, "history.clear", fbID, key, input, func(rec *idemRec) (ClearHistoryResult, error) {
		d, err := s.loadDeliveryFor(ctx, p, "history.clear", fbID, revision, false)
		if err != nil {
			return ClearHistoryResult{}, err
		}
		return s.hide(ctx, p, rec, d.WorkspaceID, "feedback", fbID, revision,
			`INSERT OR IGNORE INTO history_hidden (feedback_id, revision, workspace_id, hidden_by, hidden_at)
				SELECT d.feedback_id, d.revision, d.workspace_id, ?, ? FROM deliveries d
				JOIN revisions r ON r.feedback_id = d.feedback_id AND r.revision = d.revision
				WHERE d.feedback_id = ? AND d.revision = ? AND r.status = 'PUBLISHED'`,
			func(now int64) []any { return []any{p.ID, now, fbID, revision} })
	})
}

// ClearAllHistory hides every currently published revision of the workspace,
// whatever its delivery state, from the history listing. The selection and the
// insert are one statement in one transaction, so a revision published
// concurrently is either cleared or stays visible, never half-handled. It
// returns the number of revisions newly hidden.
func (s *Service) ClearAllHistory(ctx context.Context, workspaceID, key string) (ClearHistoryResult, error) {
	p, err := s.requireHuman(ctx, "history.clear")
	if err != nil {
		return ClearHistoryResult{}, err
	}
	if err := s.workspaceOK(p, workspaceID); err != nil {
		return ClearHistoryResult{}, err
	}
	return idempotent(ctx, s, p, "history.clear_all", workspaceID, key, []byte(workspaceID), func(rec *idemRec) (ClearHistoryResult, error) {
		return s.hide(ctx, p, rec, workspaceID, "workspace", workspaceID, 0,
			`INSERT OR IGNORE INTO history_hidden (feedback_id, revision, workspace_id, hidden_by, hidden_at)
				SELECT d.feedback_id, d.revision, d.workspace_id, ?, ? FROM deliveries d
				JOIN revisions r ON r.feedback_id = d.feedback_id AND r.revision = d.revision
				WHERE d.workspace_id = ? AND r.status = 'PUBLISHED'`,
			func(now int64) []any { return []any{p.ID, now, workspaceID} })
	})
}

// hide runs insert (an INSERT OR IGNORE ... SELECT) and records the outcome,
// the event and the idempotency response in one transaction.
func (s *Service) hide(ctx context.Context, p Principal, rec *idemRec, workspaceID, subjectType, subjectID string, revision int, insert string, args func(now int64) []any) (ClearHistoryResult, error) {
	var out ClearHistoryResult
	err := s.st.DB.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, insert, args(s.ms(s.nowUTC()))...)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		out.Cleared = int(n)
		if err := s.emit(ctx, tx, p, evSpec{Workspace: workspaceID, Category: "feedback", Action: "history.cleared", SubjectType: subjectType, SubjectID: subjectID,
			Revision: revision, Visibility: storage.VisWorkspace, Detail: "cleared=" + strconv.Itoa(out.Cleared)}); err != nil {
			return err
		}
		return rec.save(ctx, tx, out)
	})
	if err != nil {
		return ClearHistoryResult{}, s.internal("history.clear", err)
	}
	s.wake()
	return out, nil
}
