package feedback

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/storage"
)

// environmentDoc is environment.json: server-derived capture context only.
// Cookies, storage, DOM and request bodies are never collected.
type environmentDoc struct {
	SchemaVersion     string           `json:"schemaVersion"`
	Source            string           `json:"source"`
	Provenance        string           `json:"provenance"`
	CapturedAt        time.Time        `json:"capturedAt"`
	ServerReceivedAt  time.Time        `json:"serverReceivedAt"`
	RuntimeGeneration string           `json:"runtimeGeneration"`
	Consistency       string           `json:"consistency"`
	TargetID          *string          `json:"targetId"`
	NavigationID      *string          `json:"navigationId"`
	ImageWidthPx      int              `json:"imageWidthPx"`
	ImageHeightPx     int              `json:"imageHeightPx"`
	ParentTransform   *ParentTransform `json:"parentTransform,omitempty"`
	Page              *PageInfo        `json:"page,omitempty"`
}

// annotationsDoc is annotations.json: the vector annotations together with
// the pixel size of the immutable source image they are normalized against.
type annotationsDoc struct {
	SchemaVersion string            `json:"schemaVersion"`
	ImageWidthPx  int               `json:"imageWidthPx"`
	ImageHeightPx int               `json:"imageHeightPx"`
	Annotations   []json.RawMessage `json:"annotations"`
}

// documentSchemaVersion returns the schemas/feedback.schema.json version a
// published revision must declare. Any 0.3.0 annotation feature (the
// highlight, spotlight, blur and pixelate kinds, an arrow control point, or
// the opacity, effectSizePx, arrowStyle, textStyle and smoothing styles)
// selects SchemaVersionAdvanced. Otherwise standalone drawings, the ellipse,
// diamond and line annotation kinds, and a task without a description (or a
// bug or visual-regression task without expected and actual results) exist
// only from SchemaVersion on; all other revisions stay byte-compatible with
// SchemaVersionLegacy readers, which require that prose.
// annotations must be the canonical output of ParseAnnotations.
func documentSchemaVersion(source string, annotations []json.RawMessage, task Task) string {
	for _, raw := range annotations {
		if needsAdvancedSchema(raw) {
			return SchemaVersionAdvanced
		}
	}
	if source == SourceDrawing || taskNeedsProseFreeSchema(task) {
		return SchemaVersion
	}
	for _, raw := range annotations {
		var a struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(raw, &a) != nil {
			continue
		}
		switch a.Kind {
		case "ellipse", "diamond", "line":
			return SchemaVersion
		}
	}
	return SchemaVersionLegacy
}

var bundleFileNames = []string{ArtifactFeedback, ArtifactTask, ArtifactAnnots, ArtifactEnv, ArtifactRedacted, ArtifactAnnotated}

func validArtifactName(n string) bool {
	if n == ArtifactManifest {
		return true
	}
	for _, f := range bundleFileNames {
		if f == n {
			return true
		}
	}
	return false
}

func mimeFor(name string) string {
	switch name {
	case ArtifactRedacted, ArtifactAnnotated:
		return mimePNG
	case ArtifactTask:
		return mimeMarkdown
	}
	return mimeJSON
}

