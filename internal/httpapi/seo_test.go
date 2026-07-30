package httpapi_test

import (
	"encoding/xml"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/luongthanhtung03/qdt-test/internal/testutil"
)

type sitemapDoc struct {
	XMLName xml.Name `xml:"urlset"`
	Xmlns   string   `xml:"xmlns,attr"`
	URLs    []struct {
		Loc     string `xml:"loc"`
		LastMod string `xml:"lastmod"`
	} `xml:"url"`
}

func fetchSitemap(t *testing.T, h *testutil.Harness) (*testutil.Response, sitemapDoc) {
	t.Helper()
	resp := h.Do(testutil.Request{Method: http.MethodGet, Path: "/sitemap.xml", NoAuth: true})
	require.Equal(t, http.StatusOK, resp.Status)

	var doc sitemapDoc
	require.NoError(t, xml.Unmarshal(resp.Body, &doc), "sitemap is not valid XML: %s", resp.Body)
	return resp, doc
}

// TestSitemap covers what a crawler actually consumes.
func TestSitemap(t *testing.T) {
	t.Parallel()
	h := testutil.New(t, testutil.WithPublicBaseURL("https://cms.example.com"))

	published := h.CreateContent("published-page", "Published")
	require.Equal(t, http.StatusOK,
		h.POST("/api/v1/contents/"+published.ID+"/publish", map[string]any{"version": 1}).Status)

	// A draft, which must not appear.
	h.CreateContent("draft-page", "Draft")

	// Published but marked noindex, which must also not appear.
	hidden := h.CreateContent("hidden-page", "Hidden")
	require.Equal(t, http.StatusOK, h.PUT("/api/v1/contents/"+hidden.ID,
		testutil.UpdateContentBody{
			Title: "Hidden", Body: "b", SEO: testutil.SEOBody{NoIndex: true},
		},
		map[string]string{"If-Match": testutil.ETag(1)}).Status)
	require.Equal(t, http.StatusOK,
		h.POST("/api/v1/contents/"+hidden.ID+"/publish", map[string]any{"version": 2}).Status)

	resp, doc := fetchSitemap(t, h)

	require.Equal(t, "http://www.sitemaps.org/schemas/sitemap/0.9", doc.Xmlns)
	require.Contains(t, resp.Header.Get("Content-Type"), "application/xml")

	require.Len(t, doc.URLs, 1, "only indexable published content belongs in the sitemap")
	require.Equal(t, "https://cms.example.com/published-page", doc.URLs[0].Loc,
		"sitemap URLs must be absolute and point at the rendered HTML page, not the JSON API")
	require.NotEmpty(t, doc.URLs[0].LastMod)

	// lastmod must parse as a W3C datetime, or crawlers ignore it.
	_, err := time.Parse(time.RFC3339, doc.URLs[0].LastMod)
	require.NoError(t, err, "lastmod must be a valid W3C datetime")

	body := string(resp.Body)
	require.NotContains(t, body, "draft-page", "a draft must never appear in the sitemap")
	require.NotContains(t, body, "hidden-page",
		"listing a noindex page in the sitemap is a contradictory signal")
}

// TestSitemap_Empty checks the document is still well-formed with no content,
// since a malformed empty sitemap is a crawler error rather than a no-op.
func TestSitemap_Empty(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	_, doc := fetchSitemap(t, h)
	require.Empty(t, doc.URLs)
}

// TestSitemap_ReflectsUnpublish checks the sitemap tracks the published
// pointer rather than caching a stale view.
func TestSitemap_ReflectsUnpublish(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	c := h.CreateContent("transient", "Transient")
	require.Equal(t, http.StatusOK,
		h.POST("/api/v1/contents/"+c.ID+"/publish", map[string]any{"version": 1}).Status)

	_, doc := fetchSitemap(t, h)
	require.Len(t, doc.URLs, 1)

	h.Clock.Advance(time.Second)
	require.Equal(t, http.StatusOK,
		h.POST("/api/v1/contents/"+c.ID+"/unpublish", nil).Status)

	_, doc = fetchSitemap(t, h)
	require.Empty(t, doc.URLs, "unpublished content must leave the sitemap")
}

// TestSitemap_Conditional checks the sitemap participates in conditional
// requests, so a crawler polling it repeatedly is cheap.
func TestSitemap_Conditional(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	c := h.CreateContent("cached-sitemap", "Cached")
	require.Equal(t, http.StatusOK,
		h.POST("/api/v1/contents/"+c.ID+"/publish", map[string]any{"version": 1}).Status)

	first, _ := fetchSitemap(t, h)
	etag := first.ETag()
	require.NotEmpty(t, etag)

	resp := h.Do(testutil.Request{
		Method: http.MethodGet, Path: "/sitemap.xml", NoAuth: true,
		Headers: map[string]string{"If-None-Match": etag},
	})
	require.Equal(t, http.StatusNotModified, resp.Status)
	require.Empty(t, resp.Body)
}

// TestRobots checks the crawler directives and the sitemap pointer.
func TestRobots(t *testing.T) {
	t.Parallel()
	h := testutil.New(t, testutil.WithPublicBaseURL("https://cms.example.com"))

	resp := h.Do(testutil.Request{Method: http.MethodGet, Path: "/robots.txt", NoAuth: true})
	require.Equal(t, http.StatusOK, resp.Status)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/plain")

	body := string(resp.Body)
	require.Contains(t, body, "User-agent: *")
	require.Contains(t, body, "Disallow: /api/")
	require.Contains(t, body, "Sitemap: https://cms.example.com/sitemap.xml",
		"crawlers discover the sitemap through robots.txt")
}

// TestSEOMetadataIsPerVersion confirms SEO fields travel with the version, so
// publishing an older version restores that version's metadata too.
func TestSEOMetadataIsPerVersion(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	c := h.CreateContent("seo-versioned", "V1")

	// v2 carries different metadata.
	require.Equal(t, http.StatusOK, h.PUT("/api/v1/contents/"+c.ID,
		testutil.UpdateContentBody{
			Title: "V2", Body: "b",
			SEO: testutil.SEOBody{
				MetaTitle:       "Second title",
				MetaDescription: "Second description",
				CanonicalURL:    "https://cms.example.com/canonical-v2",
			},
		},
		map[string]string{"If-Match": testutil.ETag(1)}).Status)

	require.Equal(t, http.StatusOK,
		h.POST("/api/v1/contents/"+c.ID+"/publish", map[string]any{"version": 2}).Status)

	resp := h.Do(testutil.Request{
		Method: http.MethodGet, Path: "/public/v1/contents/seo-versioned", NoAuth: true,
	})
	var item struct {
		SEO struct {
			MetaTitle       string `json:"meta_title"`
			MetaDescription string `json:"meta_description"`
			CanonicalURL    string `json:"canonical_url"`
		} `json:"seo"`
	}
	resp.JSON(t, &item)
	require.Equal(t, "Second title", item.SEO.MetaTitle)
	require.Equal(t, "https://cms.example.com/canonical-v2", item.SEO.CanonicalURL)

	// Roll back to v1: its empty metadata must come back with it.
	h.Clock.Advance(time.Second)
	require.Equal(t, http.StatusOK,
		h.POST("/api/v1/contents/"+c.ID+"/publish", map[string]any{"version": 1}).Status)

	resp = h.Do(testutil.Request{
		Method: http.MethodGet, Path: "/public/v1/contents/seo-versioned", NoAuth: true,
	})
	resp.JSON(t, &item)
	require.Empty(t, item.SEO.MetaTitle,
		"metadata is part of the version, so a rollback rolls it back too")
}
