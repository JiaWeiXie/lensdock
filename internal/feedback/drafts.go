package feedback

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/JiaWeiXie/lensdock/internal/storage"
)

type revisionRow struct {
	FeedbackID, WorkspaceID, OwnerID, Status, CaptureID string
	Revision, Version, PreviewVersion                   int
	TaskJSON, AnnotationsJSON, Title                    string
	PreviewKey, PublisherID, ManifestSHA, RedactedSHA   sql.NullString
	PreviewSize                                         int64
	Created, Updated, Expires                           int64
	Warned                                              int
	PublishedAt                                         sql.NullInt64
}

const revisionCols = `r.feedback_id, f.workspace_id, r.owner_id, r.status, r.capture_id, r.revision, r.version, r.preview_version,
	r.task_json, r.annotations_json, r.title, r.preview_key, r.publisher_id, r.manifest_sha256, r.redacted_sha256,
	r.preview_size, r.created_at, r.updated_at, r.expires_at, r.warned, r.published_at`

func scanRevision(sc scanner) (*revisionRow, error) {
	var r revisionRow
	err := sc.Scan(&r.FeedbackID, &r.WorkspaceID, &r.OwnerID, &r.Status, &r.CaptureID, &r.Revision, &r.Version, &r.PreviewVersion,
		&r.TaskJSON, &r.AnnotationsJSON, &r.Title, &r.PreviewKey, &r.PublisherID, &r.ManifestSHA, &r.RedactedSHA,
		&r.PreviewSize, &r.Created, &r.Updated, &r.Expires, &r.Warned, &r.PublishedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func getRevision(ctx context.Context, q storage.Querier, fbID string, rev int) (*revisionRow, error) {
	r, err := scanRevision(q.QueryRowContext(ctx, `SELECT `+revisionCols+` FROM revisions r JOIN feedback f ON f.id = r.feedback_id
		WHERE r.feedback_id = ? AND r.revision = ?`, fbID, rev))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

func latestRevision(ctx context.Context, q storage.Querier, fbID string) (*revisionRow, error) {
	r, err := scanRevision(q.QueryRowContext(ctx, `SELECT `+revisionCols+` FROM revisions r JOIN feedback f ON f.id = r.feedback_id
		WHERE r.feedback_id = ? ORDER BY r.revision DESC LIMIT 1`, fbID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

func etagFor(fbID string, rev, version int) string {
	return sha256Hex([]byte(fmt.Sprintf("lensdock-draft|%s|%d|%d", fbID, rev, version)))[:32]
}

func (r *revisionRow) etag() string { return etagFor(r.FeedbackID, r.Revision, r.Version) }

func (r *revisionRow) draft() Draft {
	return Draft{FeedbackID: r.FeedbackID, Revision: r.Revision, ETag: r.etag(), State: DraftState}
}

func (r *revisionRow) previewReady() bool {
	return r.PreviewKey.Valid && r.PreviewVersion == r.Version
}

// checkETag validates an If-Match header against the draft. A weak prefix and
// quotes are tolerated; "*" never matches (a precondition must bind a version).
func checkETag(ifMatch string, r *revisionRow) error {
	v := strings.TrimSpace(ifMatch)
	if v == "" {
		return ErrPreconditionRequired
	}
	v = strings.TrimPrefix(v, "W/")
	v = strings.Trim(v, `"`)
	if subtle.ConstantTimeCompare([]byte(v), []byte(r.etag())) != 1 {
		return ErrPreconditionFailed
	}
	return nil
}

func (s *Service) detail(r *revisionRow) (DraftDetail, error) {
	var task Task
	if err := json.Unmarshal([]byte(r.TaskJSON), &task); err != nil {
		return DraftDetail{}, err
	}
	if task.ReproductionSteps == nil {
		task.ReproductionSteps = []string{}
	}
	var annots []json.RawMessage
	if err := json.Unmarshal([]byte(r.AnnotationsJSON), &annots); err != nil {
		return DraftDetail{}, err
	}
	if annots == nil {
		annots = []json.RawMessage{}
	}
	_, reds, err := ParseAnnotations(annots)
	if err != nil {
		return DraftDetail{}, err
	}
	d := DraftDetail{
		Draft: r.draft(), CaptureID: r.CaptureID, Task: task, Annotations: annots,
		PreviewReady: r.previewReady(), ExpiresAt: fromMS(r.Expires), RedactionCount: len(reds), Warnings: []string{},
	}
	if len(reds) == 0 {
		d.Warnings = append(d.Warnings, WarnNoRedaction)
	}
	if exp := fromMS(r.Expires); s.now().Before(exp) && exp.Sub(s.now()) <= s.lim.ExpiryWarning {
		d.Warnings = append(d.Warnings, WarnExpiring)
	}
	return d, nil
}

// authDraft returns the open draft revision of fbID if p owns it.
func (s *Service) authDraft(ctx context.Context, action, fbID string) (Principal, *revisionRow, error) {
	p, err := s.requireHuman(ctx, action)
	if err != nil {
		return p, nil, err
	}
	if fbID == "" || len(fbID) > 128 {
		return p, nil, ErrNotFound
	}
	r, err := latestRevision(ctx, s.db(), fbID)
	if err != nil {
		return p, nil, s.internal(action, err)
	}
	if !p.InWorkspace(r.WorkspaceID) || (r.Status == revisionDraft && r.OwnerID != p.ID) {
		s.auditDenied(ctx, p, action, r.WorkspaceID, "feedback", fbID)
		return p, nil, ErrNotFound
	}
	if r.Status != revisionDraft {
		return p, nil, ErrNotFound
	}
	return p, r, nil
}

func (s *Service) draftQuota(ctx context.Context, q storage.Querier, ws string) error {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM revisions r JOIN feedback f ON f.id = r.feedback_id
		WHERE f.workspace_id = ? AND r.status = 'DRAFT'`, ws).Scan(&n); err != nil {
		return err
	}
	if n >= s.lim.MaxDraftsPerWorkspace {
		return ErrQuotaExceeded
	}
	return nil
}

func (s *Service) loadOwnedReadyCapture(ctx context.Context, q storage.Querier, p Principal, ws, id string) (*captureRow, error) {
	if id == "" || len(id) > 128 {
		return nil, badRequest("captureId is required")
	}
	c, err := getCapture(ctx, q, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if c.WorkspaceID != ws || c.OwnerID != p.ID {
		return nil, ErrNotFound
	}
	if c.Status != StatusReady || !c.ObjectKey.Valid {
		return nil, ErrCaptureNotReady
	}
	if c.FeedbackID.Valid {
		return nil, ErrCaptureInUse
	}
	return c, nil
}

// CreateDraft creates feedback revision 1 as a private draft on an owned,
// ready, unbound capture. All prose may be empty, now and at publish;
// enums, lengths and characters are validated now.
func (s *Service) CreateDraft(ctx context.Context, in CreateDraftInput, key string) (Draft, error) {
	p, err := s.requireHuman(ctx, "draft.create")
	if err != nil {
		return Draft{}, err
	}
	if err := s.workspaceOK(p, in.WorkspaceID); err != nil {
		return Draft{}, err
	}
	task := in.Task
	if err := validateTask(&task); err != nil {
		return Draft{}, err
	}
	taskJSON, _ := json.Marshal(task)
	input, _ := json.Marshal(in)
	return idempotent(ctx, s, p, "draft.create", in.WorkspaceID, key, input, func(rec *idemRec) (Draft, error) {
		now := s.nowUTC()
		fbID := newID("fb_")
		var out Draft
		err := s.st.DB.InTx(ctx, func(tx *sql.Tx) error {
			if err := s.draftQuota(ctx, tx, in.WorkspaceID); err != nil {
				return err
			}
			c, err := s.loadOwnedReadyCapture(ctx, tx, p, in.WorkspaceID, in.CaptureID)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO feedback (id, workspace_id, created_by, created_at) VALUES (?, ?, ?, ?)`,
				fbID, in.WorkspaceID, p.ID, s.ms(now)); err != nil {
				return err
			}
			exp := s.ms(now.Add(s.lim.DraftTTL))
			if _, err := tx.ExecContext(ctx, `INSERT INTO revisions (feedback_id, revision, owner_id, status, capture_id, task_json, annotations_json, version, created_at, updated_at, expires_at)
				VALUES (?, 1, ?, 'DRAFT', ?, ?, '[]', 1, ?, ?, ?)`, fbID, p.ID, c.ID, string(taskJSON), s.ms(now), s.ms(now), exp); err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `UPDATE captures SET feedback_id = ?, updated_at = ?, expires_at = ? WHERE id = ? AND feedback_id IS NULL`,
				fbID, s.ms(now), exp, c.ID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return ErrCaptureInUse
			}
			out = Draft{FeedbackID: fbID, Revision: 1, ETag: etagFor(fbID, 1, 1), State: DraftState}
			if err := s.emit(ctx, tx, p, evSpec{Workspace: in.WorkspaceID, Category: "feedback", Action: "draft.created", SubjectType: "feedback", SubjectID: fbID, Revision: 1, Visibility: storage.VisPrivate}); err != nil {
				return err
			}
			return rec.save(ctx, tx, out)
		})
		if err != nil {
			return Draft{}, s.internal("draft.create", err)
		}
		s.wake()
		return out, nil
	})
}

