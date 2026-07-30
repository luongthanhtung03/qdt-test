package httpapi

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/luongthanhtung03/qdt-test/internal/clock"
	"github.com/luongthanhtung03/qdt-test/internal/store"
)

// The JSON API is what an application consumes. This is what a crawler
// consumes: one server-rendered page per published document, at the canonical
// slug URL, with the metadata in the markup rather than behind a fetch.
//
// It reads through exactly the same store method as the JSON endpoint
// (GetPublicBySlug, which joins through contents.published_version_id), so it
// inherits the same guarantee: a draft has no row to render.

//go:embed templates/page.html
var templateFS embed.FS

// Parsed once at startup. template.Must panics on a malformed template, which
// is correct here -- a broken template is a build-time mistake, and failing at
// boot beats discovering it on the first crawler request.
var pageTemplate = template.Must(template.ParseFS(templateFS, "templates/page.html"))

// pageData is what the template renders. Every field is escaped by
// html/template on the way out.
type pageData struct {
	Title            string // <title> and og:title -- meta_title when set
	Heading          string // the on-page <h1> -- always the content title
	Description      string
	Body             string
	CanonicalURL     string
	OGImage          string
	NoIndex          bool
	PublishedAt      string // RFC3339, for machines
	PublishedAtHuman string // readable, for the page
	UpdatedAt        string
	JSONLD           template.JS
}

// articleLD is the schema.org Article payload search engines read.
type articleLD struct {
	Context       string `json:"@context"`
	Type          string `json:"@type"`
	Headline      string `json:"headline"`
	Description   string `json:"description,omitempty"`
	DatePublished string `json:"datePublished"`
	DateModified  string `json:"dateModified"`
	Image         string `json:"image,omitempty"`
	MainEntity    struct {
		Type string `json:"@type"`
		ID   string `json:"@id"`
	} `json:"mainEntityOfPage"`
}

// handleHTMLPage serves GET /{slug}.
//
// Registered at the root because that is where a canonical content URL belongs:
// /my-article reads better than /public/v1/contents/my-article and is what the
// sitemap advertises. chi matches static routes before wildcards, so /healthz,
// /robots.txt, /sitemap.xml, /api/... and /public/... are unaffected.
func (s *Server) handleHTMLPage(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")

	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	p, err := s.Content.GetPublicBySlug(ctx, slug)
	if err != nil {
		// Same 404 for "no such slug" and "not published", for the same reason
		// the JSON endpoint does it: a distinguishable response would let
		// anyone enumerate unreleased content by probing.
		s.writeHTMLError(w, r, err)
		return
	}

	if s.serveConditional(w, r, p.UpdatedAt) {
		return
	}

	// Render into a buffer first. Writing directly to the ResponseWriter would
	// commit a 200 and a partial page before a template error could surface.
	var buf bytes.Buffer
	if err := pageTemplate.Execute(&buf, s.buildPageData(p)); err != nil {
		writeInternal(w, r, err)
		return
	}

	setPublicCacheHeaders(w, p.UpdatedAt)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// buildPageData maps a published document onto the template's inputs.
func (s *Server) buildPageData(p store.PublicContent) pageData {
	// meta_title overrides the title for search results when set. The on-page
	// <h1> keeps the content title regardless -- having the two differ is the
	// entire reason meta_title exists, since a heading that reads well in the
	// article is often not the one that reads well in a search result.
	title := p.Fields.MetaTitle
	if title == "" {
		title = p.Title
	}

	// An author-supplied canonical wins -- that is the whole point of the
	// field, e.g. when the same article is syndicated from elsewhere.
	canonical := p.Fields.CanonicalURL
	if canonical == "" {
		canonical = s.publicURL(p.Slug)
	}

	published := clock.FromMillis(p.PublishedAt)
	updated := clock.FromMillis(p.UpdatedAt)

	data := pageData{
		Title:            title,
		Heading:          p.Title,
		Description:      p.Fields.MetaDescription,
		Body:             p.Body,
		CanonicalURL:     canonical,
		OGImage:          p.Fields.OGImageURL,
		NoIndex:          p.Fields.NoIndex,
		PublishedAt:      published.Format(rfc3339Milli),
		PublishedAtHuman: published.Format("2 January 2006"),
		UpdatedAt:        updated.Format(rfc3339Milli),
	}

	ld := articleLD{
		Context:       "https://schema.org",
		Type:          "Article",
		Headline:      title,
		Description:   p.Fields.MetaDescription,
		DatePublished: data.PublishedAt,
		DateModified:  data.UpdatedAt,
		Image:         p.Fields.OGImageURL,
	}
	ld.MainEntity.Type = "WebPage"
	ld.MainEntity.ID = canonical

	// json.Marshal escapes <, > and & to \u00xx by default, so the result
	// cannot break out of the script element.
	if encoded, err := json.Marshal(ld); err == nil {
		data.JSONLD = template.JS(encoded)
	}
	return data
}

// errorPage is a minimal standalone page. It carries noindex so a crawler that
// reaches an error does not add it to the index.
const errorPage = `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">` +
	`<meta name="robots" content="noindex"><title>%[1]s</title></head>` +
	`<body><h1>%[1]s</h1></body></html>`

// writeHTMLError returns an HTML error page rather than a JSON envelope, since
// the caller here is a browser or a crawler.
//
// A correct 404 matters for SEO in its own right: a "not found" served as 200
// becomes a soft 404, which search engines treat as a quality problem.
func (s *Server) writeHTMLError(w http.ResponseWriter, r *http.Request, err error) {
	status, heading := http.StatusNotFound, "404 - Not found"
	if !errors.Is(err, store.ErrNotFound) {
		slog.ErrorContext(r.Context(), "rendering page failed",
			"path", r.URL.Path, "error", err)
		status, heading = http.StatusInternalServerError, "500 - Something went wrong"
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, errorPage, heading)
}
