package lease

import (
	"errors"
	"net/http"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
)

// HTTPError maps an error returned by Manager to its HTTP status and
// apiwire.Code* wire code. ok is false for errors this package does not
// define, so callers can choose their own fallback.
//
// *LockedError maps to 409 with its lock code when that is a known readiness
// code (SANDBOX_PREFLIGHT_FAILED, PROFILE_LOCKED or RUNTIME_NOT_READY) and
// RUNTIME_NOT_READY otherwise. *EndedError maps to 409 LEASE_ENDED for every
// end reason, including EndLocked, EndTakeover and EndCapture.
func HTTPError(err error) (status int, code string, ok bool) {
	var locked *LockedError
	var ended *EndedError
	switch {
	case errors.As(err, &locked):
		code = apiwire.CodeRuntimeNotReady
		switch locked.Code {
		case apiwire.CodeSandboxPreflightFailed, apiwire.CodeProfileLocked:
			code = locked.Code
		}
		return http.StatusConflict, code, true
	case errors.As(err, &ended):
		return http.StatusConflict, apiwire.CodeLeaseEnded, true
	case errors.Is(err, ErrUnauthenticated):
		return http.StatusUnauthorized, apiwire.CodeUnauthenticated, true
	case errors.Is(err, ErrWrongMode):
		return http.StatusForbidden, apiwire.CodeWrongLeaseMode, true
	case errors.Is(err, ErrConflict), errors.Is(err, ErrInterruptNotAllowed):
		return http.StatusConflict, apiwire.CodeLeaseConflict, true
	case errors.Is(err, ErrAgentDisabled):
		return http.StatusConflict, apiwire.CodeAgentModeDisabled, true
	case errors.Is(err, ErrAgentPaused):
		return http.StatusConflict, apiwire.CodeAgentPaused, true
	case errors.Is(err, ErrPolicyPersist), errors.Is(err, ErrStalePolicy):
		return http.StatusInternalServerError, apiwire.CodeInternal, true
	case errors.Is(err, ErrTooManyWatchers):
		return http.StatusTooManyRequests, apiwire.CodeTooManyConnections, true
	case errors.Is(err, ErrMaintenance):
		return http.StatusConflict, apiwire.CodeMaintenance, true
	case errors.Is(err, ErrIdempotencyReplay):
		return http.StatusConflict, apiwire.CodeIdempotencyReplay, true
	case errors.Is(err, ErrInvalidTTL), errors.Is(err, ErrInvalidArgument):
		return http.StatusBadRequest, apiwire.CodeInvalidRequest, true
	case errors.Is(err, ErrEpochMismatch):
		return http.StatusPreconditionFailed, apiwire.CodeEpochMismatch, true
	case errors.Is(err, ErrGenerationMismatch):
		return http.StatusPreconditionFailed, apiwire.CodeGenerationMismatch, true
	case errors.Is(err, ErrLeaseMismatch):
		return http.StatusPreconditionFailed, apiwire.CodeLeaseMismatch, true
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, apiwire.CodeNotFound, true
	case errors.Is(err, ErrShutdown):
		return http.StatusConflict, apiwire.CodeRuntimeNotReady, true
	}
	return 0, "", false
}