// GetDraft returns the owner's open draft.
func (s *Service) GetDraft(ctx context.Context, fbID string) (DraftDetail, error) {
	_, r, err := s.authDraft(ctx, "draft.read", fbID)
	if err != nil {
		return DraftDetail{}, err
	}
	d, err := s.detail(r)
	return d, s.internal("draft.read", err)
}

// ListDrafts returns the caller's open drafts of a workspace, newest first.
func (s *Service) ListDrafts(ctx context.Context, workspaceID string) (DraftList, error) {
	p, err := s.requireHuman(ctx, "draft.list")
	if err != nil {
		return DraftList{}, err
	}
	if err := s.workspaceOK(p, workspaceID); err != nil {
		return DraftList{}, err
	}
	rows, err := s.db().QueryContext(ctx, `SELECT `+revisionCols+` FROM revisions r JOIN feedback f ON f.id = r.feedback_id
		WHERE f.workspace_id = ? AND r.owner_id = ? AND r.status = 'DRAFT' ORDER BY r.updated_at DESC LIMIT 100`, workspaceID, p.ID)
	if err != nil {
		return DraftList{}, s.internal("draft.list", err)
	}
	var revs []*revisionRow
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			_ = rows.Close()
			return DraftList{}, s.internal("draft.list", err)
		}
		revs = append(revs, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return DraftList{}, s.internal("draft.list", err)
	}
	_ = rows.Close()
	out := DraftList{Items: []DraftDetail{}}
	for _, r := range revs {
		d, err := s.detail(r)
		if err != nil {
			return DraftList{}, s.internal("draft.list", err)
		}
		out.Items = append(out.Items, d)
	}
	return out, nil
}

