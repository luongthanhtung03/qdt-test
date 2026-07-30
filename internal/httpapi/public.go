package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/luongthanhtung03/qdt-test/internal/clock"
	"github.com/luongthanhtung03/qdt-test/internal/store"
)

// The public handlers never receive a content id, a version number, or a draft.
// They call store methods that join through contents.published_version_id, so
// unpublished content is not filtered out -- it is never selected in the first
// place. See internal/store/public.go.

func toPublicDTO(p store.PublicContent) publicContentDTO {
	return publicContentDTO{
		Slug:        p.Slug,
		Title:       p.Title,
		Body:        p.Body,
		SEO:         toSEO(p.Fields),
		PublishedAt: formatMillis(p.PublishedAt),
		UpdatedAt:   formatMillis(p.UpdatedAt),
	}
}

// handlePublicGet serves one published document by slug.
func (s *Server) handlePublicGet(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")

	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	p, err := s.Content.GetPublicBySlug(ctx, slug)
	if err != nil {
		// An unknown slug and an unpublished document are the same 404. A
		// distinct response for "exists but not published" would let anyone
		// enumerate unreleased content by probing slugs.
		s.writePublicError(w, r, err)
		return
	}

	// The published pointer only moves on publish or unpublish, so
	// contents.updated_at is an exact validator for this representation.
	if s.serveConditional(w, r, p.UpdatedAt) {
		return
	}
	setPublicCacheHeaders(w, p.UpdatedAt)
	writeJSON(w, http.StatusOK, toPublicDTO(p))
}

// handlePublicList serves published documents.
func (s *Server) handlePublicList(w http.ResponseWriter, r *http.Request) {
	limit, offset, ok := parsePagination(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	items, total, err := s.Content.ListPublic(ctx, limit, offset)
	if err != nil {
		s.writePublicError(w, r, err)
		return
	}

	out := make([]publicContentDTO, 0, len(items))
	for _, p := range items {
		out = append(out, toPublicDTO(p))
	}

	latest, err := s.DB.LatestPublishedAt(ctx)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	if s.serveConditional(w, r, latest) {
		return
	}
	setPublicCacheHeaders(w, latest)
	writeJSON(w, http.StatusOK, listResponse[publicContentDTO]{
		Items: out, Total: total, Limit: limit, Offset: offset,
	})
}

// writePublicError keeps the public surface to 404 and 500. Public callers
// have no use for the admin error vocabulary.
func (s *Server) writePublicError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, "Not found.", nil)
		return
	}
	writeInternal(w, r, err)
}

// serveConditional answers 304 when the client's cached copy is still current,
// and reports whether it handled the response.
//
// This is the part of SEO a backend actually owns: a crawler that gets 304 for
// unchanged pages spends its crawl budget on pages that did change.
func (s *Server) serveConditional(w http.ResponseWriter, r *http.Request, lastModifiedMs int64) bool {
	etag := contentETag(lastModifiedMs)

	if ifNoneMatchMatches(r.Header.Get("If-None-Match"), etag) {
		// RFC 9110: a 304 carries the validators but no body.
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", httpDate(lastModifiedMs))
		setCacheControl(w)
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

// contentETag derives a strong validator from the last-modified timestamp.
//
// Millisecond precision matters: Last-Modified only has second resolution, so
// two changes within the same second would look identical to a client relying
// on it alone. The ETag disambiguates them.
func contentETag(lastModifiedMs int64) string {
	return `"` + strconv.FormatInt(lastModifiedMs, 10) + `"`
}

func httpDate(ms int64) string {
	return clock.FromMillis(ms).Format(http.TimeFormat)
}

func setPublicCacheHeaders(w http.ResponseWriter, lastModifiedMs int64) {
	w.Header().Set("ETag", contentETag(lastModifiedMs))
	w.Header().Set("Last-Modified", httpDate(lastModifiedMs))
	setCacheControl(w)
}

// setCacheControl allows shared caches to serve this for a minute, and to keep
// serving a stale copy for five more while it revalidates in the background.
// Short enough that a publish shows up promptly, long enough to absorb a spike.
func setCacheControl(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "public, max-age=60, stale-while-revalidate=300")
}
