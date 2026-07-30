package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/luongthanhtung03/qdt-test/internal/testutil"
)

func getHTML(t *testing.T, h *testutil.Harness, path string) *testutil.Response {
	t.Helper()
	return h.Do(testutil.Request{Method: http.MethodGet, Path: path, NoAuth: true})
}

// TestHTMLPage_RendersPublishedContent checks the markup a crawler actually
// reads. Each assertion maps to something a search engine consumes.
func TestHTMLPage_RendersPublishedContent(t *testing.T) {
	t.Parallel()
	h := testutil.New(t, testutil.WithPublicBaseURL("https://cms.example.com"))

	c := h.CreateContent("seo-page", "Original Title")
	require.Equal(t, http.StatusOK, h.PUT("/api/v1/contents/"+c.ID,
		testutil.UpdateContentBody{
			Title: "Original Title",
			Body:  "The body of the article.",
			SEO: testutil.SEOBody{
				MetaTitle:       "Search Engine Title",
				MetaDescription: "A description for the SERP.",
				OGImageURL:      "https://cms.example.com/img/cover.png",
			},
		},
		map[string]string{"If-Match": testutil.ETag(1)}).Status)
	require.Equal(t, http.StatusOK,
		h.POST("/api/v1/contents/"+c.ID+"/publish", map[string]any{"version": 2}).Status)

	resp := getHTML(t, h, "/seo-page")
	require.Equal(t, http.StatusOK, resp.Status)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/html")

	body := string(resp.Body)

	t.Run("title prefers meta_title but the h1 does not", func(t *testing.T) {
		require.Contains(t, body, "<title>Search Engine Title</title>",
			"meta_title exists precisely so the SERP title can differ from the on-page one")
		require.Contains(t, body, "<h1>Original Title</h1>",
			"the on-page heading must stay the content title; collapsing the two "+
				"would make meta_title pointless")
	})

	t.Run("meta description", func(t *testing.T) {
		require.Contains(t, body, `<meta name="description" content="A description for the SERP.">`)
	})

	t.Run("canonical defaults to the page url", func(t *testing.T) {
		require.Contains(t, body, `<link rel="canonical" href="https://cms.example.com/seo-page">`)
	})

	t.Run("open graph tags", func(t *testing.T) {
		require.Contains(t, body, `<meta property="og:type" content="article">`)
		require.Contains(t, body, `<meta property="og:title" content="Search Engine Title">`)
		require.Contains(t, body, `<meta property="og:url" content="https://cms.example.com/seo-page">`)
		require.Contains(t, body, `content="https://cms.example.com/img/cover.png"`)
	})

	t.Run("content is present in the markup, not behind a fetch", func(t *testing.T) {
		require.Contains(t, body, "The body of the article.",
			"a crawler must see the text without executing JavaScript")
	})

	t.Run("json-ld parses and describes an Article", func(t *testing.T) {
		ld := extractJSONLD(t, body)
		require.Equal(t, "https://schema.org", ld["@context"])
		require.Equal(t, "Article", ld["@type"])
		require.Equal(t, "Search Engine Title", ld["headline"])
		require.NotEmpty(t, ld["datePublished"])
	})

	t.Run("not marked noindex by default", func(t *testing.T) {
		require.NotContains(t, body, `name="robots"`)
	})
}

