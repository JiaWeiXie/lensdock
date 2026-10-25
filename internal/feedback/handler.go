package feedback

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
)

const requestIDHeader = "X-Request-Id"

// MountPatterns are the ServeMux patterns the Gateway mounts on the handler
// returned by NewHandler (the handler routes the full /api/v1 paths itself).
var MountPatterns = []string{
	"/api/v1/feedback",
	"/api/v1/feedback/",
	"/api/v1/captures/",
	"POST /api/v1/workspaces/{workspaceId}/captures",
}

type handler struct{ s *Service }

// NewHandler returns the HTTP API of captures and feedback. The Gateway must
// authenticate the request and put the Principal into the request context
// with WithPrincipal; without one every route answers 401. Errors are the
// flat apiwire.Error envelope; responses are never cacheable.
func NewHandler(s *Service) http.Handler {
	h := &handler{s: s}
	mux := http.NewServeMux()
	route := func(pattern string, fn func(ctx context.Context, w http.ResponseWriter, r *http.Request) error) {
		mux.HandleFunc(pattern, h.wrap(fn))
	}
	route("POST /api/v1/workspaces/{workspaceId}/captures", h.createCapture)
	route("GET /api/v1/captures/{captureId}", h.getCapture)
	route("GET /api/v1/captures/{captureId}/image", h.getCaptureImage)
	route("PUT /api/v1/captures/{captureId}/image", h.putCaptureImage)
	route("POST /api/v1/captures/{captureId}/crop", h.cropCapture)
	route("POST /api/v1/feedback", h.createDraft)
	route("GET /api/v1/feedback", h.listFeedback)
	route("GET /api/v1/feedback/drafts", h.listDrafts)
	route("GET /api/v1/feedback/{feedbackId}", h.getFeedback)
	route("PATCH /api/v1/feedback/{feedbackId}", h.patchDraft)
	route("GET /api/v1/feedback/{feedbackId}/draft", h.getDraft)
	route("DELETE /api/v1/feedback/{feedbackId}/draft", h.deleteDraft)
	route("PUT /api/v1/feedback/{feedbackId}/draft/preview", h.putPreview)
	route("GET /api/v1/feedback/{feedbackId}/draft/preview/{name}", h.getSharedPreview)
	route("GET /api/v1/feedback/{feedbackId}/revisions", h.listRevisions)
	route("POST /api/v1/feedback/{feedbackId}/revisions", h.newRevision)
	route("POST /api/v1/feedback/{feedbackId}/publish", h.publish)
	route("POST /api/v1/feedback/{feedbackId}/dispatch", h.dispatch)
	route("POST /api/v1/feedback/{feedbackId}/ack", h.ack)
	route("POST /api/v1/feedback/{feedbackId}/resolve", h.resolve)
	route("POST /api/v1/feedback/{feedbackId}/verify", h.verify)
	route("POST /api/v1/feedback/{feedbackId}/cancel", h.cancel)
	route("GET /api/v1/feedback/{feedbackId}/delivery", h.getDelivery)
	route("GET /api/v1/feedback/{feedbackId}/manifest", h.getManifest)
	route("GET /api/v1/feedback/{feedbackId}/bundle", h.getBundle)
	route("GET /api/v1/feedback/{feedbackId}/artifacts/{name}", h.getArtifact)

	for pattern, allow := range map[string]string{
		"/api/v1/workspaces/{workspaceId}/captures":          "POST",
		"/api/v1/captures/{captureId}":                       "GET",
		"/api/v1/captures/{captureId}/image":                 "GET, PUT",
		"/api/v1/captures/{captureId}/crop":                  "POST",
		"/api/v1/feedback":                                   "GET, POST",
		"/api/v1/feedback/{feedbackId}":                      "GET, PATCH",
		"/api/v1/feedback/{feedbackId}/draft":                "GET, DELETE",
		"/api/v1/feedback/{feedbackId}/draft/preview":        "PUT",
		"/api/v1/feedback/{feedbackId}/draft/preview/{name}": "GET",
		"/api/v1/feedback/{feedbackId}/revisions":            "GET, POST",
		"/api/v1/feedback/{feedbackId}/publish":              "POST",
		"/api/v1/feedback/{feedbackId}/dispatch":             "POST",
		"/api/v1/feedback/{feedbackId}/ack":                  "POST",
		"/api/v1/feedback/{feedbackId}/resolve":              "POST",
		"/api/v1/feedback/{feedbackId}/verify":               "POST",
		"/api/v1/feedback/{feedbackId}/cancel":               "POST",
		"/api/v1/feedback/{feedbackId}/delivery":             "GET",
		"/api/v1/feedback/{feedbackId}/manifest":             "GET",
		"/api/v1/feedback/{feedbackId}/bundle":               "GET",
		"/api/v1/feedback/{feedbackId}/artifacts/{name}":     "GET",
	} {
		allow := allow
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			setSecurityHeaders(w)
			w.Header().Set("Allow", allow)
			writeErr(w, newError(http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed"))
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		writeErr(w, ErrNotFound)
	})
	return mux
}

func setSecurityHeaders(w http.ResponseWriter) {
	hd := w.Header()
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; sandbox")
	hd.Set("Referrer-Policy", "no-referrer")
	if hd.Get(requestIDHeader) == "" {
		hd.Set(requestIDHeader, newID(""))
	}
}

func (h *handler) wrap(fn func(ctx context.Context, w http.ResponseWriter, r *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		ctx := WithRequestID(r.Context(), w.Header().Get(requestIDHeader))
		if _, err := requirePrincipal(ctx); err != nil {
			writeErr(w, asError(err))
			return
		}
		if err := fn(ctx, w, r); err != nil {
			e := asError(err)
			if e == ErrInternal && !errors.Is(err, ErrInternal) {
				// Unclassified failures (context cancellation etc.).
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					e = newError(http.StatusServiceUnavailable, "UNAVAILABLE", "request cancelled")
				}
			}
			writeErr(w, e)
		}
	}
}

