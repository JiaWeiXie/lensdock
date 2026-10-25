package feedback

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"image"
	"math"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/storage"
)

type captureRow struct {
	ID, WorkspaceID, OwnerID                     string
	Source, Provenance, Status, Consistency, Gen string
	TargetID, NavigationID                       sql.NullString
	LeaseID, PriorLeaseID                        sql.NullString
	CapturedAt, ReceivedAt                       sql.NullInt64
	Width, Height                                int
	PageJSON, ParentJSON                         sql.NullString
	ObjectKey, SHA, FeedbackID                   sql.NullString
	Size                                         int64
	Created, Updated, Expires                    int64
}

const captureCols = `id, workspace_id, owner_id, source, provenance, status, consistency, runtime_generation,
	target_id, navigation_id, lease_id, prior_lease_id, captured_at, server_received_at, width, height,
	page_json, parent_json, object_key, sha256, size_bytes, feedback_id, created_at, updated_at, expires_at`

type scanner interface{ Scan(dest ...any) error }

func scanCapture(sc scanner) (*captureRow, error) {
	var c captureRow
	err := sc.Scan(&c.ID, &c.WorkspaceID, &c.OwnerID, &c.Source, &c.Provenance, &c.Status, &c.Consistency, &c.Gen,
		&c.TargetID, &c.NavigationID, &c.LeaseID, &c.PriorLeaseID, &c.CapturedAt, &c.ReceivedAt, &c.Width, &c.Height,
		&c.PageJSON, &c.ParentJSON, &c.ObjectKey, &c.SHA, &c.Size, &c.FeedbackID, &c.Created, &c.Updated, &c.Expires)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func getCapture(ctx context.Context, q storage.Querier, id string) (*captureRow, error) {
	c, err := scanCapture(q.QueryRowContext(ctx, `SELECT `+captureCols+` FROM captures WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

func (c *captureRow) state() CaptureState {
	st := CaptureState{
		CaptureID: c.ID, WorkspaceID: c.WorkspaceID, Status: c.Status, Consistency: c.Consistency,
		Source: c.Source, Provenance: c.Provenance, RuntimeGeneration: c.Gen,
		TargetID: strPtr(c.TargetID), NavigationID: strPtr(c.NavigationID),
		CapturedAt: ptrTime(c.CapturedAt), ServerReceivedAt: ptrTime(c.ReceivedAt),
		ExpiresAt: fromMS(c.Expires),
	}
	if c.Width > 0 && c.Height > 0 {
		st.ImageWidthPx, st.ImageHeightPx = c.Width, c.Height
	}
	if c.FeedbackID.Valid {
		st.FeedbackID = c.FeedbackID.String
	}
	if c.PageJSON.Valid {
		var pg PageInfo
		if json.Unmarshal([]byte(c.PageJSON.String), &pg) == nil {
			st.Page = &pg
		}
	}
	if c.ParentJSON.Valid {
		var pt ParentTransform
		if json.Unmarshal([]byte(c.ParentJSON.String), &pt) == nil {
			st.ParentTransform = &pt
		}
	}
	return st
}

func (s *Service) insertCapture(ctx context.Context, q storage.Querier, c *captureRow) error {
	_, err := q.ExecContext(ctx, `INSERT INTO captures (`+captureCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.WorkspaceID, c.OwnerID, c.Source, c.Provenance, c.Status, c.Consistency, c.Gen,
		c.TargetID, c.NavigationID, c.LeaseID, c.PriorLeaseID, c.CapturedAt, c.ReceivedAt, c.Width, c.Height,
		c.PageJSON, c.ParentJSON, c.ObjectKey, c.SHA, c.Size, c.FeedbackID, c.Created, c.Updated, c.Expires)
	return err
}

// requireHuman checks the role and workspace scope for human-only actions.
func (s *Service) requireHuman(ctx context.Context, action string) (Principal, error) {
	p, err := requirePrincipal(ctx)
	if err != nil {
		return p, err
	}
	if !p.IsHuman() {
		s.auditDenied(ctx, p, action, p.WorkspaceID, "role", p.Role)
		return p, ErrForbidden
	}
	return p, nil
}

// authCapture loads a capture and checks that p owns it. Other humans and
// other workspaces get NOT_FOUND so existence is not confirmed.
func (s *Service) authCapture(ctx context.Context, action, id string) (Principal, *captureRow, error) {
	p, err := s.requireHuman(ctx, action)
	if err != nil {
		return p, nil, err
	}
	if len(id) < 1 || len(id) > 128 {
		return p, nil, ErrNotFound
	}
	c, err := getCapture(ctx, s.db(), id)
	if err != nil {
		return p, nil, s.internal(action, err)
	}
	if !p.InWorkspace(c.WorkspaceID) || c.OwnerID != p.ID {
		s.auditDenied(ctx, p, action, c.WorkspaceID, "capture", c.ID)
		return p, nil, ErrNotFound
	}
	return p, c, nil
}

func (s *Service) privateBytes(ctx context.Context, q storage.Querier, ws string) (int64, error) {
	var a, b int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes), 0) FROM captures WHERE workspace_id = ? AND object_key IS NOT NULL`, ws).Scan(&a); err != nil {
		return 0, err
	}
	err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(r.preview_size), 0) FROM revisions r JOIN feedback f ON f.id = r.feedback_id
		WHERE f.workspace_id = ? AND r.status = 'DRAFT' AND r.preview_key IS NOT NULL`, ws).Scan(&b)
	return a + b, err
}

// checkCaptureQuota blocks new captures when the workspace is full; it never
// deletes existing evidence.
func (s *Service) checkCaptureQuota(ctx context.Context, q storage.Querier, ws string, addBytes int64) error {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM captures WHERE workspace_id = ? AND status != 'failed'
		AND (object_key IS NOT NULL OR status IN ('awaiting-upload','capturing'))`, ws).Scan(&n); err != nil {
		return err
	}
	if n >= s.lim.MaxOpenCaptures {
		return ErrQuotaExceeded
	}
	return s.checkBytesQuota(ctx, q, ws, addBytes)
}

func (s *Service) checkBytesQuota(ctx context.Context, q storage.Querier, ws string, addBytes int64) error {
	if addBytes <= 0 {
		return nil
	}
	used, err := s.privateBytes(ctx, q, ws)
	if err != nil {
		return err
	}
	if used+addBytes > s.lim.MaxPrivateBytes {
		return ErrQuotaExceeded
	}
	return nil
}

// CreateCapture reserves a desktop capture (awaiting the PNG upload), captures
// through Options.ViewportCapturer for source page-viewport, or generates a
// blank standalone drawing canvas for source drawing. A drawing is a ready
// opaque white PNG created entirely by the Gateway: no browser, runtime, lease
// or target is involved, so it works while the browser is unavailable. Human
// or admin only.
func (s *Service) CreateCapture(ctx context.Context, workspaceID string, in CreateCaptureInput, key string) (CaptureState, error) {
	p, err := s.requireHuman(ctx, "capture.create")
	if err != nil {
		return CaptureState{}, err
	}
	if err := s.workspaceOK(p, workspaceID); err != nil {
		return CaptureState{}, err
	}
	switch in.Source {
	case SourceDesktop:
		if in.TargetID != "" || in.LeaseID != "" || in.AllowInterrupt || in.ImageWidthPx != 0 || in.ImageHeightPx != 0 {
			return CaptureState{}, badRequest("desktop captures take no targetId, leaseId, allowInterrupt or image size")
		}
	case SourceViewport:
		if in.TargetID == "" || len(in.TargetID) > 256 || in.LeaseID == "" || len(in.LeaseID) > 128 {
			return CaptureState{}, badRequest("page-viewport captures require targetId and leaseId")
		}
		if in.ImageWidthPx != 0 || in.ImageHeightPx != 0 {
			return CaptureState{}, badRequest("page-viewport captures take no image size")
		}
	case SourceDrawing:
		if in.TargetID != "" || in.LeaseID != "" || in.AllowInterrupt {
			return CaptureState{}, badRequest("drawings take no targetId, leaseId or allowInterrupt")
		}
		if err := s.checkDrawingSize(in.ImageWidthPx, in.ImageHeightPx); err != nil {
			return CaptureState{}, err
		}
	default:
		return CaptureState{}, badRequest("source must be desktop, page-viewport or drawing")
	}
	input, _ := json.Marshal(in)
	return idempotent(ctx, s, p, "capture.create", workspaceID, key, input, func(rec *idemRec) (CaptureState, error) {
		if in.Source == SourceDrawing {
			return s.createDrawing(ctx, p, workspaceID, in, rec)
		}
		if in.Source == SourceViewport {
			if s.opt.ViewportCapturer == nil {
				return CaptureState{}, ErrViewportUnavailable
			}
			ing, err := s.opt.ViewportCapturer(ctx, ViewportRequest{Principal: p, WorkspaceID: workspaceID, TargetID: in.TargetID, LeaseID: in.LeaseID, AllowInterrupt: in.AllowInterrupt})
			if err != nil {
				var fe *Error
				if errors.As(err, &fe) {
					return CaptureState{}, err
				}
				s.log.Warn("viewport capture failed", "workspace", workspaceID)
				return CaptureState{}, ErrCaptureFailed
			}
			return s.ingestViewport(ctx, p, workspaceID, ing, rec)
		}
		now := s.nowUTC()
		c := &captureRow{
			ID: newID("cap_"), WorkspaceID: workspaceID, OwnerID: p.ID, Source: SourceDesktop, Provenance: ProvClientFB,
			Status: StatusAwaiting, Consistency: ConsUnknown, Gen: s.generation(workspaceID),
			Created: s.ms(now), Updated: s.ms(now), Expires: s.ms(now.Add(s.lim.ReservationTTL)),
		}
		err := s.st.DB.InTx(ctx, func(tx *sql.Tx) error {
			if err := s.checkCaptureQuota(ctx, tx, workspaceID, 0); err != nil {
				return err
			}
			if err := s.insertCapture(ctx, tx, c); err != nil {
				return err
			}
			if err := s.emit(ctx, tx, p, evSpec{Workspace: workspaceID, Category: "capture", Action: "capture.reserved", SubjectType: "capture", SubjectID: c.ID, Visibility: storage.VisPrivate}); err != nil {
				return err
			}
			return rec.save(ctx, tx, c.state())
		})
		if err != nil {
			return CaptureState{}, s.internal("capture.create", err)
		}
		s.wake()
		return c.state(), nil
	})
}

// checkDrawingSize validates the requested drawing size: each side within the
// drawing range and the product's dimension and pixel limits. Nothing is
// clipped or defaulted.
func (s *Service) checkDrawingSize(w, h int) error {
	if w < minDrawingDimension || w > maxDrawingDimension || h < minDrawingDimension || h > maxDrawingDimension {
		return badRequest("imageWidthPx and imageHeightPx must each be integers from 64 to 4096")
	}
	if w > s.lim.MaxDimension || h > s.lim.MaxDimension || int64(w)*int64(h) > int64(s.lim.MaxPixels) {
		return ErrImageTooLarge
	}
	return nil
}

// createDrawing generates the blank canvas and registers it as a ready
// capture. The runtime is never consulted: runtime generation, target,
// navigation, lease and page are all absent by construction.
func (s *Service) createDrawing(ctx context.Context, p Principal, ws string, in CreateCaptureInput, rec *idemRec) (CaptureState, error) {
	png, err := s.blankPNG(ctx, in.ImageWidthPx, in.ImageHeightPx)
	if err != nil {
		return CaptureState{}, err
	}
	objKey, err := storage.NewObjectKey()
	if err != nil {
		return CaptureState{}, s.internal("capture.drawing", err)
	}
	sum, err := s.st.Private.Put(objKey, png)
	if err != nil {
		return CaptureState{}, s.internal("capture.drawing", err)
	}
	now := s.nowUTC()
	c := &captureRow{
		ID: newID("cap_"), WorkspaceID: ws, OwnerID: p.ID, Source: SourceDrawing, Provenance: ProvGenerated,
		Status: StatusReady, Consistency: ConsUnknown, Gen: "",
		CapturedAt: sql.NullInt64{Int64: s.ms(now), Valid: true}, ReceivedAt: sql.NullInt64{Int64: s.ms(now), Valid: true},
		Width: in.ImageWidthPx, Height: in.ImageHeightPx,
		ObjectKey: nullStr(objKey), SHA: nullStr(sum), Size: int64(len(png)),
		Created: s.ms(now), Updated: s.ms(now), Expires: s.ms(now.Add(s.lim.DraftTTL)),
	}
	err = s.st.DB.InTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkCaptureQuota(ctx, tx, ws, int64(len(png))); err != nil {
			return err
		}
		if err := s.insertCapture(ctx, tx, c); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, p, evSpec{Workspace: ws, Category: "capture", Action: "capture.drawing", SubjectType: "capture", SubjectID: c.ID, Visibility: storage.VisPrivate}); err != nil {
			return err
		}
		return rec.save(ctx, tx, c.state())
	})
	if err != nil {
		s.discardObject("capture.drawing", objKey)
		return CaptureState{}, s.internal("capture.drawing", err)
	}
	s.wake()
	return c.state(), nil
}

