package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/luongthanhtung03/qdt-test/internal/content"
	"github.com/luongthanhtung03/qdt-test/internal/store"
)

// Request payloads.

type seoRequest struct {
	MetaTitle       string `json:"meta_title"`
	MetaDescription string `json:"meta_description"`
	CanonicalURL    string `json:"canonical_url"`
	OGImageURL      string `json:"og_image_url"`
	NoIndex         bool   `json:"noindex"`
}

func (s seoRequest) fields(title, body string) store.VersionFields {
	return store.VersionFields{
		Title:           title,
		Body:            body,
		MetaTitle:       s.MetaTitle,
		MetaDescription: s.MetaDescription,
		CanonicalURL:    s.CanonicalURL,
		OGImageURL:      s.OGImageURL,
		NoIndex:         s.NoIndex,
	}
}

type createContentRequest struct {
	Slug  string     `json:"slug"`
	Title string     `json:"title"`
	Body  string     `json:"body"`
	SEO   seoRequest `json:"seo"`
}

type updateContentRequest struct {
	Title string     `json:"title"`
	Body  string     `json:"body"`
	SEO   seoRequest `json:"seo"`
}

// handleCreateContent creates content at version 1.
func (s *Server) handleCreateContent(w http.ResponseWriter, r *http.Request) {
	var req createContentRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	created, err := s.Content.Create(ctx, content.CreateInput{
		Slug:   req.Slug,
		Fields: req.SEO.fields(req.Title, req.Body),
		Actor:  actorFrom(r),
	})
	if err != nil {
		s.writeDomainError(w, r, err)
		return
	}

	setETag(w, created.Content.CurrentVersion)
	w.Header().Set("Location", "/api/v1/contents/"+created.Content.ID)
	writeJSON(w, http.StatusCreated, toContentDTO(created, nil))
}

// handleGetContent returns the draft view with its ETag.
func (s *Server) handleGetContent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	cwv, err := s.Content.Get(ctx, id)
	if err != nil {
		s.writeDomainError(w, r, err)
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

// handleListContents returns a page of content in the admin view.
func (s *Server) handleListContents(w http.ResponseWriter, r *http.Request) {
	limit, offset, ok := parsePagination(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	items, total, err := s.Content.List(ctx, limit, offset)
	if err != nil {
		s.writeDomainError(w, r, err)
		return
	}

	out := make([]contentDTO, 0, len(items))
	for _, cwv := range items {
		publishedVersion, err := s.publishedVersionNumber(ctx, cwv.Content)
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		out = append(out, toContentDTO(cwv, publishedVersion))
	}

	writeJSON(w, http.StatusOK, listResponse[contentDTO]{
		Items: out, Total: total, Limit: limit, Offset: offset,
	})
}

// handleUpdateContent appends a new version behind an If-Match check.
//
// If-Match is mandatory rather than optional. An unconditional PUT is exactly
// the lost update this design exists to prevent, so a caller who omits the
// header gets 428 and has to opt in to overwriting with "*".
func (s *Server) handleUpdateContent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req updateContentRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	expected, wildcard, err := parseIfMatch(r.Header.Get("If-Match"))
	switch {
	case errors.Is(err, errNoIfMatch):
		writeError(w, http.StatusPreconditionRequired, CodePreconditionRequired,
			"This endpoint requires an If-Match header carrying the current ETag. "+
				"Use If-Match: * to overwrite unconditionally.", nil)
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, CodeValidation, err.Error(),
			map[string]any{"header": "If-Match"})
		return
	}

	if wildcard {
		// "*" means "whatever the current version is". Read it, then run the
		// same compare-and-swap: this is still not a blind write, so a
		// concurrent edit between the read and the swap loses the race
		// rather than being silently clobbered.
		current, err := s.Content.Get(ctx, id)
		if err != nil {
			s.writeDomainError(w, r, err)
			return
		}
		expected = current.Content.CurrentVersion
	}

	updated, err := s.Content.Update(ctx, content.UpdateInput{
		ID:              id,
		ExpectedVersion: expected,
		Fields:          req.SEO.fields(req.Title, req.Body),
		Actor:           actorFrom(r),
	})
	if err != nil {
		s.writeDomainError(w, r, err)
		return
	}

	publishedVersion, err := s.publishedVersionNumber(ctx, updated.Content)
	if err != nil {
		writeInternal(w, r, err)
		return
	}

	setETag(w, updated.Content.CurrentVersion)
	writeJSON(w, http.StatusOK, toContentDTO(updated, publishedVersion))
}