// TestHTMLPage_NeverLeaksUnpublished is the leak test again, for the new
// surface. A rendering endpoint is exactly where a draft would escape if the
// query were written independently, so it is worth asserting separately from
// the JSON one.
func TestHTMLPage_NeverLeaksUnpublished(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	// A pure draft.
	h.CreateContent("html-draft", "Draft Only")

	// Published, then edited: the draft text must not appear.
	edited := h.CreateContent("html-edited", "Published V1")
	require.Equal(t, http.StatusOK,
		h.POST("/api/v1/contents/"+edited.ID+"/publish", map[string]any{"version": 1}).Status)
	require.Equal(t, http.StatusOK, h.PUT("/api/v1/contents/"+edited.ID,
		testutil.UpdateContentBody{Title: "SECRET DRAFT", Body: "UNRELEASED TEXT"},
		map[string]string{"If-Match": testutil.ETag(1)}).Status)

	// Published, then withdrawn.
	gone := h.CreateContent("html-withdrawn", "Withdrawn")
	require.Equal(t, http.StatusOK,
		h.POST("/api/v1/contents/"+gone.ID+"/publish", map[string]any{"version": 1}).Status)
	require.Equal(t, http.StatusOK,
		h.POST("/api/v1/contents/"+gone.ID+"/unpublish", nil).Status)

	t.Run("drafts and withdrawn content 404", func(t *testing.T) {
		for _, slug := range []string{"html-draft", "html-withdrawn", "no-such-page"} {
			resp := getHTML(t, h, "/"+slug)
			require.Equal(t, http.StatusNotFound, resp.Status, "slug %q", slug)
			require.Contains(t, string(resp.Body), `name="robots"`,
				"an error page must tell crawlers not to index it")
		}
	})

	t.Run("a published page never renders its newer draft", func(t *testing.T) {
		body := string(getHTML(t, h, "/html-edited").Body)
		require.Contains(t, body, "Published V1")
		require.NotContains(t, body, "SECRET DRAFT")
		require.NotContains(t, body, "UNRELEASED TEXT")
	})
}

// TestHTMLPage_NoIndex checks that a noindex document still renders for a
// direct visitor but tells crawlers to stay away, and stays out of the sitemap.
func TestHTMLPage_NoIndex(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	c := h.CreateContent("hidden-page", "Hidden")
	require.Equal(t, http.StatusOK, h.PUT("/api/v1/contents/"+c.ID,
		testutil.UpdateContentBody{
			Title: "Hidden", Body: "b", SEO: testutil.SEOBody{NoIndex: true},
		},
		map[string]string{"If-Match": testutil.ETag(1)}).Status)
	require.Equal(t, http.StatusOK,
		h.POST("/api/v1/contents/"+c.ID+"/publish", map[string]any{"version": 2}).Status)

	resp := getHTML(t, h, "/hidden-page")
	require.Equal(t, http.StatusOK, resp.Status, "noindex still serves to real visitors")
	require.Contains(t, string(resp.Body), `<meta name="robots" content="noindex, nofollow">`)

	_, doc := fetchSitemap(t, h)
	require.Empty(t, doc.URLs, "and it stays out of the sitemap")
}

// TestHTMLPage_AuthorCanonicalWins covers syndication: when the author supplies
// a canonical URL it must override the generated one, since that is the entire
// purpose of the field.
func TestHTMLPage_AuthorCanonicalWins(t *testing.T) {
	t.Parallel()
	h := testutil.New(t, testutil.WithPublicBaseURL("https://cms.example.com"))

	c := h.CreateContent("syndicated", "Syndicated")
	require.Equal(t, http.StatusOK, h.PUT("/api/v1/contents/"+c.ID,
		testutil.UpdateContentBody{
			Title: "Syndicated", Body: "b",
			SEO: testutil.SEOBody{CanonicalURL: "https://origin.example.org/the-original"},
		},
		map[string]string{"If-Match": testutil.ETag(1)}).Status)
	require.Equal(t, http.StatusOK,
		h.POST("/api/v1/contents/"+c.ID+"/publish", map[string]any{"version": 2}).Status)

	body := string(getHTML(t, h, "/syndicated").Body)
	require.Contains(t, body, `<link rel="canonical" href="https://origin.example.org/the-original">`)
	require.NotContains(t, body, `rel="canonical" href="https://cms.example.com/syndicated"`)
}

// TestHTMLPage_DoesNotShadowOtherRoutes is the risk of mounting a wildcard at
// the root. chi matches static segments first, but that is worth proving rather
// than trusting -- a regression here would silently break the whole API.
func TestHTMLPage_DoesNotShadowOtherRoutes(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	c := h.CreateContent("healthz-lookalike", "Real page")
	require.Equal(t, http.StatusOK,
		h.POST("/api/v1/contents/"+c.ID+"/publish", map[string]any{"version": 1}).Status)

	for _, tc := range []struct {
		path      string
		wantType  string
		wantsAuth bool
	}{
		{path: "/healthz", wantType: "application/json"},
		{path: "/robots.txt", wantType: "text/plain"},
		{path: "/sitemap.xml", wantType: "application/xml"},
		{path: "/public/v1/contents", wantType: "application/json"},
		{path: "/public/v1/contents/healthz-lookalike", wantType: "application/json"},
		{path: "/api/v1/contents", wantType: "application/json", wantsAuth: true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			resp := h.Do(testutil.Request{
				Method: http.MethodGet, Path: tc.path, NoAuth: !tc.wantsAuth,
			})
			require.Equal(t, http.StatusOK, resp.Status)
			require.Contains(t, resp.Header.Get("Content-Type"), tc.wantType,
				"the wildcard page route must not have swallowed %s", tc.path)
		})
	}

	// And the HTML route itself still works.
	require.Equal(t, http.StatusOK, getHTML(t, h, "/healthz-lookalike").Status)
}

