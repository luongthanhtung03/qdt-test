package httpapi

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// recoverer turns a panic into a 500 in the standard error envelope instead of
// letting it kill the connection.
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler is a deliberate abort, not a bug. Let it
			// through so the server handles it as intended.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			slog.ErrorContext(r.Context(), "panic in handler",
				"method", r.Method, "path", r.URL.Path,
				"panic", rec, "stack", string(debug.Stack()))
			writeError(w, http.StatusInternalServerError, CodeInternal,
				"An internal error occurred.", nil)
		}()
		next.ServeHTTP(w, r)
	})
}

// requestLogger emits one structured line per request.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		slog.InfoContext(r.Context(), "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", middleware.GetReqID(r.Context()),
		)
	})
}
