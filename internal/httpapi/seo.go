package httpapi

import (
	"context"
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/luongthanhtung03/qdt-test/internal/clock"
)

// maxSitemapURLs caps a single sitemap document. The sitemaps protocol allows
// 50,000 URLs per file; beyond that a sitemap index is required. This service
// serves one file, so the limit is explicit rather than implied.
const maxSitemapURLs = 50000

// urlset is the sitemaps.org document root.
type urlset struct {
	XMLName xml.Name     `xml:"urlset"`
	Xmlns   string       `xml:"xmlns,attr"`
	URLs    []sitemapURL `xml:"url"`
}

type sitemapURL struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod,omitempty"`
}

// handleSitemap serves /sitemap.xml.
//
// Only published content appears, because the query joins through
// published_version_id like every other public read. Rows marked noindex are
// excluded as well: listing a URL in a sitemap while telling crawlers not to
// index it is a contradictory signal that search consoles report as an error.
func (s *Server) handleSitemap(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	entries, err := s.DB.ListSitemapEntries(ctx, maxSitemapURLs)
	if err != nil {
		writeInternal(w, r, err)
		return
	}

	latest, err := s.DB.LatestPublishedAt(ctx)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	if s.serveConditional(w, r, latest) {
		return
	}

	doc := urlset{
		Xmlns: "http://www.sitemaps.org/schemas/sitemap/0.9",
		URLs:  make([]sitemapURL, 0, len(entries)),
	}
	for _, e := range entries {
		doc.URLs = append(doc.URLs, sitemapURL{
			Loc: s.publicURL(e.Slug),
			// W3C Datetime format; a date-time with timezone is valid and more
			// precise than a bare date.
			LastMod: clock.FromMillis(e.LastModified).Format("2006-01-02T15:04:05Z07:00"),
		})
	}

	setPublicCacheHeaders(w, latest)
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write([]byte(xml.Header)); err != nil {
		return
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		// The status line is already sent, so this can only be logged.
		writeInternal(w, r, err)
	}
}

// handleRobots serves /robots.txt.
//
// The admin API is disallowed. That is a courtesy to well-behaved crawlers, not
// a security control -- the endpoints require a bearer token regardless.
func (s *Server) handleRobots(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	b.WriteString("User-agent: *\n")
	// Content pages live at the root and are the point of the site, so the
	// default allow stands. Only the machine-facing surfaces are excluded.
	b.WriteString("Disallow: /api/\n")
	b.WriteString("Disallow: /public/\n") // the JSON API; the HTML pages are the indexable form
	b.WriteString("Disallow: /healthz\n")
	b.WriteString("\n")
	b.WriteString("Sitemap: " + s.Cfg.PublicBaseURL + "/sitemap.xml\n")

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

// publicURL builds the canonical absolute URL for a slug.
//
// This points at the server-rendered HTML page, not the JSON endpoint. A
// sitemap whose entries return application/json advertises URLs a crawler
// cannot index, and the loc must match the URL the page is actually served
// from or crawlers treat it as a different page.
func (s *Server) publicURL(slug string) string {
	return s.Cfg.PublicBaseURL + "/" + slug
}
