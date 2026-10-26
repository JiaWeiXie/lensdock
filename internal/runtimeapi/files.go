package runtimeapi

import (
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/transfer"
)

func (a *api) filesReady(w http.ResponseWriter) bool {
	if a.Files == nil {
		writeError(w, http.StatusBadGateway, apiwire.CodeUpstreamUnavailable, "file service unavailable")
		return false
	}
	return true
}

func writeTransferError(a *api, w http.ResponseWriter, err error) {
	status, code, msg := transferErrorStatus(err)
	if status >= http.StatusInternalServerError {
		// File names are user data: log the error type only.
		a.Logger.Error("file operation failed",
			slog.String("requestId", w.Header().Get(requestIDHeader)),
			slog.String("errorType", errType(err)),
		)
	}
	writeError(w, status, code, msg)
}

func (a *api) listFiles(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) {
		return
	}
	area := transfer.Area(r.URL.Query().Get("area"))
	if !transfer.ValidArea(area) {
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "area must be downloads or uploads")
		return
	}
	if !a.filesReady(w) {
		return
	}
	l, err := a.Files.List(r.Context(), area)
	if err != nil {
		writeTransferError(a, w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

// contentDisposition builds an attachment header with an ASCII fallback and
// the exact UTF-8 name (RFC 6266 / RFC 5987).
func contentDisposition(name string) string {
	ascii := make([]byte, 0, len(name))
	for i := range len(name) {
		c := name[i]
		if c < 0x20 || c > 0x7e || c == '"' || c == '\\' || c == '%' {
			c = '_'
		}
		ascii = append(ascii, c)
	}
	return `attachment; filename="` + string(ascii) + `"; filename*=UTF-8''` + url.PathEscape(name)
}

func (a *api) getFile(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) || !a.filesReady(w) {
		return
	}
	area, name := transfer.Area(r.PathValue("area")), r.PathValue("name")
	started := false
	err := a.Files.Get(r.Context(), area, name, func(size int64, modTime time.Time) (io.Writer, error) {
		h := w.Header()
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Length", strconv.FormatInt(size, 10))
		h.Set("Content-Disposition", contentDisposition(name))
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Last-Modified", modTime.UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
		started = true
		return w, nil
	})
	if err == nil {
		return
	}
	if !started {
		writeTransferError(a, w, err)
		return
	}
	// The response is already streaming with a Content-Length: abort so the
	// client sees a truncated transfer instead of a short "successful" body.
	a.Logger.Error("download interrupted",
		slog.String("requestId", w.Header().Get(requestIDHeader)),
		slog.String("errorType", errType(err)),
	)
	panic(http.ErrAbortHandler)
}

func isOctetStream(contentType string) bool {
	mt, params, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "application/octet-stream" && len(params) == 0
}

func (a *api) putFile(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) || !a.filesReady(w) {
		return
	}
	if transfer.Area(r.PathValue("area")) != transfer.AreaUploads {
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "files can only be uploaded to the uploads area")
		return
	}
	name := r.PathValue("name")
	if err := transfer.ValidName(name); err != nil {
		writeTransferError(a, w, err)
		return
	}
	if !isOctetStream(r.Header.Get("Content-Type")) {
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "Content-Type must be application/octet-stream")
		return
	}
	if r.ContentLength < 0 {
		writeError(w, http.StatusLengthRequired, apiwire.CodeInvalidRequest, "Content-Length is required")
		return
	}
	if a.FileLimits.MaxFileBytes > 0 && r.ContentLength > a.FileLimits.MaxFileBytes {
		writeError(w, http.StatusRequestEntityTooLarge, apiwire.CodeBodyTooLarge, "file exceeds the per-file size limit")
		return
	}
	overwrite := false
	switch r.URL.Query().Get("overwrite") {
	case "", "0":
	case "1":
		overwrite = true
	default:
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "overwrite must be 0 or 1")
		return
	}
	if a.UploadReadTimeout > 0 {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(a.UploadReadTimeout))
	}
	// The HTTP server frames the body by Content-Length, so it cannot
	// deliver more than size bytes; Put still verifies it got exactly size.
	e, err := a.Files.Put(r.Context(), name, r.Body, r.ContentLength, overwrite)
	if err != nil {
		writeTransferError(a, w, err)
		return
	}
	a.Logger.Info("file uploaded",
		slog.String("requestId", w.Header().Get(requestIDHeader)),
		slog.Int64("bytes", e.Size),
	)
	writeJSON(w, http.StatusCreated, e)
}

func (a *api) deleteFile(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) || !a.filesReady(w) {
		return
	}
	if err := a.Files.Delete(r.Context(), transfer.Area(r.PathValue("area")), r.PathValue("name")); err != nil {
		writeTransferError(a, w, err)
		return
	}
	a.Logger.Info("file deleted", slog.String("requestId", w.Header().Get(requestIDHeader)))
	w.WriteHeader(http.StatusNoContent)
}