// blankPNG renders an opaque white w x h canvas as a metadata-free PNG under
// the shared image-worker bound and the shared encoded-size limit.
func (s *Service) blankPNG(ctx context.Context, w, h int) ([]byte, error) {
	release, err := s.acquireImage(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	png, err := encodePNG(img, s.lim.MaxImageBytes)
	if err != nil {
		return nil, s.internal("png.encode", err)
	}
	return png, nil
}

// discardObject removes a private object whose database record was not
// committed. A failure is logged and left to the orphan reconciliation of the
// janitor, which deletes unreferenced files after Limits.OrphanGrace.
func (s *Service) discardObject(op, key string) {
	if err := s.st.Private.Delete(key); err != nil {
		s.log.Warn("private object cleanup failed; orphan reconciliation will remove it", "op", op, "error", err.Error())
	}
}

// ReserveDesktopCapture is CreateCapture for source desktop.
func (s *Service) ReserveDesktopCapture(ctx context.Context, workspaceID, key string) (CaptureState, error) {
	return s.CreateCapture(ctx, workspaceID, CreateCaptureInput{Source: SourceDesktop}, key)
}

func validPage(pg *PageInfo) bool {
	pos := func(f float64) bool { return f > 0 && !math.IsInf(f, 0) && !math.IsNaN(f) }
	fin := func(f float64) bool { return !math.IsInf(f, 0) && !math.IsNaN(f) }
	return pos(pg.ViewportWidthCSS) && pos(pg.ViewportHeightCSS) && pos(pg.PixelsPerCSSX) && pos(pg.PixelsPerCSSY) &&
		fin(pg.ScrollXCSS) && fin(pg.ScrollYCSS) && pg.BrowserVersion != "" && len(pg.BrowserVersion) <= 256
}

// IngestViewportCapture registers a page-viewport capture obtained from the
// runtime. The PNG hash is verified when ExpectedSHA256 is set, the image is
// decoded under limits and re-encoded without metadata, and the page URL and
// title are sanitized again.
func (s *Service) IngestViewportCapture(ctx context.Context, workspaceID string, in ViewportIngest, key string) (CaptureState, error) {
	p, err := s.requireHuman(ctx, "capture.ingest")
	if err != nil {
		return CaptureState{}, err
	}
	if err := s.workspaceOK(p, workspaceID); err != nil {
		return CaptureState{}, err
	}
	meta := in
	meta.PNG = nil
	input, _ := json.Marshal(meta)
	input = append(input, []byte(sha256Hex(in.PNG))...)
	return idempotent(ctx, s, p, "capture.ingest", workspaceID, key, input, func(rec *idemRec) (CaptureState, error) {
		return s.ingestViewport(ctx, p, workspaceID, in, rec)
	})
}

func (s *Service) ingestViewport(ctx context.Context, p Principal, ws string, in ViewportIngest, rec *idemRec) (CaptureState, error) {
	now := s.nowUTC()
	switch in.Consistency {
	case ConsStable, ConsBestEffort, ConsUnknown:
	default:
		return CaptureState{}, badRequest("invalid capture consistency")
	}
	if in.TargetID == "" || len(in.TargetID) > 256 || len(in.NavigationID) > 256 || len(in.LeaseID) > 128 || len(in.PriorLeaseID) > 128 {
		return CaptureState{}, badRequest("invalid capture target or lease identifiers")
	}
	gen := in.RuntimeGeneration
	if gen == "" {
		gen = s.generation(ws)
	}
	if len(gen) > 128 {
		return CaptureState{}, badRequest("invalid runtime generation")
	}
	if in.CapturedAt.IsZero() || in.CapturedAt.After(now.Add(s.lim.CaptureSkew)) {
		return CaptureState{}, badRequest("invalid capture time")
	}
	pg := in.Page
	if !validPage(&pg) {
		return CaptureState{}, badRequest("invalid page capture context")
	}
	pg.URLSanitized = sanitizeURL(pg.URLSanitized)
	pg.TitleSanitized = sanitizeTitle(pg.TitleSanitized)
	if in.ExpectedSHA256 != "" && in.ExpectedSHA256 != sha256Hex(in.PNG) {
		return CaptureState{}, ErrInvalidImage
	}
	png, w, h, err := s.reencode(ctx, in.PNG)
	if err != nil {
		return CaptureState{}, err
	}
	pageJSON, _ := json.Marshal(pg)
	key, err := storage.NewObjectKey()
	if err != nil {
		return CaptureState{}, s.internal("capture.ingest", err)
	}
	sum, err := s.st.Private.Put(key, png)
	if err != nil {
		return CaptureState{}, s.internal("capture.ingest", err)
	}
	c := &captureRow{
		ID: newID("cap_"), WorkspaceID: ws, OwnerID: p.ID, Source: SourceViewport, Provenance: ProvRuntimeCDP,
		Status: StatusReady, Consistency: in.Consistency, Gen: gen,
		TargetID: nullStr(in.TargetID), NavigationID: nullStr(in.NavigationID),
		LeaseID: nullStr(in.LeaseID), PriorLeaseID: nullStr(in.PriorLeaseID),
		CapturedAt: sql.NullInt64{Int64: s.ms(in.CapturedAt.UTC()), Valid: true},
		ReceivedAt: sql.NullInt64{Int64: s.ms(now), Valid: true},
		Width:      w, Height: h, PageJSON: sql.NullString{String: string(pageJSON), Valid: true},
		ObjectKey: nullStr(key), SHA: nullStr(sum), Size: int64(len(png)),
		Created: s.ms(now), Updated: s.ms(now), Expires: s.ms(now.Add(s.lim.DraftTTL)),
	}
	err = s.st.DB.InTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkCaptureQuota(ctx, tx, ws, int64(len(png))); err != nil {
			return err
		}
		if err := s.insertCapture(ctx, tx, c); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, p, evSpec{Workspace: ws, Category: "capture", Action: "capture.viewport", SubjectType: "capture", SubjectID: c.ID, Visibility: storage.VisPrivate}); err != nil {
			return err
		}
		return rec.save(ctx, tx, c.state())
	})
	if err != nil {
		_ = s.st.Private.Delete(key)
		return CaptureState{}, s.internal("capture.ingest", err)
	}
	s.wake()
	return c.state(), nil
}

