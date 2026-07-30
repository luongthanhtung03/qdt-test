package httpapi_test

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/luongthanhtung03/qdt-test/internal/testutil"
)

// TestConcurrentUpdate_OnlyOneWins is scenario 1 from docs/DESIGN.md section 9,
// and the single most important test in the suite.
//
// Twenty writers read the same version and all try to write on top of it. This
// is the lost-update scenario: without the compare-and-swap, every one of them
// would succeed and nineteen edits would vanish. Exactly one must win.
func TestConcurrentUpdate_OnlyOneWins(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	created := h.CreateContent("race-target", "Original")
	require.EqualValues(t, 1, created.Version)

	const writers = 20
	var succeeded, conflicted, other atomic.Int64

	testutil.Concurrently(writers, func(i int) {
		// Every writer sends the same If-Match: they all read version 1.
		resp := h.UpdateContent(created.ID, 1, fmt.Sprintf("Edit from writer %d", i))
		switch resp.Status {
		case http.StatusOK:
			succeeded.Add(1)
		case http.StatusPreconditionFailed:
			conflicted.Add(1)
		default:
			other.Add(1)
			t.Errorf("unexpected status %d: %s", resp.Status, string(resp.Body))
		}
	})

	require.EqualValues(t, 1, succeeded.Load(), "exactly one writer must win")
	require.EqualValues(t, writers-1, conflicted.Load(), "every other writer must get 412")
	require.EqualValues(t, 0, other.Load())

	// The API view and the database must agree.
	resp := h.GET("/api/v1/contents/" + created.ID)
	require.Equal(t, http.StatusOK, resp.Status)
	var after testutil.ContentResponse
	resp.JSON(t, &after)
	require.EqualValues(t, 2, after.Version, "one successful edit means version 2")
	require.Equal(t, testutil.ETag(2), resp.ETag())

	rows := h.CountRows(`SELECT COUNT(*) FROM content_versions WHERE content_id = ?`, created.ID)
	require.Equal(t, 2, rows, "the original plus exactly one edit")
}

// TestConcurrentCreateSameSlug is scenario 8: the slug unique index has to
// resolve the race, not application-level pre-checking.
func TestConcurrentCreateSameSlug(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	const attempts = 15
	var created, conflicted atomic.Int64

	testutil.Concurrently(attempts, func(i int) {
		resp := h.POST("/api/v1/contents", testutil.CreateContentBody{
			Slug:  "contested-slug",
			Title: fmt.Sprintf("Attempt %d", i),
			Body:  "body",
		})
		switch resp.Status {
		case http.StatusCreated:
			created.Add(1)
		case http.StatusConflict:
			conflicted.Add(1)
			require.Equal(t, "slug_conflict", resp.ErrorCode(t))
		default:
			t.Errorf("unexpected status %d: %s", resp.Status, string(resp.Body))
		}
	})

	require.EqualValues(t, 1, created.Load(), "exactly one create must succeed")
	require.EqualValues(t, attempts-1, conflicted.Load())
	require.Equal(t, 1, h.CountRows(`SELECT COUNT(*) FROM contents WHERE slug = ?`, "contested-slug"))
}

// TestUpdate_IfMatchHandling covers the precondition contract in one table.
func TestUpdate_IfMatchHandling(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)
	created := h.CreateContent("if-match-cases", "Original")

	t.Run("missing If-Match returns 428", func(t *testing.T) {
		resp := h.PUT("/api/v1/contents/"+created.ID,
			testutil.UpdateContentBody{Title: "No header", Body: "b"}, nil)
		require.Equal(t, http.StatusPreconditionRequired, resp.Status)
		require.Equal(t, "precondition_required", resp.ErrorCode(t))
	})

	t.Run("stale If-Match returns 412 with the current version", func(t *testing.T) {
		resp := h.UpdateContent(created.ID, 99, "Stale")
		require.Equal(t, http.StatusPreconditionFailed, resp.Status)

		var body struct {
			Error struct {
				Code    string `json:"code"`
				Details struct {
					CurrentVersion int64  `json:"current_version"`
					ETag           string `json:"etag"`
				} `json:"details"`
			} `json:"error"`
		}
		resp.JSON(t, &body)
		require.Equal(t, "version_conflict", body.Error.Code)
		require.EqualValues(t, 1, body.Error.Details.CurrentVersion,
			"the client should be able to retry without a second GET")
		require.Equal(t, testutil.ETag(1), body.Error.Details.ETag)
	})

	t.Run("weak validator is rejected", func(t *testing.T) {
		resp := h.PUT("/api/v1/contents/"+created.ID,
			testutil.UpdateContentBody{Title: "Weak", Body: "b"},
			map[string]string{"If-Match": `W/"1"`})
		require.Equal(t, http.StatusBadRequest, resp.Status)
		require.Equal(t, "validation_error", resp.ErrorCode(t))
	})

	t.Run("malformed validator is rejected", func(t *testing.T) {
		resp := h.PUT("/api/v1/contents/"+created.ID,
			testutil.UpdateContentBody{Title: "Bad", Body: "b"},
			map[string]string{"If-Match": "not-a-tag"})
		require.Equal(t, http.StatusBadRequest, resp.Status)
	})

	t.Run("unknown id returns 404 not 412", func(t *testing.T) {
		resp := h.UpdateContent("00000000-0000-0000-0000-000000000000", 1, "Ghost")
		require.Equal(t, http.StatusNotFound, resp.Status)
		require.Equal(t, "not_found", resp.ErrorCode(t))
	})

	// Runs last: it is the only subtest that actually mutates state.
	t.Run("wildcard overwrites the current version", func(t *testing.T) {
		resp := h.PUT("/api/v1/contents/"+created.ID,
			testutil.UpdateContentBody{Title: "Wildcard", Body: "b"},
			map[string]string{"If-Match": "*"})
		require.Equal(t, http.StatusOK, resp.Status)
		require.Equal(t, testutil.ETag(2), resp.ETag())
	})
}

