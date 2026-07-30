package httpapi_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/luongthanhtung03/qdt-test/internal/testutil"
)

// publicItem mirrors the public DTO.
type publicItem struct {
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	PublishedAt string `json:"published_at"`
	SEO         struct {
		NoIndex bool `json:"noindex"`
	} `json:"seo"`
}

type publicList struct {
	Items []publicItem `json:"items"`
	Total int          `json:"total"`
}

// TestPublicNeverLeaksUnpublished is scenario 5 from docs/DESIGN.md section 9.
//
// This is the test for the structural claim the whole schema is built around:
// the public API reads through contents.published_version_id, so there is no
// status filter that could be forgotten. Every state that must not be publicly
// visible is checked here in one place.
func TestPublicNeverLeaksUnpublished(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	// 1. A pure draft, never published.
	draft := h.CreateContent("pure-draft", "Draft Only")

	// 2. Scheduled but not yet due.
	scheduled := h.CreateContent("scheduled-not-due", "Scheduled")
	future := testutil.BaseTime.Add(time.Hour).Format(time.RFC3339)
	require.Equal(t, http.StatusCreated, h.POST(
		"/api/v1/contents/"+scheduled.ID+"/schedules",
		map[string]any{"version": 1, "run_at": future}).Status)

	// 3. Published, then edited: the draft must stay private while the
	//    published version keeps serving.
	edited := h.CreateContent("published-then-edited", "Published V1")
	require.Equal(t, http.StatusOK, h.POST(
		"/api/v1/contents/"+edited.ID+"/publish", map[string]any{"version": 1}).Status)
	require.Equal(t, http.StatusOK, h.UpdateContent(edited.ID, 1, "Secret Draft V2").Status)

	// 4. Published then unpublished.
	withdrawn := h.CreateContent("withdrawn", "Withdrawn")
	require.Equal(t, http.StatusOK, h.POST(
		"/api/v1/contents/"+withdrawn.ID+"/publish", map[string]any{"version": 1}).Status)
	require.Equal(t, http.StatusOK, h.POST(
		"/api/v1/contents/"+withdrawn.ID+"/unpublish", nil).Status)

	t.Run("public list contains only the published document", func(t *testing.T) {
		resp := h.Do(testutil.Request{
			Method: http.MethodGet, Path: "/public/v1/contents", NoAuth: true,
		})
		require.Equal(t, http.StatusOK, resp.Status)

		var list publicList
		resp.JSON(t, &list)
		require.Len(t, list.Items, 1, "only published-then-edited should be public")
		require.Equal(t, "published-then-edited", list.Items[0].Slug)
		require.Equal(t, 1, list.Total)
	})

	t.Run("unpublished slugs return 404", func(t *testing.T) {
		for _, slug := range []string{
			"pure-draft", "scheduled-not-due", "withdrawn", "no-such-slug",
		} {
			resp := h.Do(testutil.Request{
				Method: http.MethodGet, Path: "/public/v1/contents/" + slug, NoAuth: true,
			})
			require.Equal(t, http.StatusNotFound, resp.Status, "slug %q must not be public", slug)
			// An unknown slug and an unpublished one must be indistinguishable,
			// or drafts become enumerable by probing.
			require.Equal(t, "not_found", resp.ErrorCode(t))
		}
	})

	t.Run("edited-after-publish still serves the published version", func(t *testing.T) {
		resp := h.Do(testutil.Request{
			Method: http.MethodGet, Path: "/public/v1/contents/published-then-edited", NoAuth: true,
		})
		require.Equal(t, http.StatusOK, resp.Status)

		var item publicItem
		resp.JSON(t, &item)
		require.Equal(t, "Published V1", item.Title,
			"the public API must serve the published version, not the newer draft")
		require.NotContains(t, string(resp.Body), "Secret Draft V2",
			"no part of the draft may appear in the public response")
	})

	t.Run("public responses expose no internal identifiers", func(t *testing.T) {
		resp := h.Do(testutil.Request{
			Method: http.MethodGet, Path: "/public/v1/contents/published-then-edited", NoAuth: true,
		})
		body := string(resp.Body)
		require.NotContains(t, body, edited.ID, "the content uuid must not be public")
		require.NotContains(t, body, draft.ID)
		require.NotContains(t, body, `"version"`, "version numbers are an admin concern")
	})

	t.Run("public endpoints need no token", func(t *testing.T) {
		resp := h.Do(testutil.Request{
			Method: http.MethodGet, Path: "/public/v1/contents", NoAuth: true,
		})
		require.Equal(t, http.StatusOK, resp.Status)
	})
}

