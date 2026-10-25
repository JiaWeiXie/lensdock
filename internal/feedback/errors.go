package feedback

import (
	"errors"
	"net/http"
)

// Error is a client-visible failure. Message is fixed text and never echoes
// request content or secrets.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func newError(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

// Wire error codes.
const (
	CodeUnauthenticated      = "UNAUTHENTICATED"
	CodeForbidden            = "FORBIDDEN"
	CodeNotFound             = "NOT_FOUND"
	CodeInvalidRequest       = "INVALID_REQUEST"
	CodeInvalidUTF8          = "INVALID_UTF8"
	CodeBodyTooLarge         = "BODY_TOO_LARGE"
	CodeUnsupportedMedia     = "UNSUPPORTED_MEDIA_TYPE"
	CodeInvalidImage         = "INVALID_IMAGE"
	CodeImageTooLarge        = "IMAGE_TOO_LARGE"
	CodeIdemKeyRequired      = "IDEMPOTENCY_KEY_REQUIRED"
	CodeIdemKeyReused        = "IDEMPOTENCY_KEY_REUSED"
	CodePreconditionRequired = "PRECONDITION_REQUIRED"
	CodePreconditionFailed   = "PRECONDITION_FAILED"
	CodeInvalidState         = "INVALID_STATE"
	CodeCaptureNotReady      = "CAPTURE_NOT_READY"
	CodeCaptureInUse         = "CAPTURE_IN_USE"
	CodeCaptureExpired       = "CAPTURE_EXPIRED"
	CodeStaleCapture         = "STALE_CAPTURE"
	CodeDraftExists          = "DRAFT_EXISTS"
	CodePreviewRequired      = "PREVIEW_REQUIRED"
	CodeQuotaExceeded        = "QUOTA_EXCEEDED"
	CodeAnnotationInvalid    = "ANNOTATION_INVALID"
	CodeRevisionSuperseded   = "REVISION_SUPERSEDED"
	CodeAgentNotConfigured   = "AGENT_NOT_CONFIGURED"
	CodeAgentAmbiguous       = "AGENT_AMBIGUOUS"
	CodeInvalidResultRef     = "INVALID_RESULT_REFERENCE"
	CodeCaptureFailed        = "CAPTURE_FAILED"
	CodeViewportUnavailable  = "VIEWPORT_CAPTURE_UNAVAILABLE"
	CodeMethodNotAllowed     = "METHOD_NOT_ALLOWED"
	CodeInternal             = "INTERNAL"
)

// Canonical errors. Compare with errors.As and Code, or errors.Is against
// these values for the exact instance.
var (
	ErrUnauthenticated      = newError(http.StatusUnauthorized, CodeUnauthenticated, "authentication required")
	ErrForbidden            = newError(http.StatusForbidden, CodeForbidden, "insufficient scope")
	ErrNotFound             = newError(http.StatusNotFound, CodeNotFound, "not found")
	ErrInvalidRequest       = newError(http.StatusBadRequest, CodeInvalidRequest, "invalid request")
	ErrBodyTooLarge         = newError(http.StatusRequestEntityTooLarge, CodeBodyTooLarge, "request body too large")
	ErrUnsupportedMedia     = newError(http.StatusUnsupportedMediaType, CodeUnsupportedMedia, "unsupported media type")
	ErrInvalidImage         = newError(http.StatusBadRequest, CodeInvalidImage, "the image is not a supported PNG")
	ErrImageTooLarge        = newError(http.StatusRequestEntityTooLarge, CodeImageTooLarge, "image exceeds the size, pixel or dimension limit; crop it")
	ErrIdemKeyRequired      = newError(http.StatusBadRequest, CodeIdemKeyRequired, "Idempotency-Key header required (1-256 printable ASCII characters)")
	ErrIdemKeyReused        = newError(http.StatusConflict, CodeIdemKeyReused, "Idempotency-Key was already used with a different request")
	ErrPreconditionRequired = newError(http.StatusPreconditionRequired, CodePreconditionRequired, "If-Match header required")
	ErrPreconditionFailed   = newError(http.StatusPreconditionFailed, CodePreconditionFailed, "the draft changed; reload it")
	ErrCaptureNotReady      = newError(http.StatusConflict, CodeCaptureNotReady, "capture has no image yet")
	ErrCaptureInUse         = newError(http.StatusConflict, CodeCaptureInUse, "capture is already used by a draft")
	ErrCaptureExpired       = newError(http.StatusGone, CodeCaptureExpired, "capture reservation expired")
	ErrStaleCapture         = newError(http.StatusConflict, CodeStaleCapture, "the capture predates its reservation; capture again")
	ErrDraftExists          = newError(http.StatusConflict, CodeDraftExists, "an open draft already exists for this feedback")
	ErrPreviewRequired      = newError(http.StatusConflict, CodePreviewRequired, "upload the annotated preview for the current draft first")
	ErrQuotaExceeded        = newError(http.StatusTooManyRequests, CodeQuotaExceeded, "storage quota exceeded; discard drafts or captures")
	ErrRevisionSuperseded   = newError(http.StatusConflict, CodeRevisionSuperseded, "a newer revision exists")
	ErrInvalidResultRef     = newError(http.StatusBadRequest, CodeInvalidResultRef, "resultFeedbackIds must name published feedback of this workspace")
	ErrCaptureFailed        = newError(http.StatusBadGateway, CodeCaptureFailed, "viewport capture failed")
	ErrViewportUnavailable  = newError(http.StatusNotImplemented, CodeViewportUnavailable, "viewport capture is not configured on this gateway")
	ErrInternal             = newError(http.StatusInternalServerError, CodeInternal, "internal error")
)

func invalidState(msg string) *Error { return newError(http.StatusConflict, CodeInvalidState, msg) }

func annotationInvalid(msg string) *Error {
	return newError(http.StatusBadRequest, CodeAnnotationInvalid, msg)
}

func badRequest(msg string) *Error { return newError(http.StatusBadRequest, CodeInvalidRequest, msg) }

// asError maps err to an *Error; anything unknown is ErrInternal.
func asError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return ErrInternal
}
