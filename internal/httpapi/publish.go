package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/luongthanhtung03/qdt-test/internal/clock"
	"github.com/luongthanhtung03/qdt-test/internal/content"
	"github.com/luongthanhtung03/qdt-test/internal/store"
)

type publishRequest struct {
	Version int64 `json:"version"`
}

type scheduleRequest struct {
	Version int64  `json:"version"`
	RunAt   string `json:"run_at"` // RFC3339
}

type scheduleDTO struct {
	ID        string  `json:"id"`
	ContentID string  `json:"content_id"`
	Version   int64   `json:"version"`
	RunAt     string  `json:"run_at"`
	Status    string  `json:"status"`
	Attempts  int64   `json:"attempts"`
	LastError string  `json:"last_error,omitempty"`
	LockedBy  *string `json:"locked_by,omitempty"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

// handlePublish makes a version public immediately.
func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req publishRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Version < 1 {
		writeError(w, http.StatusBadRequest, CodeValidation,
			"version must be a positive integer.", map[string]any{"field": "version"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	c, err := s.Content.Publish(ctx, id, req.Version, actorFrom(r))
	if err != nil {
		s.writeDomainError(w, r, err)
		return
	}
	s.writeContentState(w, r, ctx, c)
}

// handleUnpublish withdraws content from the public API.
func (s *Server) handleUnpublish(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	c, err := s.Content.Unpublish(ctx, id, actorFrom(r))
	if err != nil {
		s.writeDomainError(w, r, err)
		return
	}
	s.writeContentState(w, r, ctx, c)
}

// writeContentState renders the full admin view after a publish state change,
// so the caller sees the resulting state without a follow-up GET.
func (s *Server) writeContentState(w http.ResponseWriter, r *http.Request, ctx context.Context, c store.Content) {
	cwv, err := s.Content.Get(ctx, c.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	publishedVersion, err := s.publishedVersionNumber(ctx, cwv.Content)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	setETag(w, cwv.Content.CurrentVersion)
	writeJSON(w, http.StatusOK, toContentDTO(cwv, publishedVersion))
}

// handleCreateSchedule records a future publish.
func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req scheduleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Version < 1 {
		writeError(w, http.StatusBadRequest, CodeValidation,
			"version must be a positive integer.", map[string]any{"field": "version"})
		return
	}

	runAt, err := time.Parse(time.RFC3339, req.RunAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeValidation,
			"run_at must be an RFC3339 timestamp, for example 2026-01-15T12:00:00Z.",
			map[string]any{"field": "run_at"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	sc, err := s.Content.Schedule(ctx, content.ScheduleInput{
		ContentID: id,
		Version:   req.Version,
		RunAtMs:   clock.ToMillis(runAt),
	})
	if err != nil {
		s.writeDomainError(w, r, err)
		return
	}

	dto, err := s.toScheduleDTO(ctx, sc)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/schedules/"+sc.ID)
	writeJSON(w, http.StatusCreated, dto)
}

// handleListSchedules returns a content's schedules.
func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	schedules, err := s.Content.ListSchedules(ctx, id)
	if err != nil {
		s.writeDomainError(w, r, err)
		return
	}

	out := make([]scheduleDTO, 0, len(schedules))
	for _, sc := range schedules {
		dto, err := s.toScheduleDTO(ctx, sc)
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		out = append(out, dto)
	}
	writeJSON(w, http.StatusOK, listResponse[scheduleDTO]{
		Items: out, Total: len(out), Limit: len(out), Offset: 0,
	})
}

// handleCancelSchedule cancels a pending schedule.
//
// Returns 409 when a worker already claimed the job. That is the documented
// resolution of the cancel-versus-claim race: the caller learns the publish is
// going to happen rather than being told the cancel succeeded when it did not.
func (s *Server) handleCancelSchedule(w http.ResponseWriter, r *http.Request) {
	scheduleID := chi.URLParam(r, "scheduleID")

	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	if err := s.Content.CancelSchedule(ctx, scheduleID); err != nil {
		s.writeDomainError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// toScheduleDTO resolves the stored version rowid back to a version number.
func (s *Server) toScheduleDTO(ctx context.Context, sc store.Schedule) (scheduleDTO, error) {
	v, err := s.DB.GetVersionByID(ctx, sc.VersionID)
	if err != nil {
		return scheduleDTO{}, err
	}
	return scheduleDTO{
		ID:        sc.ID,
		ContentID: sc.ContentID,
		Version:   v.Version,
		RunAt:     formatMillis(sc.RunAt),
		Status:    sc.Status,
		Attempts:  sc.Attempts,
		LastError: sc.LastError,
		LockedBy:  sc.LockedBy,
		CreatedAt: formatMillis(sc.CreatedAt),
		UpdatedAt: formatMillis(sc.UpdatedAt),
	}, nil
}