// PatchDraft replaces the task and/or the annotations of the draft. ifMatch
// is the ETag from the last read; every change yields a new ETag and drops
// the uploaded preview.
func (s *Service) PatchDraft(ctx context.Context, fbID, ifMatch string, in PatchDraftInput, key string) (Draft, error) {
	p, r, err := s.authDraft(ctx, "draft.update", fbID)
	if err != nil {
		return Draft{}, err
	}
	var taskJSON, annotJSON string
	if in.Task != nil {
		t := *in.Task
		if err := validateTask(&t); err != nil {
			return Draft{}, err
		}
		b, _ := json.Marshal(t)
		taskJSON = string(b)
	}
	if in.Annotations != nil {
		canon, _, err := ParseAnnotations(*in.Annotations)
		if err != nil {
			return Draft{}, err
		}
		b, _ := json.Marshal(canon)
		annotJSON = string(b)
	}
	input, _ := json.Marshal(in)
	input = append(input, []byte("|"+ifMatch)...)
	return idempotent(ctx, s, p, "draft.update", fbID, key, input, func(rec *idemRec) (Draft, error) {
		cur, err := latestRevision(ctx, s.db(), fbID)
		if err != nil {
			return Draft{}, s.internal("draft.update", err)
		}
		r = cur
		if r.Status != revisionDraft {
			return Draft{}, ErrNotFound
		}
		if err := checkETag(ifMatch, r); err != nil {
			return Draft{}, err
		}
		if in.Task == nil && in.Annotations == nil {
			return r.draft(), nil
		}
		now := s.nowUTC()
		task, annots := r.TaskJSON, r.AnnotationsJSON
		if in.Task != nil {
			task = taskJSON
		}
		if in.Annotations != nil {
			annots = annotJSON
		}
		var out Draft
		err = s.st.DB.InTx(ctx, func(tx *sql.Tx) error {
			exp := s.ms(now.Add(s.lim.DraftTTL))
			res, err := tx.ExecContext(ctx, `UPDATE revisions SET task_json = ?, annotations_json = ?, version = version + 1,
				preview_key = NULL, preview_size = 0, preview_version = 0, updated_at = ?, expires_at = ?, warned = 0
				WHERE feedback_id = ? AND revision = ? AND status = 'DRAFT' AND version = ?`,
				task, annots, s.ms(now), exp, r.FeedbackID, r.Revision, r.Version)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return ErrPreconditionFailed
			}
			if _, err := tx.ExecContext(ctx, `UPDATE captures SET expires_at = ?, updated_at = ? WHERE id = ?`, exp, s.ms(now), r.CaptureID); err != nil {
				return err
			}
			out = Draft{FeedbackID: r.FeedbackID, Revision: r.Revision, ETag: etagFor(r.FeedbackID, r.Revision, r.Version+1), State: DraftState}
			if err := s.emit(ctx, tx, p, evSpec{Workspace: r.WorkspaceID, Category: "feedback", Action: "draft.updated", SubjectType: "feedback", SubjectID: r.FeedbackID, Revision: r.Revision, Visibility: storage.VisPrivate}); err != nil {
				return err
			}
			return rec.save(ctx, tx, out)
		})
		if err != nil {
			return Draft{}, s.internal("draft.update", err)
		}
		if r.PreviewKey.Valid {
			_ = s.st.Private.Delete(r.PreviewKey.String)
		}
		s.wake()
		return out, nil
	})
}