// reencode decodes under limits and returns the metadata-free re-encoding.
func (s *Service) reencode(ctx context.Context, data []byte) (png []byte, w, h int, err error) {
	release, err := s.acquireImage(ctx)
	if err != nil {
		return nil, 0, 0, err
	}
	defer release()
	img, err := decodePNG(data, s.lim)
	if err != nil {
		return nil, 0, 0, err
	}
	png, err = encodePNG(img, s.lim.MaxImageBytes)
	if err != nil {
		return nil, 0, 0, s.internal("png.encode", err)
	}
	return png, img.Bounds().Dx(), img.Bounds().Dy(), nil
}

// UploadCaptureImage stores the PNG of a desktop capture reservation. capturedAt
// is the client capture time; it must not predate the reservation (a stale
// framebuffer must not pass as a fresh screenshot) nor lie in the future.
func (s *Service) UploadCaptureImage(ctx context.Context, captureID string, data []byte, capturedAt *time.Time, key string) (CaptureState, error) {
	p, c, err := s.authCapture(ctx, "capture.upload", captureID)
	if err != nil {
		return CaptureState{}, err
	}
	input := []byte(sha256Hex(data))
	if capturedAt != nil {
		input = append(input, []byte(capturedAt.UTC().Format(time.RFC3339Nano))...)
	}
	return idempotent(ctx, s, p, "capture.upload", captureID, key, input, func(rec *idemRec) (CaptureState, error) {
		now := s.nowUTC()
		cur, err := getCapture(ctx, s.db(), captureID)
		if err != nil {
			return CaptureState{}, s.internal("capture.upload", err)
		}
		c = cur
		if c.Status != StatusAwaiting {
			return CaptureState{}, invalidState("capture is not awaiting an image")
		}
		if c.Expires < s.ms(now) {
			return CaptureState{}, ErrCaptureExpired
		}
		at := now
		if capturedAt != nil {
			at = capturedAt.UTC()
			if at.After(now.Add(s.lim.CaptureSkew)) || at.Before(fromMS(c.Created).Add(-s.lim.CaptureSkew)) {
				return CaptureState{}, ErrStaleCapture
			}
		}
		png, w, h, err := s.reencode(ctx, data)
		if err != nil {
			return CaptureState{}, err
		}
		objKey, err := storage.NewObjectKey()
		if err != nil {
			return CaptureState{}, s.internal("capture.upload", err)
		}
		sum, err := s.st.Private.Put(objKey, png)
		if err != nil {
			return CaptureState{}, s.internal("capture.upload", err)
		}
		var out CaptureState
		err = s.st.DB.InTx(ctx, func(tx *sql.Tx) error {
			if err := s.checkBytesQuota(ctx, tx, c.WorkspaceID, int64(len(png))); err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `UPDATE captures SET status = 'ready', object_key = ?, sha256 = ?, size_bytes = ?, width = ?, height = ?,
				captured_at = ?, server_received_at = ?, updated_at = ?, expires_at = ? WHERE id = ? AND status = 'awaiting-upload'`,
				objKey, sum, len(png), w, h, s.ms(at), s.ms(now), s.ms(now), s.ms(now.Add(s.lim.DraftTTL)), c.ID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return invalidState("capture is not awaiting an image")
			}
			row, err := getCapture(ctx, tx, c.ID)
			if err != nil {
				return err
			}
			out = row.state()
			if err := s.emit(ctx, tx, p, evSpec{Workspace: c.WorkspaceID, Category: "capture", Action: "capture.uploaded", SubjectType: "capture", SubjectID: c.ID, Visibility: storage.VisPrivate}); err != nil {
				return err
			}
			return rec.save(ctx, tx, out)
		})
		if err != nil {
			_ = s.st.Private.Delete(objKey)
			return CaptureState{}, s.internal("capture.upload", err)
		}
		s.wake()
		return out, nil
	})
}

