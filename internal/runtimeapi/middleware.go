package runtimeapi

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
)

const requestIDHeader = "X-Request-Id"

// respWriter records the response status for logging and recovery while
// keeping the optional ResponseWriter interfaces (Flusher, Hijacker) that the
// CDP WebSocket proxy needs.
type respWriter struct {
	http.ResponseWriter
	status   int
	bytes    int64
	wrote    bool
	hijacked bool
}

func (w *respWriter) WriteHeader(code int) {
	if !w.wrote && (code >= 200 || code == http.StatusSwitchingProtocols) {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *respWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *respWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *respWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("runtimeapi: response writer does not support hijacking")
	}
	c, rw, err := h.Hijack()
	if err == nil {
		w.hijacked = true
	}
	return c, rw, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *respWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func newRequestID() string {
	id, err := auth.RandomHex(16)
	if err != nil {
		// crypto/rand failing means the process cannot generate any
		// credential safely; Go aborts the process on this in newer
		// runtimes, so surfacing a panic here is consistent.
		panic("runtimeapi: crypto/rand failed")
	}
	return id
}

// middleware wraps h with request IDs, panic recovery and access logging.
// Logs carry only the request ID, method, matched route pattern, status,
// size and duration: never headers, query strings, bodies or paths, so
// credentials, clipboard text and CDP payloads cannot reach them.
func middleware(log *slog.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := newRequestID()
		rw.Header().Set(requestIDHeader, id)
		w := &respWriter{ResponseWriter: rw}

		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				log.Error("handler panic",
					slog.String("requestId", id),
					slog.String("method", r.Method),
					slog.String("route", routeOf(r)),
					slog.String("panicType", fmt.Sprintf("%T", rec)),
					slog.String("stack", string(debug.Stack())),
				)
				if !w.wrote && !w.hijacked {
					writeError(w, http.StatusInternalServerError, apiwire.CodeInternal, "internal error")
				}
			}
			level := slog.LevelInfo
			if r.Pattern == "GET /healthz" || r.Pattern == "GET /readyz" {
				level = slog.LevelDebug
			}
			status := w.status
			if w.hijacked {
				status = http.StatusSwitchingProtocols
			}
			log.LogAttrs(r.Context(), level, "request",
				slog.String("requestId", id),
				slog.String("method", r.Method),
				slog.String("route", routeOf(r)),
				slog.Int("status", status),
				slog.Int64("bytes", w.bytes),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote", r.RemoteAddr),
			)
		}()
		h.ServeHTTP(w, r)
	})
}

func routeOf(r *http.Request) string {
	if r.Pattern == "" {
		return "unmatched"
	}
	return r.Pattern
}