// TestHTMLPage_EscapesContent checks that stored text cannot inject markup.
// Content is author-supplied, and html/template escaping is the only thing
// standing between a title and a stored XSS.
func TestHTMLPage_EscapesContent(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	const payload = `<script>alert("xss")</script>`
	c := h.CreateContent("escaping", "safe")
	require.Equal(t, http.StatusOK, h.PUT("/api/v1/contents/"+c.ID,
		testutil.UpdateContentBody{
			Title: payload, Body: payload,
			SEO: testutil.SEOBody{MetaDescription: payload},
		},
		map[string]string{"If-Match": testutil.ETag(1)}).Status)
	require.Equal(t, http.StatusOK,
		h.POST("/api/v1/contents/"+c.ID+"/publish", map[string]any{"version": 2}).Status)

	body := string(getHTML(t, h, "/escaping").Body)
	require.NotContains(t, body, "<script>alert",
		"author content must never be rendered as live markup")
	require.Contains(t, body, "&lt;script&gt;", "it should appear escaped instead")
}

// TestHTMLPage_Conditional checks the page participates in conditional
// requests, which is what keeps repeat crawls cheap.
func TestHTMLPage_Conditional(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	c := h.CreateContent("cacheable-page", "Cacheable")
	require.Equal(t, http.StatusOK,
		h.POST("/api/v1/contents/"+c.ID+"/publish", map[string]any{"version": 1}).Status)

	first := getHTML(t, h, "/cacheable-page")
	require.Equal(t, http.StatusOK, first.Status)
	etag := first.ETag()
	require.NotEmpty(t, etag)
	require.Contains(t, first.Header.Get("Cache-Control"), "max-age=60")

	resp := h.Do(testutil.Request{
		Method: http.MethodGet, Path: "/cacheable-page", NoAuth: true,
		Headers: map[string]string{"If-None-Match": etag},
	})
	require.Equal(t, http.StatusNotModified, resp.Status)
	require.Empty(t, resp.Body)
}

// TestSitemapPointsAtRenderedPages guards the loop this whole file closes: a
// sitemap advertising JSON URLs lists pages no crawler can index.
func TestSitemapPointsAtRenderedPages(t *testing.T) {
	t.Parallel()
	h := testutil.New(t, testutil.WithPublicBaseURL("https://cms.example.com"))

	c := h.CreateContent("indexable", "Indexable")
	require.Equal(t, http.StatusOK,
		h.POST("/api/v1/contents/"+c.ID+"/publish", map[string]any{"version": 1}).Status)

	_, doc := fetchSitemap(t, h)
	require.Len(t, doc.URLs, 1)
	require.Equal(t, "https://cms.example.com/indexable", doc.URLs[0].Loc)

	// Follow the advertised URL and confirm it really is an indexable page.
	resp := getHTML(t, h, "/indexable")
	require.Equal(t, http.StatusOK, resp.Status)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/html",
		"every URL in the sitemap must resolve to something a crawler can index")
}

// extractJSONLD pulls the ld+json block out of the page and parses it.
func extractJSONLD(t *testing.T, body string) map[string]any {
	t.Helper()

	const open = `<script type="application/ld+json">`
	start := strings.Index(body, open)
	require.GreaterOrEqual(t, start, 0, "page must contain a JSON-LD block")
	rest := body[start+len(open):]
	end := strings.Index(rest, "</script>")
	require.GreaterOrEqual(t, end, 0, "JSON-LD block must be closed")

	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(rest[:end]), &out),
		"JSON-LD must be valid JSON or search engines ignore it: %s", rest[:end])
	return out
}