func writeErr(w http.ResponseWriter, e *Error) {
	if e.Status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	writeJSON(w, e.Status, apiwire.Error{Code: e.Code, Message: e.Message, RequestID: w.Header().Get(requestIDHeader)})
}

// writeJSON marshals first so a failure cannot produce a partial body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := marshalCompact(v)
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte(`{"code":"INTERNAL","message":"internal error","requestId":""}` + "\n")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func quoteETag(etag string) string { return `"` + etag + `"` }

func readBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, ErrBodyTooLarge
		}
		return nil, badRequest("could not read the request body")
	}
	return data, nil
}

func mediaTypeIs(r *http.Request, want string) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == want
}

// decodeJSON strictly decodes a JSON body: unknown fields, trailing data and
// invalid UTF-8 are rejected.
func (h *handler) decodeJSON(w http.ResponseWriter, r *http.Request, v any, optional bool) error {
	data, err := readBody(w, r, h.s.lim.MaxJSONBody)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		if optional {
			return nil
		}
		return badRequest("a JSON body is required")
	}
	if !mediaTypeIs(r, "application/json") {
		return ErrUnsupportedMedia
	}
	if err := apiwire.Decode(bytes.NewReader(data), h.s.lim.MaxJSONBody, v); err != nil {
		switch {
		case errors.Is(err, apiwire.ErrBodyTooLarge):
			return ErrBodyTooLarge
		case errors.Is(err, apiwire.ErrInvalidUTF8):
			return newError(http.StatusBadRequest, CodeInvalidUTF8, "request body is not valid UTF-8")
		}
		return ErrInvalidRequest
	}
	return nil
}

func (h *handler) readPNG(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if !mediaTypeIs(r, "image/png") {
		return nil, ErrUnsupportedMedia
	}
	return readBody(w, r, h.s.lim.MaxImageBytes)
}

func revisionParam(r *http.Request) (int, error) {
	v := r.URL.Query().Get("revision")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, badRequest("revision must be a positive integer")
	}
	return n, nil
}

func idemKey(r *http.Request) string { return r.Header.Get("Idempotency-Key") }

func writeBytes(w http.ResponseWriter, contentType string, data []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// ---- captures --------------------------------------------------------------

func (h *handler) createCapture(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var in CreateCaptureInput
	if err := h.decodeJSON(w, r, &in, false); err != nil {
		return err
	}
	st, err := h.s.CreateCapture(ctx, r.PathValue("workspaceId"), in, idemKey(r))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, st)
	return nil
}