// handleListVersions returns the version history.
func (s *Server) handleListVersions(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	limit, offset, ok := parsePagination(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	versions, err := s.Content.ListVersions(ctx, id, limit, offset)
	if err != nil {
		s.writeDomainError(w, r, err)
		return
	}

	out := make([]versionDTO, 0, len(versions))
	for _, v := range versions {
		out = append(out, toVersionDTO(v))
	}
	writeJSON(w, http.StatusOK, listResponse[versionDTO]{
		Items: out, Total: len(out), Limit: limit, Offset: offset,
	})
}

// handleGetVersion returns one historical version.
func (s *Server) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	n, err := strconv.ParseInt(chi.URLParam(r, "version"), 10, 64)
	if err != nil || n < 1 {
		writeError(w, http.StatusBadRequest, CodeValidation,
			"Version must be a positive integer.", map[string]any{"version": chi.URLParam(r, "version")})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	v, err := s.Content.GetVersion(ctx, id, n)
	if err != nil {
		s.writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toVersionDTO(v))
}

// publishedVersionNumber resolves the published version pointer (a rowid) to a
// human-meaningful version number, or nil when nothing is published.
func (s *Server) publishedVersionNumber(ctx context.Context, c store.Content) (*int64, error) {
	if c.PublishedVersionID == nil {
		return nil, nil
	}
	v, err := s.DB.GetVersionByID(ctx, *c.PublishedVersionID)
	if err != nil {
		return nil, err
	}
	return &v.Version, nil
}

// writeDomainError maps store and domain errors onto the HTTP contract. Every
// handler funnels failures through here so the mapping exists exactly once.
func (s *Server) writeDomainError(w http.ResponseWriter, r *http.Request, err error) {
	if ve, ok := content.AsValidationError(err); ok {
		writeError(w, http.StatusBadRequest, CodeValidation, ve.Message,
			map[string]any{"field": ve.Field})
		return
	}

	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, "Resource not found.", nil)

	case errors.Is(err, store.ErrSlugTaken):
		writeError(w, http.StatusConflict, CodeSlugConflict,
			"That slug is already in use.", nil)

	case errors.Is(err, store.ErrVersionConflict):
		// Include the current version so the client can rebase without a
		// second round trip.
		details := map[string]any{}
		if id := chi.URLParam(r, "id"); id != "" {
			if c, cerr := s.DB.GetContent(r.Context(), id); cerr == nil {
				details["current_version"] = c.CurrentVersion
				details["etag"] = formatETag(c.CurrentVersion)
			}
		}
		writeError(w, http.StatusPreconditionFailed, CodeVersionConflict,
			"The content was modified by someone else. Re-read it and retry.", details)

	case errors.Is(err, store.ErrScheduleExists):
		writeError(w, http.StatusConflict, CodeScheduleConflict,
			"This content already has an active schedule. Cancel it first.", nil)

	case errors.Is(err, store.ErrNotCancellable):
		writeError(w, http.StatusConflict, CodeScheduleConflict,
			"The schedule is no longer pending; it was already claimed, completed, or cancelled.", nil)

	default:
		writeInternal(w, r, err)
	}
}

// decodeJSON reads a JSON body, rejecting unknown fields so a typo in a field
// name is an error rather than a silently ignored value.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, CodeValidation,
				"Request body is too large.", nil)
			return false
		}
		writeError(w, http.StatusBadRequest, CodeValidation,
			"Request body is not valid JSON: "+err.Error(), nil)
		return false
	}

	// Exactly one JSON value per request; trailing content means a malformed
	// or doubled-up body.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, CodeValidation,
			"Request body must contain exactly one JSON object.", nil)
		return false
	}
	return true
}

const (
	defaultLimit = 20
	maxLimit     = 100
)

func parsePagination(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	limit, offset = defaultLimit, 0
	q := r.URL.Query()

	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, http.StatusBadRequest, CodeValidation,
				"limit must be an integer between 1 and "+strconv.Itoa(maxLimit)+".",
				map[string]any{"limit": raw})
			return 0, 0, false
		}
		limit = n
	}

	if raw := q.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, CodeValidation,
				"offset must be a non-negative integer.", map[string]any{"offset": raw})
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}

// actorFrom records who made a change. With a single shared admin token there
// is no real identity to record, so an optional header is used for the audit
// trail. Real authentication would replace this.
func actorFrom(r *http.Request) string {
	actor := r.Header.Get("X-Actor")
	if len(actor) > 100 {
		return actor[:100]
	}
	return actor
}
