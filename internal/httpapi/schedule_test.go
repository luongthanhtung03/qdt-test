package httpapi_test

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/luongthanhtung03/qdt-test/internal/testutil"
)

type scheduleResponse struct {
	ID        string `json:"id"`
	ContentID string `json:"content_id"`
	Version   int64  `json:"version"`
	RunAt     string `json:"run_at"`
	Status    string `json:"status"`
	Attempts  int64  `json:"attempts"`
}

func futureRFC3339(d time.Duration) string {
	return testutil.BaseTime.Add(d).Format(time.RFC3339)
}

// TestCreateSchedule covers the happy path and the input rules.
func TestCreateSchedule(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)
	c := h.CreateContent("schedulable", "V1")

	t.Run("creates a pending schedule", func(t *testing.T) {
		resp := h.POST("/api/v1/contents/"+c.ID+"/schedules", map[string]any{
			"version": 1, "run_at": futureRFC3339(time.Hour),
		})
		require.Equal(t, http.StatusCreated, resp.Status, "body: %s", string(resp.Body))

		var sc scheduleResponse
		resp.JSON(t, &sc)
		require.Equal(t, "pending", sc.Status)
		require.EqualValues(t, 1, sc.Version)
		require.EqualValues(t, 0, sc.Attempts)
		require.NotEmpty(t, resp.Header.Get("Location"))
	})

	t.Run("a second active schedule is rejected", func(t *testing.T) {
		resp := h.POST("/api/v1/contents/"+c.ID+"/schedules", map[string]any{
			"version": 1, "run_at": futureRFC3339(2 * time.Hour),
		})
		require.Equal(t, http.StatusConflict, resp.Status)
		require.Equal(t, "schedule_conflict", resp.ErrorCode(t))
	})

	t.Run("past run_at is rejected", func(t *testing.T) {
		other := h.CreateContent("past-schedule", "V1")
		resp := h.POST("/api/v1/contents/"+other.ID+"/schedules", map[string]any{
			"version": 1, "run_at": testutil.BaseTime.Add(-time.Hour).Format(time.RFC3339),
		})
		require.Equal(t, http.StatusBadRequest, resp.Status)
		require.Equal(t, "validation_error", resp.ErrorCode(t))
	})

	t.Run("malformed run_at is rejected", func(t *testing.T) {
		other := h.CreateContent("bad-runat", "V1")
		resp := h.POST("/api/v1/contents/"+other.ID+"/schedules", map[string]any{
			"version": 1, "run_at": "tomorrow please",
		})
		require.Equal(t, http.StatusBadRequest, resp.Status)
	})

	t.Run("unknown version is rejected", func(t *testing.T) {
		other := h.CreateContent("bad-version", "V1")
		resp := h.POST("/api/v1/contents/"+other.ID+"/schedules", map[string]any{
			"version": 42, "run_at": futureRFC3339(time.Hour),
		})
		require.Equal(t, http.StatusNotFound, resp.Status)
	})
}

// TestConcurrentScheduleSameContent proves ux_schedule_active_per_content, not
// application-level checking, resolves the race.
func TestConcurrentScheduleSameContent(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)
	c := h.CreateContent("contested-schedule", "V1")

	const attempts = 12
	var created, conflicted atomic.Int64

	testutil.Concurrently(attempts, func(i int) {
		resp := h.POST("/api/v1/contents/"+c.ID+"/schedules", map[string]any{
			"version": 1, "run_at": futureRFC3339(time.Duration(i+1) * time.Hour),
		})
		switch resp.Status {
		case http.StatusCreated:
			created.Add(1)
		case http.StatusConflict:
			conflicted.Add(1)
		default:
			t.Errorf("unexpected status %d: %s", resp.Status, string(resp.Body))
		}
	})

	require.EqualValues(t, 1, created.Load(), "exactly one schedule may be active")
	require.EqualValues(t, attempts-1, conflicted.Load())
	require.Equal(t, 1, h.CountRows(
		`SELECT COUNT(*) FROM publish_schedules WHERE content_id = ? AND status IN ('pending','claimed')`,
		c.ID))
}

// TestCancelSchedule covers cancellation while no worker is running, so the
// outcome is deterministic. The racing case is covered by
// TestCancelRacesDueTime in the scheduler package.
func TestCancelSchedule(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)
	c := h.CreateContent("cancellable", "V1")

	resp := h.POST("/api/v1/contents/"+c.ID+"/schedules", map[string]any{
		"version": 1, "run_at": futureRFC3339(time.Hour),
	})
	require.Equal(t, http.StatusCreated, resp.Status)

	var sc scheduleResponse
	resp.JSON(t, &sc)

	t.Run("cancels a pending schedule", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent, h.DELETE("/api/v1/schedules/"+sc.ID).Status)
		require.Equal(t, 1, h.CountRows(
			`SELECT COUNT(*) FROM publish_schedules WHERE id = ? AND status = 'cancelled'`, sc.ID))
	})

	t.Run("cancelling twice is a conflict, not a silent success", func(t *testing.T) {
		resp := h.DELETE("/api/v1/schedules/" + sc.ID)
		require.Equal(t, http.StatusConflict, resp.Status)
		require.Equal(t, "schedule_conflict", resp.ErrorCode(t))
	})

	t.Run("unknown schedule returns 404", func(t *testing.T) {
		require.Equal(t, http.StatusNotFound,
			h.DELETE("/api/v1/schedules/00000000-0000-0000-0000-000000000000").Status)
	})

	t.Run("cancelling frees the slot for a new schedule", func(t *testing.T) {
		resp := h.POST("/api/v1/contents/"+c.ID+"/schedules", map[string]any{
			"version": 1, "run_at": futureRFC3339(3 * time.Hour),
		})
		require.Equal(t, http.StatusCreated, resp.Status,
			"a cancelled schedule must not keep blocking the content")
	})
}

// TestListSchedules checks the listing endpoint.
func TestListSchedules(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)
	c := h.CreateContent("listable-schedules", "V1")

	require.Equal(t, http.StatusCreated, h.POST("/api/v1/contents/"+c.ID+"/schedules",
		map[string]any{"version": 1, "run_at": futureRFC3339(time.Hour)}).Status)

	resp := h.GET("/api/v1/contents/" + c.ID + "/schedules")
	require.Equal(t, http.StatusOK, resp.Status)

	var list struct {
		Items []scheduleResponse `json:"items"`
	}
	resp.JSON(t, &list)
	require.Len(t, list.Items, 1)
	require.Equal(t, "pending", list.Items[0].Status)

	require.Equal(t, http.StatusNotFound,
		h.GET("/api/v1/contents/00000000-0000-0000-0000-000000000000/schedules").Status)
}
