package httpapi

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/luongthanhtung03/qdt-test/internal/clock"
	"github.com/luongthanhtung03/qdt-test/internal/config"
	"github.com/luongthanhtung03/qdt-test/internal/content"
	"github.com/luongthanhtung03/qdt-test/internal/store"
)

// Server holds the dependencies shared by every handler.
type Server struct {
	DB      *store.DB
	Cfg     config.Config
	Clock   clock.Clock
	Content *content.Service
}

// New builds a Server.
func New(db *store.DB, cfg config.Config, clk clock.Clock) *Server {
	return &Server{
		DB:      db,
		Cfg:     cfg,
		Clock:   clk,
		Content: content.New(db, clk),
	}
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

	// Admin subtree. The bearer-token middleware is attached inside this
	// Route group, so it applies to everything below /api/v1 and to nothing
	// outside it.
	r.Route("/api/v1", func(admin chi.Router) {
		admin.Use(requireBearerToken(s.Cfg.AdminAPIToken))

		admin.Route("/contents", func(c chi.Router) {
			c.Post("/", s.handleCreateContent)
			c.Get("/", s.handleListContents)

			c.Route("/{id}", func(one chi.Router) {
				one.Get("/", s.handleGetContent)
				one.Put("/", s.handleUpdateContent)
				one.Get("/versions", s.handleListVersions)
				one.Get("/versions/{version}", s.handleGetVersion)

				one.Post("/publish", s.handlePublish)
				one.Post("/unpublish", s.handleUnpublish)
				one.Post("/schedules", s.handleCreateSchedule)
				one.Get("/schedules", s.handleListSchedules)
			})
		})

		admin.Delete("/schedules/{scheduleID}", s.handleCancelSchedule)
	})

	// Public subtree. Mounted as a sibling of /api/v1, never inside it, so no
	// admin middleware can reach it and no public handler can see a draft.
	r.Route("/public/v1", func(pub chi.Router) {
		pub.Get("/contents", s.handlePublicList)
		pub.Get("/contents/{slug}", s.handlePublicGet)
	})

	// Crawler-facing endpoints live at the root because that is where crawlers
	// look for them; a sitemap under a path prefix is largely ignored.
	r.Get("/sitemap.xml", s.handleSitemap)
	r.Get("/robots.txt", s.handleRobots)

	// The canonical public page for a document, and what the sitemap points at.
	// Registered last and at the root: chi matches static segments before
	// wildcards, so every route above still wins over this one.
	r.Get("/{slug}", s.handleHTMLPage)

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
