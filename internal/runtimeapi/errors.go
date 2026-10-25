package runtimeapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/capture"
	"github.com/JiaWeiXie/lensdock/internal/clipboard"
	"github.com/JiaWeiXie/lensdock/internal/lease"
	"github.com/JiaWeiXie/lensdock/internal/transfer"
	"github.com/JiaWeiXie/lensdock/internal/vnc"
)

// writeJSON marshals v before writing so a failure cannot produce a partial
// body. Responses may carry capability tokens, so they are never cacheable.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"code":"`+apiwire.CodeInternal+`","message":"internal error","requestId":""}`)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	writeJSON(w, status, apiwire.Error{Code: code, Message: message, RequestID: w.Header().Get(requestIDHeader)})
}

// methodNotAllowed answers a known path requested with an unsupported method.
func methodNotAllowed(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		writeError(w, http.StatusMethodNotAllowed, apiwire.CodeMethodNotAllowed, "method not allowed")
	}
}

// leaseErrorStatus maps a lease.Manager error to its HTTP status, wire code
// and a fixed message. The message never echoes request content. Status and
// code come from lease.HTTPError; errors it does not know are 500 INTERNAL.
func leaseErrorStatus(err error) (status int, code, message string) {
	status, code, ok := lease.HTTPError(err)
	if !ok {
		return http.StatusInternalServerError, apiwire.CodeInternal, "internal error"
	}
	switch {
	case errors.Is(err, lease.ErrShutdown):
		message = "runtime is shutting down"
	case errors.Is(err, lease.ErrInvalidTTL):
		message = "ttlSeconds is out of range"
	case errors.Is(err, lease.ErrInterruptNotAllowed):
		message = "the operation would interrupt the current lease; set allowInterrupt to accept"
	case errors.Is(err, lease.ErrPolicyPersist), errors.Is(err, lease.ErrStalePolicy):
		message = "the agent policy could not be saved; a pause or NOVNC change that was requested stays in force but may not survive a restart"
	case errors.Is(err, lease.ErrTooManyWatchers):
		message = "too many agent policy watchers"
	default:
		message = leaseCodeMessages[code]
	}
	return status, code, message
}

// leaseCodeMessages holds the fixed message of each code lease.HTTPError can
// return.
var leaseCodeMessages = map[string]string{
	apiwire.CodeRuntimeNotReady:        "workspace is not ready",
	apiwire.CodeSandboxPreflightFailed: "workspace is not ready",
	apiwire.CodeProfileLocked:          "the browser profile is locked by another runtime",
	apiwire.CodeLeaseEnded:             "lease ended",
	apiwire.CodeUnauthenticated:        "invalid credentials",
	apiwire.CodeWrongLeaseMode:         "lease mode does not permit this operation",
	apiwire.CodeLeaseConflict:          "workspace already has an active lease or a control change in progress",
	apiwire.CodeMaintenance:            "workspace is in maintenance",
	apiwire.CodeLeaseMismatch:          "the expected lease is not the current lease",
	apiwire.CodeIdempotencyReplay:      "idempotency key was already used by the active lease",
	apiwire.CodeInvalidRequest:         "invalid request",
	apiwire.CodeEpochMismatch:          "lease epoch does not match",
	apiwire.CodeGenerationMismatch:     "runtime generation does not match",
	apiwire.CodeNotFound:               "not found",
	apiwire.CodeAgentModeDisabled:      "agent mode is disabled; a human must enable AGENT mode in settings",
	apiwire.CodeAgentPaused:            "the agent is paused; a human must resume it",
}

// bodyErrorStatus maps apiwire.Decode failures.
func bodyErrorStatus(err error) (status int, code, message string) {
	switch {
	case errors.Is(err, apiwire.ErrBodyTooLarge):
		return http.StatusRequestEntityTooLarge, apiwire.CodeBodyTooLarge, "request body too large"
	case errors.Is(err, apiwire.ErrInvalidUTF8):
		return http.StatusBadRequest, apiwire.CodeInvalidUTF8, "request body is not valid UTF-8"
	default:
		return http.StatusBadRequest, apiwire.CodeInvalidRequest, "invalid request body"
	}
}