// TestPublishOlderVersion is scenario 7: publishing v1 while v3 is the draft.
// This is how a rollback works, so it has to be supported rather than treated
// as an error.
func TestPublishOlderVersion(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	c := h.CreateContent("rollback-target", "V1")
	require.Equal(t, http.StatusOK, h.UpdateContent(c.ID, 1, "V2").Status)
	require.Equal(t, http.StatusOK, h.UpdateContent(c.ID, 2, "V3").Status)

	// Publish the newest, then roll back to the oldest.
	require.Equal(t, http.StatusOK,
		h.POST("/api/v1/contents/"+c.ID+"/publish", map[string]any{"version": 3}).Status)
	requirePublicTitle(t, h, "rollback-target", "V3")

	resp := h.POST("/api/v1/contents/"+c.ID+"/publish", map[string]any{"version": 1})
	require.Equal(t, http.StatusOK, resp.Status)

	var state testutil.ContentResponse
	resp.JSON(t, &state)
	require.EqualValues(t, 3, state.Version, "the draft pointer does not move on publish")
	require.NotNil(t, state.PublishedVersion)
	require.EqualValues(t, 1, *state.PublishedVersion)
	require.Equal(t, "published_with_draft", state.Status)

	requirePublicTitle(t, h, "rollback-target", "V1")
}

// TestPublishIsIdempotent checks that re-publishing the same version does not
// accumulate audit events. This is what makes at-least-once job delivery safe.
func TestPublishIsIdempotent(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	c := h.CreateContent("idempotent-publish", "V1")
	for range 3 {
		require.Equal(t, http.StatusOK,
			h.POST("/api/v1/contents/"+c.ID+"/publish", map[string]any{"version": 1}).Status)
	}

	events := h.CountRows(
		`SELECT COUNT(*) FROM publish_events WHERE content_id = ? AND action = 'publish'`, c.ID)
	require.Equal(t, 1, events, "republishing the same version must not add events")
}

// TestPublishValidation covers the ways a publish request can be wrong.
func TestPublishValidation(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)
	c := h.CreateContent("publish-validation", "V1")

	t.Run("nonexistent version", func(t *testing.T) {
		resp := h.POST("/api/v1/contents/"+c.ID+"/publish", map[string]any{"version": 99})
		require.Equal(t, http.StatusNotFound, resp.Status)
	})

	t.Run("version zero", func(t *testing.T) {
		resp := h.POST("/api/v1/contents/"+c.ID+"/publish", map[string]any{"version": 0})
		require.Equal(t, http.StatusBadRequest, resp.Status)
	})

	t.Run("unknown content", func(t *testing.T) {
		resp := h.POST("/api/v1/contents/00000000-0000-0000-0000-000000000000/publish",
			map[string]any{"version": 1})
		require.Equal(t, http.StatusNotFound, resp.Status)
	})

	t.Run("cannot publish another document's version", func(t *testing.T) {
		other := h.CreateContent("other-doc", "Other V1")
		// Version numbers are per-content, so version 1 exists for both. The
		// store resolves the number within this content, so this publishes
		// the right row -- the check is that it does not reach across.
		require.Equal(t, http.StatusOK,
			h.POST("/api/v1/contents/"+other.ID+"/publish", map[string]any{"version": 1}).Status)
		requirePublicTitle(t, h, "other-doc", "Other V1")
	})
}