func (h *handler) getCapture(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	st, err := h.s.GetCapture(ctx, r.PathValue("captureId"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, st)
	return nil
}

func (h *handler) getCaptureImage(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	data, err := h.s.ReadCaptureImage(ctx, r.PathValue("captureId"))
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeBytes(w, "image/png", data)
	return nil
}

func (h *handler) putCaptureImage(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var at *time.Time
	if v := r.Header.Get("X-LensDock-Captured-At"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return badRequest("X-LensDock-Captured-At must be an RFC 3339 time")
		}
		at = &t
	}
	data, err := h.readPNG(w, r)
	if err != nil {
		return err
	}
	st, err := h.s.UploadCaptureImage(ctx, r.PathValue("captureId"), data, at, idemKey(r))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, st)
	return nil
}

func (h *handler) cropCapture(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var in CropInput
	if err := h.decodeJSON(w, r, &in, false); err != nil {
		return err
	}
	st, err := h.s.CropCapture(ctx, r.PathValue("captureId"), in, idemKey(r))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, st)
	return nil
}

// ---- drafts ----------------------------------------------------------------

func (h *handler) createDraft(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var in CreateDraftInput
	if err := h.decodeJSON(w, r, &in, false); err != nil {
		return err
	}
	d, err := h.s.CreateDraft(ctx, in, idemKey(r))
	if err != nil {
		return err
	}
	w.Header().Set("ETag", quoteETag(d.ETag))
	writeJSON(w, http.StatusCreated, d)
	return nil
}

func (h *handler) listDrafts(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	l, err := h.s.ListDrafts(ctx, r.URL.Query().Get("workspaceId"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, l)
	return nil
}

func (h *handler) getDraft(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	d, err := h.s.GetDraft(ctx, r.PathValue("feedbackId"))
	if err != nil {
		return err
	}
	w.Header().Set("ETag", quoteETag(d.ETag))
	writeJSON(w, http.StatusOK, d)
	return nil
}

func (h *handler) patchDraft(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var in PatchDraftInput
	if err := h.decodeJSON(w, r, &in, false); err != nil {
		return err
	}
	d, err := h.s.PatchDraft(ctx, r.PathValue("feedbackId"), r.Header.Get("If-Match"), in, idemKey(r))
	if err != nil {
		return err
	}
	w.Header().Set("ETag", quoteETag(d.ETag))
	writeJSON(w, http.StatusOK, d)
	return nil
}

func (h *handler) deleteDraft(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	if err := h.s.DiscardDraft(ctx, r.PathValue("feedbackId")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *handler) putPreview(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	data, err := h.readPNG(w, r)
	if err != nil {
		return err
	}
	d, err := h.s.UploadPreview(ctx, r.PathValue("feedbackId"), r.Header.Get("If-Match"), data, idemKey(r))
	if err != nil {
		return err
	}
	w.Header().Set("ETag", quoteETag(d.ETag))
	writeJSON(w, http.StatusOK, d)
	return nil
}

func (h *handler) getSharedPreview(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	data, err := h.s.SharedPreview(ctx, r.PathValue("feedbackId"), r.PathValue("name"))
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeBytes(w, "image/png", data)
	return nil
}

func (h *handler) newRevision(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var in NewRevisionInput
	if err := h.decodeJSON(w, r, &in, true); err != nil {
		return err
	}
	d, err := h.s.NewRevision(ctx, r.PathValue("feedbackId"), in, idemKey(r))
	if err != nil {
		return err
	}
	w.Header().Set("ETag", quoteETag(d.ETag))
	writeJSON(w, http.StatusCreated, d)
	return nil
}

func (h *handler) publish(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var in struct {
		HumanApproved *bool `json:"humanApproved"`
		Dispatch      bool  `json:"dispatch"`
	}
	if err := h.decodeJSON(w, r, &in, false); err != nil {
		return err
	}
	pf, err := h.s.Publish(ctx, r.PathValue("feedbackId"), r.Header.Get("If-Match"), PublishInput{HumanApproved: in.HumanApproved != nil && *in.HumanApproved, Dispatch: in.Dispatch}, idemKey(r))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, pf)
	return nil
}

// ---- published reads -------------------------------------------------------

func (h *handler) listFeedback(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return badRequest("limit must be a positive integer")
		}
		limit = n
	}
	l, err := h.s.ListPublished(ctx, ListQuery{WorkspaceID: q.Get("workspaceId"), State: q.Get("state"), Cursor: q.Get("cursor"), Limit: limit})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, l)
	return nil
}