// UploadPreview binds the client-rendered annotated PNG to the current draft
// version. The upload is untrusted: it is decoded under limits, must match
// the capture's pixel size and is re-encoded. Sharing happens only through
// Publish, which redacts it again.
func (s *Service) UploadPreview(ctx context.Context, fbID, ifMatch string, data []byte, key string) (DraftDetail, error) {
	p, r, err := s.authDraft(ctx, "draft.preview", fbID)
	if err != nil {
		return DraftDetail{}, err
	}
	input := []byte(sha256Hex(data) + "|" + ifMatch)
	return idempotent(ctx, s, p, "draft.preview", fbID, key, input, func(rec *idemRec) (DraftDetail, error) {
		cur, err := latestRevision(ctx, s.db(), fbID)
		if err != nil {
			return DraftDetail{}, s.internal("draft.preview", err)
		}
		r = cur
		if r.Status != revisionDraft {
			return DraftDetail{}, ErrNotFound
		}
		if err := checkETag(ifMatch, r); err != nil {
			return DraftDetail{}, err
		}
		c, err := getCapture(ctx, s.db(), r.CaptureID)
		if err != nil {
			return DraftDetail{}, s.internal("draft.preview", err)
		}
		png, w, h, err := s.reencode(ctx, data)
		if err != nil {
			return DraftDetail{}, err
		}
		if w != c.Width || h != c.Height {
			return DraftDetail{}, badRequest("the preview must have the same pixel size as the capture")
		}
		objKey, err := storage.NewObjectKey()
		if err != nil {
			return DraftDetail{}, s.internal("draft.preview", err)
		}
		if _, err := s.st.Private.Put(objKey, png); err != nil {
			return DraftDetail{}, s.internal("draft.preview", err)
		}
		now := s.nowUTC()
		var out DraftDetail
		err = s.st.DB.InTx(ctx, func(tx *sql.Tx) error {
			extra := int64(len(png)) - r.PreviewSize
			if err := s.checkBytesQuota(ctx, tx, r.WorkspaceID, extra); err != nil {
				return err
			}
			exp := s.ms(now.Add(s.lim.DraftTTL))
			res, err := tx.ExecContext(ctx, `UPDATE revisions SET preview_key = ?, preview_size = ?, preview_version = version, updated_at = ?, expires_at = ?, warned = 0
				WHERE feedback_id = ? AND revision = ? AND status = 'DRAFT' AND version = ?`,
				objKey, len(png), s.ms(now), exp, r.FeedbackID, r.Revision, r.Version)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return ErrPreconditionFailed
			}
			if _, err := tx.ExecContext(ctx, `UPDATE captures SET expires_at = ?, updated_at = ? WHERE id = ?`, exp, s.ms(now), r.CaptureID); err != nil {
				return err
			}
			row, err := getRevision(ctx, tx, r.FeedbackID, r.Revision)
			if err != nil {
				return err
			}
			out, err = s.detail(row)
			if err != nil {
				return err
			}
			if err := s.emit(ctx, tx, p, evSpec{Workspace: r.WorkspaceID, Category: "feedback", Action: "draft.preview", SubjectType: "feedback", SubjectID: r.FeedbackID, Revision: r.Revision, Visibility: storage.VisPrivate}); err != nil {
				return err
			}
			return rec.save(ctx, tx, out)
		})
		if err != nil {
			_ = s.st.Private.Delete(objKey)
			return DraftDetail{}, s.internal("draft.preview", err)
		}
		if r.PreviewKey.Valid {
			_ = s.st.Private.Delete(r.PreviewKey.String)
		}
		s.wake()
		return out, nil
	})
}