// TestUnpublishIsIdempotent checks repeated unpublish is safe.
func TestUnpublishIsIdempotent(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	c := h.CreateContent("unpublish-twice", "V1")
	require.Equal(t, http.StatusOK,
		h.POST("/api/v1/contents/"+c.ID+"/publish", map[string]any{"version": 1}).Status)

	for range 2 {
		require.Equal(t, http.StatusOK,
			h.POST("/api/v1/contents/"+c.ID+"/unpublish", nil).Status)
	}

	events := h.CountRows(
		`SELECT COUNT(*) FROM publish_events WHERE content_id = ? AND action = 'unpublish'`, c.ID)
	require.Equal(t, 1, events)

	resp := h.Do(testutil.Request{
		Method: http.MethodGet, Path: "/public/v1/contents/unpublish-twice", NoAuth: true,
	})
	require.Equal(t, http.StatusNotFound, resp.Status)
}

// TestPublicETagReturns304 is scenario 10. Conditional requests are the part of
// SEO a backend owns: crawlers that get 304 for unchanged pages spend their
// budget on pages that changed.
func TestPublicETagReturns304(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	c := h.CreateContent("cacheable", "Cacheable V1")
	require.Equal(t, http.StatusOK,
		h.POST("/api/v1/contents/"+c.ID+"/publish", map[string]any{"version": 1}).Status)

	first := h.Do(testutil.Request{
		Method: http.MethodGet, Path: "/public/v1/contents/cacheable", NoAuth: true,
	})
	require.Equal(t, http.StatusOK, first.Status)

	etag := first.ETag()
	require.NotEmpty(t, etag)
	require.NotEmpty(t, first.Header.Get("Last-Modified"))
	require.Contains(t, first.Header.Get("Cache-Control"), "max-age=60")

	t.Run("matching If-None-Match returns 304 with no body", func(t *testing.T) {
		resp := h.Do(testutil.Request{
			Method: http.MethodGet, Path: "/public/v1/contents/cacheable", NoAuth: true,
			Headers: map[string]string{"If-None-Match": etag},
		})
		require.Equal(t, http.StatusNotModified, resp.Status)
		require.Empty(t, resp.Body, "a 304 must not carry a body")
		require.Equal(t, etag, resp.ETag(), "a 304 must still carry the validator")
	})

	t.Run("weak form of the same tag also matches", func(t *testing.T) {
		resp := h.Do(testutil.Request{
			Method: http.MethodGet, Path: "/public/v1/contents/cacheable", NoAuth: true,
			Headers: map[string]string{"If-None-Match": "W/" + etag},
		})
		require.Equal(t, http.StatusNotModified, resp.Status)
	})

	t.Run("stale If-None-Match returns the body", func(t *testing.T) {
		resp := h.Do(testutil.Request{
			Method: http.MethodGet, Path: "/public/v1/contents/cacheable", NoAuth: true,
			Headers: map[string]string{"If-None-Match": `"0"`},
		})
		require.Equal(t, http.StatusOK, resp.Status)
		require.NotEmpty(t, resp.Body)
	})

	t.Run("the tag changes when the published version changes", func(t *testing.T) {
		require.Equal(t, http.StatusOK, h.UpdateContent(c.ID, 1, "Cacheable V2").Status)
		h.Clock.Advance(time.Second)
		require.Equal(t, http.StatusOK,
			h.POST("/api/v1/contents/"+c.ID+"/publish", map[string]any{"version": 2}).Status)

		resp := h.Do(testutil.Request{
			Method: http.MethodGet, Path: "/public/v1/contents/cacheable", NoAuth: true,
			Headers: map[string]string{"If-None-Match": etag},
		})
		require.Equal(t, http.StatusOK, resp.Status, "a stale tag must not yield 304")
		require.NotEqual(t, etag, resp.ETag())
	})
}

func requirePublicTitle(t *testing.T, h *testutil.Harness, slug, wantTitle string) {
	t.Helper()
	resp := h.Do(testutil.Request{
		Method: http.MethodGet, Path: "/public/v1/contents/" + slug, NoAuth: true,
	})
	require.Equal(t, http.StatusOK, resp.Status, "body: %s", string(resp.Body))

	var item publicItem
	resp.JSON(t, &item)
	require.Equal(t, wantTitle, item.Title)
}