// clipboardErrorStatus maps clipboard.Bridge.Set failures.
func clipboardErrorStatus(err error) (status int, code, message string) {
	switch {
	case errors.Is(err, clipboard.ErrInvalidUTF8):
		return http.StatusBadRequest, apiwire.CodeInvalidUTF8, "clipboard text is not valid UTF-8"
	case errors.Is(err, clipboard.ErrNUL):
		return http.StatusBadRequest, apiwire.CodeInvalidRequest, "clipboard text contains NUL"
	case errors.Is(err, clipboard.ErrTooLarge):
		return http.StatusRequestEntityTooLarge, apiwire.CodeBodyTooLarge, "clipboard text too large"
	}
	return http.StatusBadGateway, apiwire.CodeUpstreamUnavailable, "clipboard backend unavailable"
}

// transferErrorStatus maps transfer.Store failures. Unknown errors are 502:
// the helper process, not the request, failed.
func transferErrorStatus(err error) (status int, code, message string) {
	switch {
	case errors.Is(err, transfer.ErrInvalidName):
		return http.StatusBadRequest, apiwire.CodeInvalidName, "file name must be a single path segment without control characters or a leading dot"
	case errors.Is(err, transfer.ErrInvalidArea):
		return http.StatusBadRequest, apiwire.CodeInvalidRequest, "area must be downloads or uploads"
	case errors.Is(err, transfer.ErrNotFound):
		return http.StatusNotFound, apiwire.CodeNotFound, "file not found"
	case errors.Is(err, transfer.ErrExists):
		return http.StatusConflict, apiwire.CodeFileExists, "a file with this name already exists"
	case errors.Is(err, transfer.ErrTooLarge):
		return http.StatusRequestEntityTooLarge, apiwire.CodeBodyTooLarge, "file exceeds the per-file size limit"
	case errors.Is(err, transfer.ErrQuota), errors.Is(err, transfer.ErrTooMany):
		return http.StatusRequestEntityTooLarge, apiwire.CodeQuotaExceeded, "the transfer quota would be exceeded"
	case errors.Is(err, transfer.ErrSizeMismatch):
		return http.StatusBadRequest, apiwire.CodeInvalidRequest, "request body length differs from Content-Length"
	}
	return http.StatusBadGateway, apiwire.CodeUpstreamUnavailable, "file service unavailable"
}

// captureErrorStatus maps capture.Capturer failures.
func captureErrorStatus(err error) (status int, code, message string) {
	switch {
	case errors.Is(err, capture.ErrInvalidTarget):
		return http.StatusBadRequest, apiwire.CodeInvalidRequest, "targetId is not a valid target identifier"
	case errors.Is(err, capture.ErrTargetNotFound):
		return http.StatusNotFound, apiwire.CodeTargetNotFound, "target not found"
	case errors.Is(err, capture.ErrImageLimit):
		return http.StatusRequestEntityTooLarge, apiwire.CodeBodyTooLarge, "screenshot exceeds the size limit"
	}
	return http.StatusBadGateway, apiwire.CodeCaptureFailed, "viewport capture failed"
}

// resizeErrorStatus maps vnc.Resizer failures.
func resizeErrorStatus(err error) (status int, code, message string) {
	switch {
	case errors.Is(err, vnc.ErrUnsupportedSize):
		return http.StatusBadRequest, apiwire.CodeResizeUnsupported, "display size is not supported by this display"
	case errors.Is(err, vnc.ErrResizeFailed):
		return http.StatusBadGateway, apiwire.CodeUpstreamUnavailable, "the display did not change size"
	}
	return http.StatusBadGateway, apiwire.CodeUpstreamUnavailable, "display backend unavailable"
}