type sharedImages struct {
	Redacted  []byte
	Annotated []byte
	Width     int
	Height    int
}

// renderShared produces the exact images publication would share: the raw
// base with solid redactions applied, and the preview with the same
// redactions re-applied, both re-encoded without metadata. It is used for the
// pre-publish view and by Publish, so what the human sees is what ships.
func (s *Service) renderShared(ctx context.Context, r *revisionRow, c *captureRow, needAnnotated bool) (*sharedImages, error) {
	var annots []json.RawMessage
	if err := json.Unmarshal([]byte(r.AnnotationsJSON), &annots); err != nil {
		return nil, err
	}
	set, err := parseAnnotationSet(annots)
	if err != nil {
		return nil, err
	}
	if !c.ObjectKey.Valid {
		return nil, ErrCaptureNotReady
	}
	base, err := s.st.Private.Read(c.ObjectKey.String, s.lim.MaxImageBytes)
	if err != nil {
		return nil, err
	}
	var preview []byte
	if needAnnotated {
		if !r.previewReady() {
			return nil, ErrPreviewRequired
		}
		preview, err = s.st.Private.Read(r.PreviewKey.String, s.lim.MaxImageBytes)
		if err != nil {
			return nil, err
		}
	}
	release, err := s.acquireImage(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	out := &sharedImages{}
	one := func(src []byte, isBase bool) ([]byte, int, int, error) {
		img, err := decodePNG(src, s.lim)
		if err != nil {
			return nil, 0, 0, err
		}
		if img.Bounds().Dx() != c.Width || img.Bounds().Dy() != c.Height {
			return nil, 0, 0, badRequest("image size no longer matches the capture")
		}
		if isBase {
			// Blur and pixelate belong to the base image only: the uploaded
			// annotated PNG already contains them, so it is re-redacted but
			// never filtered a second time.
			if err := shareBase(img, set.Redactions, set.Effects); err != nil {
				return nil, 0, 0, err
			}
		} else {
			px := applyRedactions(img, set.Redactions)
			if !verifyRedactions(img, px) {
				return nil, 0, 0, errors.New("feedback: redaction verification failed")
			}
		}
		b, err := encodePNG(img, s.lim.MaxImageBytes)
		return b, img.Bounds().Dx(), img.Bounds().Dy(), err
	}
	out.Redacted, out.Width, out.Height, err = one(base, true)
	if err != nil {
		return nil, err
	}
	if needAnnotated {
		out.Annotated, _, _, err = one(preview, false)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SharedPreview returns the server-redacted image publication would share,
// name being screenshot.redacted.png or screenshot.annotated.png. The latter
// needs an uploaded preview.
func (s *Service) SharedPreview(ctx context.Context, fbID, name string) ([]byte, error) {
	if name != ArtifactRedacted && name != ArtifactAnnotated {
		return nil, ErrNotFound
	}
	_, r, err := s.authDraft(ctx, "draft.shared_preview", fbID)
	if err != nil {
		return nil, err
	}
	c, err := getCapture(ctx, s.db(), r.CaptureID)
	if err != nil {
		return nil, s.internal("draft.shared_preview", err)
	}
	imgs, err := s.renderShared(ctx, r, c, name == ArtifactAnnotated)
	if err != nil {
		return nil, s.internal("draft.shared_preview", err)
	}
	if name == ArtifactAnnotated {
		return imgs.Annotated, nil
	}
	return imgs.Redacted, nil
}

// DiscardDraft deletes the owner's open draft and its preview. The capture is
// kept (unbound) until its TTL so it can start another draft.
func (s *Service) DiscardDraft(ctx context.Context, fbID string) error {
	p, r, err := s.authDraft(ctx, "draft.discard", fbID)
	if err != nil {
		return err
	}
	err = s.st.DB.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM revisions WHERE feedback_id = ? AND revision = ? AND status = 'DRAFT'`, r.FeedbackID, r.Revision); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE captures SET feedback_id = NULL, updated_at = ? WHERE id = ? AND feedback_id = ?`, s.ms(s.nowUTC()), r.CaptureID, r.FeedbackID); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM revisions WHERE feedback_id = ?`, r.FeedbackID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM feedback WHERE id = ?`, r.FeedbackID); err != nil {
				return err
			}
		}
		return s.emit(ctx, tx, p, evSpec{Workspace: r.WorkspaceID, Category: "feedback", Action: "draft.discarded", SubjectType: "feedback", SubjectID: r.FeedbackID, Revision: r.Revision, Visibility: storage.VisPrivate})
	})
	if err != nil {
		return s.internal("draft.discard", err)
	}
	if r.PreviewKey.Valid {
		_ = s.st.Private.Delete(r.PreviewKey.String)
	}
	s.wake()
	return nil
}