// GetCapture returns the private status of a capture to its owner.
func (s *Service) GetCapture(ctx context.Context, captureID string) (CaptureState, error) {
	_, c, err := s.authCapture(ctx, "capture.read", captureID)
	if err != nil {
		return CaptureState{}, err
	}
	return c.state(), nil
}

// ReadCaptureImage returns the private PNG of a ready capture to its owner.
func (s *Service) ReadCaptureImage(ctx context.Context, captureID string) ([]byte, error) {
	_, c, err := s.authCapture(ctx, "capture.image", captureID)
	if err != nil {
		return nil, err
	}
	if c.Status != StatusReady || !c.ObjectKey.Valid {
		return nil, ErrCaptureNotReady
	}
	data, err := s.st.Private.Read(c.ObjectKey.String, s.lim.MaxImageBytes)
	if err != nil {
		return nil, s.internal("capture.image", err)
	}
	return data, nil
}

// CropCapture creates a new capture that is a pixel crop of an owned ready
// capture and records the parent transform. The parent is unchanged, so
// existing annotations never drift.
func (s *Service) CropCapture(ctx context.Context, captureID string, in CropInput, key string) (CaptureState, error) {
	p, c, err := s.authCapture(ctx, "capture.crop", captureID)
	if err != nil {
		return CaptureState{}, err
	}
	input, _ := json.Marshal(in)
	return idempotent(ctx, s, p, "capture.crop", captureID, key, input, func(rec *idemRec) (CaptureState, error) {
		cur, err := getCapture(ctx, s.db(), captureID)
		if err != nil {
			return CaptureState{}, s.internal("capture.crop", err)
		}
		c = cur
		if c.Status != StatusReady || !c.ObjectKey.Valid {
			return CaptureState{}, ErrCaptureNotReady
		}
		rect := image.Rect(in.X, in.Y, in.X+in.Width, in.Y+in.Height)
		if in.X < 0 || in.Y < 0 || in.Width < 1 || in.Height < 1 || !rect.In(image.Rect(0, 0, c.Width, c.Height)) {
			return CaptureState{}, badRequest("crop rectangle is outside the image")
		}
		data, err := s.st.Private.Read(c.ObjectKey.String, s.lim.MaxImageBytes)
		if err != nil {
			return CaptureState{}, s.internal("capture.crop", err)
		}
		release, err := s.acquireImage(ctx)
		if err != nil {
			return CaptureState{}, err
		}
		img, err := decodePNG(data, s.lim)
		if err != nil {
			release()
			return CaptureState{}, s.internal("capture.crop", err)
		}
		cropped, err := cropRGBA(img, rect)
		var png []byte
		if err == nil {
			png, err = encodePNG(cropped, s.lim.MaxImageBytes)
		}
		release()
		if err != nil {
			return CaptureState{}, s.internal("capture.crop", err)
		}
		now := s.nowUTC()
		objKey, err := storage.NewObjectKey()
		if err != nil {
			return CaptureState{}, s.internal("capture.crop", err)
		}
		sum, err := s.st.Private.Put(objKey, png)
		if err != nil {
			return CaptureState{}, s.internal("capture.crop", err)
		}
		child := *c
		child.ID = newID("cap_")
		child.FeedbackID = sql.NullString{}
		child.Width, child.Height = rect.Dx(), rect.Dy()
		child.ObjectKey, child.SHA, child.Size = nullStr(objKey), nullStr(sum), int64(len(png))
		child.Created, child.Updated, child.Expires = s.ms(now), s.ms(now), s.ms(now.Add(s.lim.DraftTTL))
		pt, _ := json.Marshal(ParentTransform{Kind: TransformCrop, ParentCaptureID: c.ID, ParentWidthPx: c.Width, ParentHeightPx: c.Height,
			CropXPx: rect.Min.X, CropYPx: rect.Min.Y, CropWidthPx: rect.Dx(), CropHeightPx: rect.Dy()})
		child.ParentJSON = sql.NullString{String: string(pt), Valid: true}
		if c.PageJSON.Valid {
			var pg PageInfo
			if json.Unmarshal([]byte(c.PageJSON.String), &pg) == nil && pg.PixelsPerCSSX > 0 && pg.PixelsPerCSSY > 0 {
				pg.ScrollXCSS += float64(rect.Min.X) / pg.PixelsPerCSSX
				pg.ScrollYCSS += float64(rect.Min.Y) / pg.PixelsPerCSSY
				pg.ViewportWidthCSS = float64(rect.Dx()) / pg.PixelsPerCSSX
				pg.ViewportHeightCSS = float64(rect.Dy()) / pg.PixelsPerCSSY
				b, _ := json.Marshal(pg)
				child.PageJSON = sql.NullString{String: string(b), Valid: true}
			}
		}
		err = s.st.DB.InTx(ctx, func(tx *sql.Tx) error {
			if err := s.checkCaptureQuota(ctx, tx, c.WorkspaceID, int64(len(png))); err != nil {
				return err
			}
			if err := s.insertCapture(ctx, tx, &child); err != nil {
				return err
			}
			if err := s.emit(ctx, tx, p, evSpec{Workspace: c.WorkspaceID, Category: "capture", Action: "capture.cropped", SubjectType: "capture", SubjectID: child.ID, Visibility: storage.VisPrivate}); err != nil {
				return err
			}
			return rec.save(ctx, tx, child.state())
		})
		if err != nil {
			_ = s.st.Private.Delete(objKey)
			return CaptureState{}, s.internal("capture.crop", err)
		}
		s.wake()
		return child.state(), nil
	})
}
