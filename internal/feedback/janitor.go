package feedback

import (
	"context"
	"database/sql"
	"log/slog"
	"strconv"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/storage"
)

// systemFor returns the pseudo-principal under which retention events are
// recorded; the owner's ID is kept so private events reach the owner.
func systemFor(owner string) Principal { return Principal{ID: owner, Role: "system"} }

// RunJanitor sweeps immediately and then every interval until ctx ends.
func (s *Service) RunJanitor(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("feedback sweep failed", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type expiredDraft struct {
	FeedbackID, Workspace, Owner, CaptureID string
	Revision                                int
	PreviewKey, ObjectKey                   sql.NullString
}

// Sweep enforces retention: reservations that never received an image fail,
// drafts and unbound captures expire 24h (DraftTTL) after their last
// modification, owners get a DRAFT_EXPIRING event ahead of that, old
// idempotency records go, and unreferenced files are removed. Published
// revisions and evidence of deliveries in progress are never touched.
func (s *Service) Sweep(ctx context.Context) error {
	now := s.nowUTC()
	nowMS := s.ms(now)

	if _, err := s.db().ExecContext(ctx, `UPDATE captures SET status = 'failed', updated_at = ? WHERE status IN ('awaiting-upload','capturing') AND expires_at < ?`, nowMS, nowMS); err != nil {
		return err
	}
	if _, err := s.db().ExecContext(ctx, `DELETE FROM captures WHERE status = 'failed' AND updated_at < ?`, s.ms(now.Add(-s.lim.FailedCaptureRetention))); err != nil {
		return err
	}

	rows, err := s.db().QueryContext(ctx, `SELECT r.feedback_id, f.workspace_id, r.owner_id, r.capture_id, r.revision, r.preview_key, c.object_key
		FROM revisions r JOIN feedback f ON f.id = r.feedback_id LEFT JOIN captures c ON c.id = r.capture_id
		WHERE r.status = 'DRAFT' AND r.expires_at < ?`, nowMS)
	if err != nil {
		return err
	}
	var expired []expiredDraft
	for rows.Next() {
		var e expiredDraft
		if err := rows.Scan(&e.FeedbackID, &e.Workspace, &e.Owner, &e.CaptureID, &e.Revision, &e.PreviewKey, &e.ObjectKey); err != nil {
			_ = rows.Close()
			return err
		}
		expired = append(expired, e)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, e := range expired {
		err := s.st.DB.InTx(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, `DELETE FROM revisions WHERE feedback_id = ? AND revision = ? AND status = 'DRAFT' AND expires_at < ?`, e.FeedbackID, e.Revision, nowMS)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return nil
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM captures WHERE id = ? AND feedback_id = ?`, e.CaptureID, e.FeedbackID); err != nil {
				return err
			}
			var left int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM revisions WHERE feedback_id = ?`, e.FeedbackID).Scan(&left); err != nil {
				return err
			}
			if left == 0 {
				if _, err := tx.ExecContext(ctx, `DELETE FROM feedback WHERE id = ?`, e.FeedbackID); err != nil {
					return err
				}
			}
			return s.emit(ctx, tx, systemFor(e.Owner), evSpec{Workspace: e.Workspace, Category: "retention", Action: "draft.expired", SubjectType: "feedback", SubjectID: e.FeedbackID, Revision: e.Revision, Visibility: storage.VisPrivate})
		})
		if err != nil {
			return err
		}
		if e.PreviewKey.Valid {
			_ = s.st.Private.Delete(e.PreviewKey.String)
		}
		if e.ObjectKey.Valid {
			_ = s.st.Private.Delete(e.ObjectKey.String)
		}
	}

	// Captures never bound to a draft.
	crows, err := s.db().QueryContext(ctx, `SELECT id, object_key FROM captures WHERE feedback_id IS NULL AND status = 'ready' AND expires_at < ?`, nowMS)
	if err != nil {
		return err
	}
	type orphan struct {
		id  string
		key sql.NullString
	}
	var orphans []orphan
	for crows.Next() {
		var o orphan
		if err := crows.Scan(&o.id, &o.key); err != nil {
			_ = crows.Close()
			return err
		}
		orphans = append(orphans, o)
	}
	if err := crows.Err(); err != nil {
		_ = crows.Close()
		return err
	}
	_ = crows.Close()
	for _, o := range orphans {
		if _, err := s.db().ExecContext(ctx, `DELETE FROM captures WHERE id = ? AND feedback_id IS NULL AND expires_at < ?`, o.id, nowMS); err != nil {
			return err
		}
		if o.key.Valid {
			_ = s.st.Private.Delete(o.key.String)
		}
	}

	// Expiry warnings, once per draft modification.
	wrows, err := s.db().QueryContext(ctx, `SELECT r.feedback_id, r.revision, f.workspace_id, r.owner_id FROM revisions r JOIN feedback f ON f.id = r.feedback_id
		WHERE r.status = 'DRAFT' AND r.warned = 0 AND r.expires_at >= ? AND r.expires_at < ?`, nowMS, s.ms(now.Add(s.lim.ExpiryWarning)))
	if err != nil {
		return err
	}
	var warn []expiredDraft
	for wrows.Next() {
		var e expiredDraft
		if err := wrows.Scan(&e.FeedbackID, &e.Revision, &e.Workspace, &e.Owner); err != nil {
			_ = wrows.Close()
			return err
		}
		warn = append(warn, e)
	}
	if err := wrows.Err(); err != nil {
		_ = wrows.Close()
		return err
	}
	_ = wrows.Close()
	for _, e := range warn {
		err := s.st.DB.InTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `UPDATE revisions SET warned = 1 WHERE feedback_id = ? AND revision = ? AND status = 'DRAFT'`, e.FeedbackID, e.Revision); err != nil {
				return err
			}
			return s.emit(ctx, tx, systemFor(e.Owner), evSpec{Workspace: e.Workspace, Category: "retention", Action: "draft.expiring", SubjectType: "feedback", SubjectID: e.FeedbackID, Revision: e.Revision, Visibility: storage.VisPrivate})
		})
		if err != nil {
			return err
		}
	}

	if _, err := storage.IdemPurge(ctx, s.db(), now.Add(-s.lim.IdempotencyRetention)); err != nil {
		return err
	}
	s.wake()
	return s.reconcile(ctx)
}