// NewRevision opens a new draft revision of published feedback. Without a
// captureId the base is a private copy of the published redacted image (the
// raw capture no longer exists after publication) and task and annotations
// are carried over; with an owned ready capture the annotations start empty.
func (s *Service) NewRevision(ctx context.Context, fbID string, in NewRevisionInput, key string) (Draft, error) {
	p, err := s.requireHuman(ctx, "revision.create")
	if err != nil {
		return Draft{}, err
	}
	last, err := latestRevision(ctx, s.db(), fbID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Draft{}, ErrNotFound
		}
		return Draft{}, s.internal("revision.create", err)
	}
	if !p.InWorkspace(last.WorkspaceID) {
		s.auditDenied(ctx, p, "revision.create", last.WorkspaceID, "feedback", fbID)
		return Draft{}, ErrNotFound
	}
	input, _ := json.Marshal(in)
	return idempotent(ctx, s, p, "revision.create", fbID, key, input, func(rec *idemRec) (Draft, error) {
		last, err := latestRevision(ctx, s.db(), fbID)
		if err != nil {
			return Draft{}, s.internal("revision.create", err)
		}
		if last.Status == revisionDraft {
			return Draft{}, ErrDraftExists
		}
		now := s.nowUTC()
		exp := s.ms(now.Add(s.lim.DraftTTL))
		newRev := last.Revision + 1
		task, annots := last.TaskJSON, last.AnnotationsJSON
		var (
			capID    string
			newCap   *captureRow
			objKey   string
			capBound = in.CaptureID != ""
		)
		if capBound {
			annots = "[]"
			if _, err := s.loadOwnedReadyCapture(ctx, s.db(), p, last.WorkspaceID, in.CaptureID); err != nil {
				return Draft{}, err
			}
			capID = in.CaptureID
		} else {
			// The published base image already contains the blur and pixelate
			// effects; keeping their annotations would filter it a second time.
			if annots, err = withoutBakedEffects(annots); err != nil {
				return Draft{}, s.internal("revision.create", err)
			}
			parent, err := getCapture(ctx, s.db(), last.CaptureID)
			if err != nil {
				return Draft{}, s.internal("revision.create", err)
			}
			art, err := s.st.Artifacts.Read(last.FeedbackID, last.Revision, ArtifactRedacted, s.lim.MaxImageBytes)
			if err != nil {
				return Draft{}, s.internal("revision.create", err)
			}
			if last.RedactedSHA.Valid && sha256Hex(art) != last.RedactedSHA.String {
				return Draft{}, s.internal("revision.create", errors.New("published redacted image hash mismatch"))
			}
			png, w, h, err := s.reencode(ctx, art)
			if err != nil {
				return Draft{}, err
			}
			objKey, err = storage.NewObjectKey()
			if err != nil {
				return Draft{}, s.internal("revision.create", err)
			}
			sum, err := s.st.Private.Put(objKey, png)
			if err != nil {
				return Draft{}, s.internal("revision.create", err)
			}
			pt, _ := json.Marshal(ParentTransform{Kind: TransformRevision, ParentCaptureID: parent.ID, ParentWidthPx: w, ParentHeightPx: h,
				CropWidthPx: w, CropHeightPx: h, FromFeedbackID: last.FeedbackID, FromRevision: last.Revision})
			nc := *parent
			nc.ID = newID("cap_")
			nc.OwnerID = p.ID
			nc.Status = StatusReady
			nc.Width, nc.Height = w, h
			nc.ObjectKey, nc.SHA, nc.Size = nullStr(objKey), nullStr(sum), int64(len(png))
			nc.FeedbackID = sql.NullString{}
			nc.ParentJSON = sql.NullString{String: string(pt), Valid: true}
			nc.Created, nc.Updated, nc.Expires = s.ms(now), s.ms(now), exp
			newCap = &nc
			capID = nc.ID
		}
		out := Draft{FeedbackID: fbID, Revision: newRev, ETag: etagFor(fbID, newRev, 1), State: DraftState}
		err = s.st.DB.InTx(ctx, func(tx *sql.Tx) error {
			if err := s.draftQuota(ctx, tx, last.WorkspaceID); err != nil {
				return err
			}
			if newCap != nil {
				if err := s.checkCaptureQuota(ctx, tx, last.WorkspaceID, newCap.Size); err != nil {
					return err
				}
				if err := s.insertCapture(ctx, tx, newCap); err != nil {
					return err
				}
			} else if _, err := s.loadOwnedReadyCapture(ctx, tx, p, last.WorkspaceID, capID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO revisions (feedback_id, revision, owner_id, status, capture_id, task_json, annotations_json, version, created_at, updated_at, expires_at)
				VALUES (?, ?, ?, 'DRAFT', ?, ?, ?, 1, ?, ?, ?)`, fbID, newRev, p.ID, capID, task, annots, s.ms(now), s.ms(now), exp); err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `UPDATE captures SET feedback_id = ?, updated_at = ?, expires_at = ? WHERE id = ? AND feedback_id IS NULL`, fbID, s.ms(now), exp, capID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return ErrCaptureInUse
			}
			if err := s.emit(ctx, tx, p, evSpec{Workspace: last.WorkspaceID, Category: "feedback", Action: "revision.created", SubjectType: "feedback", SubjectID: fbID, Revision: newRev, Visibility: storage.VisPrivate}); err != nil {
				return err
			}
			return rec.save(ctx, tx, out)
		})
		if err != nil {
			if objKey != "" {
				_ = s.st.Private.Delete(objKey)
			}
			return Draft{}, s.internal("revision.create", err)
		}
		s.wake()
		return out, nil
	})
}