func (h *handler) listRevisions(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	l, err := h.s.ListRevisions(ctx, r.PathValue("feedbackId"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, l)
	return nil
}

func (h *handler) getFeedback(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	rev, err := revisionParam(r)
	if err != nil {
		return err
	}
	pf, err := h.s.GetPublished(ctx, r.PathValue("feedbackId"), rev)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, pf)
	return nil
}

func (h *handler) getDelivery(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	rev, err := revisionParam(r)
	if err != nil {
		return err
	}
	d, err := h.s.GetDelivery(ctx, r.PathValue("feedbackId"), rev)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, d)
	return nil
}

func serveArtifact(w http.ResponseWriter, a *ArtifactData) {
	hd := w.Header()
	hd.Set("Cache-Control", "private, no-store")
	hd.Set("Content-Disposition", `attachment; filename="`+a.Name+`"`)
	hd.Set("X-LensDock-SHA256", a.SHA256)
	hd.Set("ETag", quoteETag(a.SHA256))
	writeBytes(w, a.MimeType, a.Data)
}

func (h *handler) getArtifact(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	rev, err := revisionParam(r)
	if err != nil {
		return err
	}
	a, err := h.s.ReadArtifact(ctx, r.PathValue("feedbackId"), rev, r.PathValue("name"))
	if err != nil {
		return err
	}
	serveArtifact(w, a)
	return nil
}

func (h *handler) getManifest(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	rev, err := revisionParam(r)
	if err != nil {
		return err
	}
	a, err := h.s.ReadManifest(ctx, r.PathValue("feedbackId"), rev)
	if err != nil {
		return err
	}
	serveArtifact(w, a)
	return nil
}

func (h *handler) getBundle(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	rev, err := revisionParam(r)
	if err != nil {
		return err
	}
	b, err := h.s.ReadBundle(ctx, r.PathValue("feedbackId"), rev)
	if err != nil {
		return err
	}
	hd := w.Header()
	hd.Set("Cache-Control", "private, no-store")
	hd.Set("Content-Disposition", `attachment; filename="`+b.Name+`"`)
	hd.Set("X-LensDock-SHA256", sha256Hex(b.Zip))
	writeBytes(w, "application/zip", b.Zip)
	return nil
}

// ---- delivery transitions --------------------------------------------------

func (h *handler) dispatch(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var in DispatchInput
	if err := h.decodeJSON(w, r, &in, false); err != nil {
		return err
	}
	d, err := h.s.Dispatch(ctx, r.PathValue("feedbackId"), in, idemKey(r))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, d)
	return nil
}

func (h *handler) ack(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var in AckInput
	if err := h.decodeJSON(w, r, &in, false); err != nil {
		return err
	}
	d, err := h.s.Ack(ctx, r.PathValue("feedbackId"), in, idemKey(r))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, d)
	return nil
}

func (h *handler) resolve(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var in ResolveInput
	if err := h.decodeJSON(w, r, &in, false); err != nil {
		return err
	}
	d, err := h.s.Resolve(ctx, r.PathValue("feedbackId"), in, idemKey(r))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, d)
	return nil
}

func (h *handler) verify(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var in VerifyInput
	if err := h.decodeJSON(w, r, &in, false); err != nil {
		return err
	}
	d, err := h.s.Verify(ctx, r.PathValue("feedbackId"), in, idemKey(r))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, d)
	return nil
}

func (h *handler) cancel(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var in CancelInput
	if err := h.decodeJSON(w, r, &in, false); err != nil {
		return err
	}
	d, err := h.s.Cancel(ctx, r.PathValue("feedbackId"), in, idemKey(r))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, d)
	return nil
}