// TestVersionHistory checks that editing appends rather than mutates -- the
// property the whole design rests on.
func TestVersionHistory(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	created := h.CreateContent("history", "V1")
	require.Equal(t, http.StatusOK, h.UpdateContent(created.ID, 1, "V2").Status)
	require.Equal(t, http.StatusOK, h.UpdateContent(created.ID, 2, "V3").Status)

	resp := h.GET("/api/v1/contents/" + created.ID + "/versions")
	require.Equal(t, http.StatusOK, resp.Status)

	var list struct {
		Items []struct {
			Version int64  `json:"version"`
			Title   string `json:"title"`
		} `json:"items"`
	}
	resp.JSON(t, &list)
	require.Len(t, list.Items, 3)
	require.Equal(t, "V3", list.Items[0].Title, "newest first")
	require.Equal(t, "V1", list.Items[2].Title)

	// The original version must be byte-for-byte intact.
	one := h.GET("/api/v1/contents/" + created.ID + "/versions/1")
	require.Equal(t, http.StatusOK, one.Status)
	var v struct {
		Version int64  `json:"version"`
		Title   string `json:"title"`
	}
	one.JSON(t, &v)
	require.EqualValues(t, 1, v.Version)
	require.Equal(t, "V1", v.Title, "an edit must never mutate an existing version")

	require.Equal(t, http.StatusNotFound,
		h.GET("/api/v1/contents/"+created.ID+"/versions/99").Status)
}

// TestAuth checks that the admin subtree is closed and the public surface is
// not accidentally behind the token.
func TestAuth(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	t.Run("no token is rejected", func(t *testing.T) {
		resp := h.Do(testutil.Request{
			Method: http.MethodGet, Path: "/api/v1/contents", NoAuth: true,
		})
		require.Equal(t, http.StatusUnauthorized, resp.Status)
		require.Equal(t, "unauthorized", resp.ErrorCode(t))
		require.NotEmpty(t, resp.Header.Get("WWW-Authenticate"))
	})

	t.Run("wrong token is rejected", func(t *testing.T) {
		resp := h.Do(testutil.Request{
			Method: http.MethodGet, Path: "/api/v1/contents", NoAuth: true,
			Headers: map[string]string{"Authorization": "Bearer wrong-token"},
		})
		require.Equal(t, http.StatusUnauthorized, resp.Status)
	})

	t.Run("wrong scheme is rejected", func(t *testing.T) {
		resp := h.Do(testutil.Request{
			Method: http.MethodGet, Path: "/api/v1/contents", NoAuth: true,
			Headers: map[string]string{"Authorization": "Basic " + testutil.AdminToken},
		})
		require.Equal(t, http.StatusUnauthorized, resp.Status)
	})

	t.Run("healthz needs no token", func(t *testing.T) {
		resp := h.Do(testutil.Request{Method: http.MethodGet, Path: "/healthz", NoAuth: true})
		require.Equal(t, http.StatusOK, resp.Status)
	})
}

// TestValidation covers input rules that protect the public URL space.
func TestValidation(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	for _, tc := range []struct {
		name string
		body testutil.CreateContentBody
	}{
		{"empty slug", testutil.CreateContentBody{Slug: "", Title: "T"}},
		{"uppercase slug", testutil.CreateContentBody{Slug: "Not-Lower", Title: "T"}},
		{"spaces in slug", testutil.CreateContentBody{Slug: "has spaces", Title: "T"}},
		{"leading hyphen", testutil.CreateContentBody{Slug: "-leading", Title: "T"}},
		{"double hyphen", testutil.CreateContentBody{Slug: "double--hyphen", Title: "T"}},
		{"path traversal", testutil.CreateContentBody{Slug: "../etc/passwd", Title: "T"}},
		{"empty title", testutil.CreateContentBody{Slug: "valid-slug", Title: ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.POST("/api/v1/contents", tc.body)
			require.Equal(t, http.StatusBadRequest, resp.Status, "body: %s", string(resp.Body))
			require.Equal(t, "validation_error", resp.ErrorCode(t))
		})
	}

	t.Run("unknown field is rejected", func(t *testing.T) {
		resp := h.Do(testutil.Request{
			Method: http.MethodPost, Path: "/api/v1/contents",
			Raw: `{"slug":"ok-slug","title":"T","body":"b","typoed_field":1}`,
		})
		require.Equal(t, http.StatusBadRequest, resp.Status)
	})

	t.Run("malformed JSON is rejected", func(t *testing.T) {
		resp := h.Do(testutil.Request{
			Method: http.MethodPost, Path: "/api/v1/contents", Raw: `{"slug":`,
		})
		require.Equal(t, http.StatusBadRequest, resp.Status)
	})
}
