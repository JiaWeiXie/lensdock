package lease

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
)

func TestHTTPErrorMapsEveryLeaseError(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"locked not ready", &LockedError{Code: apiwire.CodeRuntimeNotReady}, http.StatusConflict, apiwire.CodeRuntimeNotReady},
		{"locked preflight", &LockedError{Code: apiwire.CodeSandboxPreflightFailed}, http.StatusConflict, apiwire.CodeSandboxPreflightFailed},
		{"locked unknown code", &LockedError{Code: "SOMETHING_ELSE"}, http.StatusConflict, apiwire.CodeRuntimeNotReady},
		{"ended expired", &EndedError{Reason: EndExpired}, http.StatusConflict, apiwire.CodeLeaseEnded},
		{"ended locked", &EndedError{Reason: EndLocked}, http.StatusConflict, apiwire.CodeLeaseEnded},
		{"ended generation", &EndedError{Reason: EndGeneration}, http.StatusConflict, apiwire.CodeLeaseEnded},
		{"unauthenticated", ErrUnauthenticated, http.StatusUnauthorized, apiwire.CodeUnauthenticated},
		{"wrong mode", ErrWrongMode, http.StatusForbidden, apiwire.CodeWrongLeaseMode},
		{"conflict", ErrConflict, http.StatusConflict, apiwire.CodeLeaseConflict},
		{"idempotency replay", ErrIdempotencyReplay, http.StatusConflict, apiwire.CodeIdempotencyReplay},
		{"invalid ttl", ErrInvalidTTL, http.StatusBadRequest, apiwire.CodeInvalidRequest},
		{"invalid argument", ErrInvalidArgument, http.StatusBadRequest, apiwire.CodeInvalidRequest},
		{"epoch mismatch", ErrEpochMismatch, http.StatusPreconditionFailed, apiwire.CodeEpochMismatch},
		{"generation mismatch", ErrGenerationMismatch, http.StatusPreconditionFailed, apiwire.CodeGenerationMismatch},
		{"not found", ErrNotFound, http.StatusNotFound, apiwire.CodeNotFound},
		{"shutdown", ErrShutdown, http.StatusConflict, apiwire.CodeRuntimeNotReady},
		{"wrapped sentinel", fmt.Errorf("%w: mode", ErrInvalidArgument), http.StatusBadRequest, apiwire.CodeInvalidRequest},
		{"wrapped locked", fmt.Errorf("renew: %w", &LockedError{Code: apiwire.CodeSandboxPreflightFailed}), http.StatusConflict, apiwire.CodeSandboxPreflightFailed},
		{"wrapped ended", fmt.Errorf("authorize: %w", &EndedError{Reason: EndReleased}), http.StatusConflict, apiwire.CodeLeaseEnded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, code, ok := HTTPError(tt.err)
			if !ok || status != tt.status || code != tt.code {
				t.Fatalf("HTTPError(%v) = %d %q ok=%v, want %d %q", tt.err, status, code, ok, tt.status, tt.code)
			}
		})
	}
}

func TestHTTPErrorRejectsNonLeaseErrors(t *testing.T) {
	for name, err := range map[string]error{
		"nil":   nil,
		"other": errors.New("boom"),
	} {
		if status, code, ok := HTTPError(err); ok || status != 0 || code != "" {
			t.Errorf("%s: HTTPError = %d %q ok=%v, want zero values and ok=false", name, status, code, ok)
		}
	}
}

// Every error a Manager method returns on its own must be mapped, so the
// transports never fall back to 500 for a known lease condition.
func TestHTTPErrorCoversManagerErrors(t *testing.T) {
	m, _ := newReadyManager(t)
	l, tok := mustAcquire(t, m, ModeAgent, 0, "k")
	var errs []error
	collect := func(vals ...any) {
		err, _ := vals[len(vals)-1].(error)
		errs = append(errs, err)
	}

	collect(m.Acquire("operator", ModeAgent, 0, "k"))
	collect(m.Acquire("operator", ModeAgent, 0, "other"))
	collect(m.Acquire("operator", "ROOT", 0, ""))
	collect(m.Acquire("operator", ModeAgent, 1, ""))
	collect(m.Renew("ldl_unknown", l.ID, l.Epoch, l.Generation))
	collect(m.Renew(tok, "lse_other", l.Epoch, l.Generation))
	collect(m.Renew(tok, l.ID, l.Epoch+1, l.Generation))
	collect(m.Renew(tok, l.ID, l.Epoch, "stale"))
	collect(m.Authorize(tok, ModeHuman))
	errs = append(errs, m.Release("lse_other"))
	m.SetLocked(true, "")
	collect(m.Renew(tok, l.ID, l.Epoch, l.Generation))
	collect(m.Authorize(tok, ModeAgent))
	m.Shutdown()
	collect(m.Renew(tok, l.ID, l.Epoch, l.Generation))

	for i, err := range errs {
		if err == nil {
			t.Errorf("case %d: expected an error", i)
			continue
		}
		if _, _, ok := HTTPError(err); !ok {
			t.Errorf("case %d: %v is not mapped by HTTPError", i, err)
		}
	}
}
