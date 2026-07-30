package httpapi

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/luongthanhtung03/qdt-test/internal/clock"
	"github.com/luongthanhtung03/qdt-test/internal/config"
	"github.com/luongthanhtung03/qdt-test/internal/store"
)

// Server holds the dependencies shared by every handler.
type Server struct {
	DB    *store.DB
	Cfg   config.Config
	Clock clock.Clock
}

// New builds a Server.
func New(db *store.DB, cfg config.Config, clk clock.Clock) *Server {
	return &Server{DB: db, Cfg: cfg, Clock: clk}
}

// Routes builds the HTTP handler.
//
// Admin and public routes are mounted as separate subtrees so the bearer-token
// middleware is scoped to /api/v1 by construction. Nothing under /public can
// accidentally inherit it, and -- more importantly -- nothing under /api/v1 can
// accidentally lose it.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(recoverer)
	r.Use(requestLogger)

	r.Get("/healthz", s.handleHealth)

	return r
}

// handleHealth reports process and database liveness.
//
// It pings the write pool specifically: the read pool can look healthy while
// the disk holding the WAL is full or read-only, and a service that cannot
// write is not healthy for this workload.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	if err := s.DB.Write.PingContext(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "unhealthy",
			"error":  "database unreachable",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   s.Clock.Now().UTC().Format(rfc3339Milli),
	})
}