// Publish atomically publishes the open draft as an immutable revision. The
// server re-encodes both shared images and re-applies every solid redaction
// to them, writes the whole bundle to a staging directory, renames it into
// place and only then commits the revision, delivery and artifact rows in one
// transaction, so no reader ever sees a partial bundle. The delivery is READY,
// or, when in.Dispatch is set, QUEUED for the agent the server resolves for
// the workspace; a missing or ambiguous recipient fails before anything is
// published and leaves the draft untouched. The raw capture and the private
// preview are deleted afterwards.
func (s *Service) Publish(ctx context.Context, fbID, ifMatch string, in PublishInput, key string) (PublishedFeedback, error) {
	p, err := s.requireHuman(ctx, "feedback.publish")
	if err != nil {
		return PublishedFeedback{}, err
	}
	if !in.HumanApproved {
		return PublishedFeedback{}, badRequest("humanApproved must be true")
	}
	// The mode is part of the request fingerprint, so one key can never be
	// replayed as the other mode. The legacy fingerprint is unchanged.
	fp := []byte(ifMatch)
	if in.Dispatch {
		fp = append(fp, "\x00dispatch"...)
	}
	return idempotent(ctx, s, p, "feedback.publish", fbID, key, fp, func(rec *idemRec) (PublishedFeedback, error) {
		_, r, err := s.authDraft(ctx, "feedback.publish", fbID)
		if err != nil {
			return PublishedFeedback{}, err
		}
		if err := checkETag(ifMatch, r); err != nil {
			return PublishedFeedback{}, err
		}
		var agentID string
		if in.Dispatch {
			if agentID, err = s.resolveAgent(r.WorkspaceID, ""); err != nil {
				return PublishedFeedback{}, err
			}
		}
		var task Task
		if err := json.Unmarshal([]byte(r.TaskJSON), &task); err != nil {
			return PublishedFeedback{}, s.internal("feedback.publish", err)
		}
		if err := prepareTaskForPublish(&task); err != nil {
			return PublishedFeedback{}, err
		}
		var rawAnnots []json.RawMessage
		if err := json.Unmarshal([]byte(r.AnnotationsJSON), &rawAnnots); err != nil {
			return PublishedFeedback{}, s.internal("feedback.publish", err)
		}
		annots, _, err := ParseAnnotations(rawAnnots)
		if err != nil {
			return PublishedFeedback{}, err
		}
		c, err := getCapture(ctx, s.db(), r.CaptureID)
		if err != nil {
			return PublishedFeedback{}, s.internal("feedback.publish", err)
		}
		if c.Status != StatusReady || !c.ObjectKey.Valid {
			return PublishedFeedback{}, ErrCaptureNotReady
		}
		if !r.previewReady() {
			return PublishedFeedback{}, ErrPreviewRequired
		}
		imgs, err := s.renderShared(ctx, r, c, true)
		if err != nil {
			return PublishedFeedback{}, s.internal("feedback.publish", err)
		}
		now := s.nowUTC()
		redactedSHA := sha256Hex(imgs.Redacted)
		annotatedSHA := sha256Hex(imgs.Annotated)

		var page *PageInfo
		if c.PageJSON.Valid {
			var pg PageInfo
			if err := json.Unmarshal([]byte(c.PageJSON.String), &pg); err != nil {
				return PublishedFeedback{}, s.internal("feedback.publish", err)
			}
			page = &pg
		}
		st := c.state()
		capturedAt, receivedAt := fromMS(c.Created), fromMS(c.Created)
		if c.CapturedAt.Valid {
			capturedAt = fromMS(c.CapturedAt.Int64)
		}
		if c.ReceivedAt.Valid {
			receivedAt = fromMS(c.ReceivedAt.Int64)
		}
		schemaVersion := documentSchemaVersion(c.Source, annots, task)
		env, err := marshalCompact(environmentDoc{
			SchemaVersion: schemaVersion, Source: c.Source, Provenance: c.Provenance, CapturedAt: capturedAt, ServerReceivedAt: receivedAt,
			RuntimeGeneration: c.Gen, Consistency: c.Consistency, TargetID: st.TargetID, NavigationID: st.NavigationID,
			ImageWidthPx: c.Width, ImageHeightPx: c.Height, ParentTransform: st.ParentTransform, Page: page,
		})
		if err != nil {
			return PublishedFeedback{}, s.internal("feedback.publish", err)
		}
		annDoc, err := marshalCompact(annotationsDoc{SchemaVersion: schemaVersion, ImageWidthPx: c.Width, ImageHeightPx: c.Height, Annotations: annots})
		if err != nil {
			return PublishedFeedback{}, s.internal("feedback.publish", err)
		}
		pf := PublishedFeedback{
			SchemaVersion: schemaVersion, FeedbackID: r.FeedbackID, Revision: r.Revision, WorkspaceID: r.WorkspaceID, PublishedAt: now,
			Capture: PublishedCapture{
				ID: c.ID, Source: c.Source, Provenance: c.Provenance, CapturedAt: capturedAt, ServerReceivedAt: receivedAt,
				RuntimeGeneration: c.Gen, TargetID: st.TargetID, NavigationID: st.NavigationID, Consistency: c.Consistency,
				ImageWidthPx: c.Width, ImageHeightPx: c.Height, RedactedImageSHA256: redactedSHA, Page: page,
			},
			Task: task, Annotations: annots,
			Artifacts: []ArtifactRef{
				{Name: ArtifactRedacted, MimeType: mimePNG, SHA256: redactedSHA, SizeBytes: int64(len(imgs.Redacted))},
				{Name: ArtifactAnnotated, MimeType: mimePNG, SHA256: annotatedSHA, SizeBytes: int64(len(imgs.Annotated))},
				{Name: ArtifactEnv, MimeType: mimeJSON, SHA256: sha256Hex(env), SizeBytes: int64(len(env))},
			},
			SharePolicy: SharePolicy{HumanApproved: true, RedactionsApplied: true, IncludesRawImage: false, DiagnosticsIncluded: false},
		}
		fbJSON, err := marshalCompact(pf)
		if err != nil {
			return PublishedFeedback{}, s.internal("feedback.publish", err)
		}
		taskMD := renderTaskMD(&pf)
		contents := map[string][]byte{
			ArtifactFeedback: fbJSON, ArtifactTask: taskMD, ArtifactAnnots: annDoc, ArtifactEnv: env,
			ArtifactRedacted: imgs.Redacted, ArtifactAnnotated: imgs.Annotated,
		}
		manifest := Manifest{SchemaVersion: schemaVersion, FeedbackID: r.FeedbackID, Revision: r.Revision, WorkspaceID: r.WorkspaceID, PublishedAt: now}
		files := make([]storage.File, 0, len(bundleFileNames)+1)
		entries := make([]FileEntry, 0, len(bundleFileNames)+1)
		var total int64
		for _, name := range bundleFileNames {
			data := contents[name]
			manifest.Files = append(manifest.Files, FileEntry{Name: name, MimeType: mimeFor(name), SHA256: sha256Hex(data), SizeBytes: int64(len(data))})
			files = append(files, storage.File{Name: name, Data: data})
			entries = append(entries, manifest.Files[len(manifest.Files)-1])
			total += int64(len(data))
		}
		manifestJSON, err := marshalCompact(manifest)
		if err != nil {
			return PublishedFeedback{}, s.internal("feedback.publish", err)
		}
		files = append(files, storage.File{Name: ArtifactManifest, Data: manifestJSON})
		entries = append(entries, FileEntry{Name: ArtifactManifest, MimeType: mimeJSON, SHA256: sha256Hex(manifestJSON), SizeBytes: int64(len(manifestJSON))})
		total += int64(len(manifestJSON))

		err = s.st.Artifacts.Publish(r.FeedbackID, r.Revision, files)
		if errors.Is(err, storage.ErrExists) {
			// A directory without a committed revision is the leftover of a
			// crash between rename and commit: this revision is still a draft.
			if rerr := s.st.Artifacts.Remove(r.FeedbackID, r.Revision); rerr != nil {
				return PublishedFeedback{}, s.internal("feedback.publish", rerr)
			}
			err = s.st.Artifacts.Publish(r.FeedbackID, r.Revision, files)
		}
		if err != nil {
			s.artifactFailure(ctx, r.WorkspaceID, "artifact.publish_failed", r.FeedbackID, r.Revision)
			return PublishedFeedback{}, s.internal("feedback.publish", err)
		}
		err = s.st.DB.InTx(ctx, func(tx *sql.Tx) error {
			var shared int64
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(a.size_bytes), 0) FROM artifacts a JOIN feedback f ON f.id = a.feedback_id WHERE f.workspace_id = ?`, r.WorkspaceID).Scan(&shared); err != nil {
				return err
			}
			if s.lim.MaxSharedBytes > 0 && shared+total > s.lim.MaxSharedBytes {
				return ErrQuotaExceeded
			}
			res, err := tx.ExecContext(ctx, `UPDATE revisions SET status = 'PUBLISHED', title = ?, published_at = ?, publisher_id = ?, manifest_sha256 = ?, redacted_sha256 = ?,
				preview_key = NULL, preview_size = 0, updated_at = ?
				WHERE feedback_id = ? AND revision = ? AND status = 'DRAFT' AND version = ?`,
				task.Title, s.ms(now), p.ID, sha256Hex(manifestJSON), redactedSHA, s.ms(now), r.FeedbackID, r.Revision, r.Version)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return ErrPreconditionFailed
			}
			for _, e := range entries {
				if _, err := tx.ExecContext(ctx, `INSERT INTO artifacts (feedback_id, revision, name, mime_type, sha256, size_bytes) VALUES (?, ?, ?, ?, ?, ?)`,
					r.FeedbackID, r.Revision, e.Name, e.MimeType, e.SHA256, e.SizeBytes); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO deliveries (feedback_id, revision, workspace_id, state, created_at, updated_at) VALUES (?, ?, ?, 'READY', ?, ?)`,
				r.FeedbackID, r.Revision, r.WorkspaceID, s.ms(now), s.ms(now)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_history (feedback_id, revision, from_state, to_state, actor_id, actor_role, at) VALUES (?, ?, 'DRAFT', 'READY', ?, ?, ?)`,
				r.FeedbackID, r.Revision, p.ID, p.Role, s.ms(now)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE captures SET object_key = NULL, size_bytes = 0, updated_at = ? WHERE id = ?`, s.ms(now), c.ID); err != nil {
				return err
			}
			if err := s.emit(ctx, tx, p, evSpec{Workspace: r.WorkspaceID, Category: "feedback", Action: "feedback.published", SubjectType: "feedback", SubjectID: r.FeedbackID, Revision: r.Revision, Visibility: storage.VisWorkspace}); err != nil {
				return err
			}
			if agentID != "" {
				// READY -> QUEUED in the same transaction: history and the
				// assigned-only event are exactly those of a separate dispatch.
				d := &deliveryRow{FeedbackID: r.FeedbackID, Revision: r.Revision, WorkspaceID: r.WorkspaceID, State: StateReady, AgentID: agentID}
				if err := s.step(ctx, tx, p, d, []string{StateReady}, StateQueued, "delivery.dispatched", "", "agent_id = ?, queued_at = ?", agentID, s.ms(now)); err != nil {
					return err
				}
			}
			return rec.save(ctx, tx, pf)
		})
		if err != nil {
			_ = s.st.Artifacts.Remove(r.FeedbackID, r.Revision)
			var fe *Error
			if !errors.As(err, &fe) {
				s.artifactFailure(ctx, r.WorkspaceID, "artifact.publish_failed", r.FeedbackID, r.Revision)
			}
			return PublishedFeedback{}, s.internal("feedback.publish", err)
		}
		// Raw evidence is no longer needed; a new revision starts from the
		// published redacted image.
		if c.ObjectKey.Valid {
			_ = s.st.Private.Delete(c.ObjectKey.String)
		}
		if r.PreviewKey.Valid {
			_ = s.st.Private.Delete(r.PreviewKey.String)
		}
		s.wake()
		return pf, nil
	})
}