// reconcile removes files that no database row references and that are older
// than the grace period (so in-flight writes are safe): crash leftovers of
// uploads, abandoned previews and publish attempts.
func (s *Service) reconcile(ctx context.Context) error {
	cutoff := s.now().Add(-s.lim.OrphanGrace)
	refs := map[string]bool{}
	for _, q := range []string{
		`SELECT object_key FROM captures WHERE object_key IS NOT NULL`,
		`SELECT preview_key FROM revisions WHERE preview_key IS NOT NULL`,
	} {
		rows, err := s.db().QueryContext(ctx, q)
		if err != nil {
			return err
		}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				_ = rows.Close()
				return err
			}
			refs[k] = true
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
	}
	objs, err := s.st.Private.List()
	if err != nil {
		return err
	}
	for _, o := range objs {
		if !refs[o.Key] && o.ModTime.Before(cutoff) {
			_ = s.st.Private.Delete(o.Key)
		}
	}

	published := map[string]bool{}
	rows, err := s.db().QueryContext(ctx, `SELECT feedback_id, revision FROM revisions WHERE status = 'PUBLISHED'`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var (
			id  string
			rev int
		)
		if err := rows.Scan(&id, &rev); err != nil {
			_ = rows.Close()
			return err
		}
		published[id+"#"+itoa(rev)] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	dirs, err := s.st.Artifacts.List()
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if d.ModTime.After(cutoff) {
			continue
		}
		if d.Staging || !published[d.FeedbackID+"#"+itoa(d.Revision)] {
			_ = s.st.Artifacts.RemovePath(d)
		}
	}
	return nil
}

func itoa(n int) string { return strconv.Itoa(n) }
