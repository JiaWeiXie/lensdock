package cdp

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
)

// writeError sends an apiwire.Error. Messages are fixed strings chosen by the
// caller; upstream text and request content never reach the client.
func writeError(w http.ResponseWriter, status int, code, message string) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if status == http.StatusUnauthorized {
		h.Set("WWW-Authenticate", "Bearer")
	}
	if status == http.StatusMethodNotAllowed {
		h.Set("Allow", "GET, PUT")
	}
	body, err := json.Marshal(apiwire.Error{Code: code, Message: message, RequestID: h.Get("X-Request-Id")})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

// writeRouteError answers a ParseRoute failure and reports whether err was
// non-nil, in which case the response is complete.
func writeRouteError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrMethodNotAllowed):
		writeError(w, http.StatusMethodNotAllowed, apiwire.CodeInvalidRequest, "method not allowed for this route")
	case errors.Is(err, ErrQueryTooLong):
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "query too long")
	default:
		writeError(w, http.StatusNotFound, apiwire.CodeNotFound, "route not found")
	}
	return true
}

// writeDiscovery sends a rewritten discovery body with Chromium-compatible
// content types.
func writeDiscovery(w http.ResponseWriter, kind RouteKind, body []byte) {
	h := w.Header()
	if kind == RouteActivate || kind == RouteClose {
		h.Set("Content-Type", "text/plain; charset=utf-8")
	} else {
		h.Set("Content-Type", "application/json; charset=UTF-8")
	}
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
