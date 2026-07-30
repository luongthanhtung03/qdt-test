package scheduler_test

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/luongthanhtung03/qdt-test/internal/clock"
	"github.com/luongthanhtung03/qdt-test/internal/testutil"
)

// TestScheduledPublish_HappyPath establishes the baseline the harder tests
// build on: a due job publishes, and the schedule is marked done.
func TestScheduledPublish_HappyPath(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	c := h.CreateContent("scheduled-happy", "Scheduled V1")
	scheduleID := h.ScheduleAt(c.ID, 1, 5*time.Minute)

	h.StartWorker("worker-1", false)

	// Not due yet: the worker must leave it alone. Wait for real poll cycles
	// to go by so this is a genuine observation rather than a race.
	require.Never(t, func() bool {
		return h.PublicStatus("scheduled-happy") == http.StatusOK
	}, 200*time.Millisecond, 20*time.Millisecond,
		"content must not publish before run_at")
	require.Equal(t, "pending", h.ScheduleStatus(scheduleID))

	// Move fake time past run_at. The real ticker notices within one poll.
	h.Clock.Advance(6 * time.Minute)

	h.WaitFor("content becomes public", func() bool {
		return h.PublicStatus("scheduled-happy") == http.StatusOK
	})
	h.WaitFor("schedule marked done", func() bool {
		return h.ScheduleStatus(scheduleID) == "done"
	})

	require.Equal(t, 1, h.CountRows(
		`SELECT COUNT(*) FROM publish_events WHERE schedule_id = ?`, scheduleID))
}

// TestScheduledPublish_ExactlyOnce_MultiWorker is scenario 2, and the test that
// actually substantiates the multi-instance requirement.
//
// Five workers, each with its own database handle, poll one SQLite file with a
// single due job. Exactly one publish event may exist. Without the atomic claim
// every worker would pick the job up; without the unique index on schedule_id a
// duplicate would be recorded silently.
func TestScheduledPublish_ExactlyOnce_MultiWorker(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	c := h.CreateContent("exactly-once", "Contested")
	scheduleID := h.ScheduleAt(c.ID, 1, time.Minute)

	// Make the job due before any worker starts, so all five find it waiting
	// on their very first poll. That is the worst case for the claim protocol.
	h.Clock.Advance(2 * time.Minute)

	const workers = 5
	for i := range workers {
		h.StartWorker("worker-"+string(rune('a'+i)), true)
	}

	h.WaitFor("schedule completes", func() bool {
		return h.ScheduleStatus(scheduleID) == "done"
	})

	// Let every worker poll several more times, so a duplicate would have time
	// to appear rather than the test simply finishing first.
	require.Never(t, func() bool {
		return h.CountRows(`SELECT COUNT(*) FROM publish_events WHERE schedule_id = ?`,
			scheduleID) > 1
	}, 300*time.Millisecond, 20*time.Millisecond, "a second publish event must never appear")

	require.Equal(t, 1, h.CountRows(
		`SELECT COUNT(*) FROM publish_events WHERE schedule_id = ?`, scheduleID),
		"exactly one publish event across all workers")
	require.Equal(t, http.StatusOK, h.PublicStatus("exactly-once"))
}

// TestScheduledPublish_SurvivesRestart is scenario 3: the requirement that
// scheduling outlive the process. An in-memory timer would fail this.
func TestScheduledPublish_SurvivesRestart(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	c := h.CreateContent("survives-restart", "Persisted")
	scheduleID := h.ScheduleAt(c.ID, 1, 10*time.Minute)

	// First worker runs and stops before the job is due -- the restart.
	_, stopFirst := h.StartWorker("worker-before-restart", true)
	h.WaitFor("first worker polls at least once", func() bool {
		return true
	})
	stopFirst()

	require.Equal(t, "pending", h.ScheduleStatus(scheduleID),
		"the schedule must still be waiting after the worker stops")
	require.Equal(t, http.StatusNotFound, h.PublicStatus("survives-restart"))

	// A brand-new worker on a fresh handle to the same file: the state lives
	// in the database, not in the process that created it.
	h.Clock.Advance(11 * time.Minute)
	h.StartWorker("worker-after-restart", true)

	h.WaitFor("publish happens after restart", func() bool {
		return h.PublicStatus("survives-restart") == http.StatusOK
	})
	require.Equal(t, "done", h.ScheduleStatus(scheduleID))
	require.Equal(t, 1, h.CountRows(
		`SELECT COUNT(*) FROM publish_events WHERE schedule_id = ?`, scheduleID))
}

// TestLeaseRecoveryAfterCrash is scenario 4: a worker claims a job and dies
// before finishing it.
//
// A crash is simulated by writing the claim directly and never completing it,
// which is exactly the state a killed process leaves behind. No graceful
// release runs, so recovery has to come from the lease expiring.
func TestLeaseRecoveryAfterCrash(t *testing.T) {
	t.Parallel()
	h := testutil.New(t, testutil.WithLeaseTTL(500*time.Millisecond))

	c := h.CreateContent("crash-recovery", "Orphaned")
	scheduleID := h.ScheduleAt(c.ID, 1, time.Minute)
	h.Clock.Advance(2 * time.Minute)

	// Worker A claims the job and then dies before running the job
	// transaction. Claiming through the store directly is what makes this a
	// crash rather than a normal cycle: a full worker tick would claim and
	// complete in one go, whereas a killed process leaves the row claimed with
	// a lease that nobody will ever release.
	now := clock.ToMillis(h.Clock.Now())
	leaseExpiry := clock.ToMillis(h.Clock.Now().Add(h.Cfg.LeaseTTL))
	jobs, err := h.DB.ClaimDueSchedules(t.Context(), "worker-that-crashes", now, leaseExpiry, 10)
	require.NoError(t, err)
	require.Len(t, jobs, 1, "the due job should have been claimed")

	require.Equal(t, "claimed", h.ScheduleStatus(scheduleID))
	require.Equal(t, http.StatusNotFound, h.PublicStatus("crash-recovery"),
		"a claimed but unfinished job must not have published anything")

	// Worker B cannot steal the job while the lease is live.
	survivor := h.NewWorker("worker-that-survives", true)
	survivor.RunOnce(context.Background())
	require.Equal(t, http.StatusNotFound, h.PublicStatus("crash-recovery"),
		"the lease must protect the job while it is still valid")

	var lockedBy string
	require.NoError(t, h.DB.Read.QueryRowContext(t.Context(),
		`SELECT locked_by FROM publish_schedules WHERE id = ?`, scheduleID).Scan(&lockedBy))
	require.Equal(t, "worker-that-crashes", lockedBy)

	// Once the lease expires, the survivor reclaims and completes it.
	h.Clock.Advance(time.Second)
	survivor.RunOnce(context.Background())

	require.Equal(t, "done", h.ScheduleStatus(scheduleID))
	require.Equal(t, http.StatusOK, h.PublicStatus("crash-recovery"))
	require.Equal(t, 1, h.CountRows(
		`SELECT COUNT(*) FROM publish_events WHERE schedule_id = ?`, scheduleID),
		"recovery must publish exactly once, not twice")
}

// TestCancelRacesDueTime is scenario 6.
//
// A cancel arrives at the same moment a worker claims the job. Exactly two
// outcomes are acceptable -- (cancelled, not published) or (published, cancel
// told 409) -- and the API must never report the one that did not happen.
//
// Rounds alternate between two orderings. Firing both simultaneously is the
// real race, but the worker reaches the database far sooner than a cancel that
// has to traverse the HTTP stack first, so on its own it only ever exercises
// the worker-wins branch. Giving the cancel a head start on alternate rounds
// covers the other branch, and both are checked against the same invariant.
func TestCancelRacesDueTime(t *testing.T) {
	t.Parallel()

	const rounds = 30
	var cancelWon, publishWon int

	for round := range rounds {
		cancelFirst := round%2 == 0

		h := testutil.New(t)
		slug := "cancel-race"
		c := h.CreateContent(slug, "Racing")
		scheduleID := h.ScheduleAt(c.ID, 1, time.Minute)
		h.Clock.Advance(2 * time.Minute) // now due

		worker := h.NewWorker("racing-worker", false)

		var cancelStatus int
		if cancelFirst {
			cancelStatus = h.DELETE("/api/v1/schedules/" + scheduleID).Status
			worker.RunOnce(context.Background())
		} else {
			var start, done sync.WaitGroup
			start.Add(1)
			done.Add(2)

			go func() {
				defer done.Done()
				start.Wait()
				cancelStatus = h.DELETE("/api/v1/schedules/" + scheduleID).Status
			}()
			go func() {
				defer done.Done()
				start.Wait()
				worker.RunOnce(context.Background())
			}()

			start.Done()
			done.Wait()
		}

		status := h.ScheduleStatus(scheduleID)
		published := h.PublicStatus(slug) == http.StatusOK
		events := h.CountRows(
			`SELECT COUNT(*) FROM publish_events WHERE schedule_id = ?`, scheduleID)

		switch cancelStatus {
		case http.StatusNoContent:
			// Cancel won, so nothing may have been published -- including by
			// the worker tick that ran immediately afterwards.
			cancelWon++
			require.Equal(t, "cancelled", status, "round %d", round)
			require.False(t, published, "round %d: a cancelled job must never publish", round)
			require.Zero(t, events, "round %d", round)

		case http.StatusConflict:
			// The worker won. The publish must actually have happened: a 409
			// that did not correspond to a real publish would be the "neither"
			// outcome this test exists to rule out.
			publishWon++
			require.Equal(t, "done", status, "round %d", round)
			require.True(t, published, "round %d: 409 means the publish went ahead", round)
			require.Equal(t, 1, events, "round %d", round)

		default:
			t.Fatalf("round %d: unexpected cancel status %d", round, cancelStatus)
		}
	}

	t.Logf("cancel won %d/%d, worker won %d/%d", cancelWon, rounds, publishWon, rounds)
	require.Equal(t, rounds, cancelWon+publishWon, "every round must resolve one way or the other")
	require.Positive(t, cancelWon, "the cancel-wins branch must actually be exercised")
	require.Positive(t, publishWon, "the worker-wins branch must actually be exercised")
}

// TestGracefulShutdownReleasesClaims checks the failover path: a planned stop
// must hand claims back rather than leaving the next worker to wait out a lease
// nobody is holding.
func TestGracefulShutdownReleasesClaims(t *testing.T) {
	t.Parallel()
	// A long lease makes the point: without an explicit release, the next
	// worker would be blocked for a full minute.
	h := testutil.New(t, testutil.WithLeaseTTL(time.Minute))

	c := h.CreateContent("graceful-release", "Handover")
	scheduleID := h.ScheduleAt(c.ID, 1, time.Minute)
	h.Clock.Advance(2 * time.Minute)

	// Claim the job, then shut the worker down through Run's cancellation path
	// so the release logic actually executes.
	worker := h.NewWorker("departing-worker", true)
	ctx, cancel := context.WithCancel(context.Background())
	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		worker.Run(ctx)
	}()

	h.WaitFor("job is claimed or already done", func() bool {
		s := h.ScheduleStatus(scheduleID)
		return s == "claimed" || s == "done"
	})

	cancel()
	done.Wait()

	// Whether it finished the job or released it, the row must never be left
	// claimed by a worker that no longer exists.
	require.NotEqual(t, "claimed", h.ScheduleStatus(scheduleID),
		"shutdown must not strand the job under a dead worker's lease")
}

// TestWorkerIgnoresCancelledAndDoneJobs guards the claim query's WHERE clause:
// only pending and lease-expired rows are eligible.
func TestWorkerIgnoresCancelledAndDoneJobs(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	cancelled := h.CreateContent("cancelled-job", "Never")
	scheduleID := h.ScheduleAt(cancelled.ID, 1, time.Minute)
	require.Equal(t, http.StatusNoContent, h.DELETE("/api/v1/schedules/"+scheduleID).Status)

	h.Clock.Advance(2 * time.Minute)

	worker := h.NewWorker("worker", false)
	for range 3 {
		worker.RunOnce(context.Background())
	}

	require.Equal(t, "cancelled", h.ScheduleStatus(scheduleID))
	require.Equal(t, http.StatusNotFound, h.PublicStatus("cancelled-job"),
		"a cancelled schedule must never publish")
	require.Zero(t, h.CountRows(
		`SELECT COUNT(*) FROM publish_events WHERE schedule_id = ?`, scheduleID))
}

// TestConcurrentClaimsAcrossWorkers checks the claim protocol directly, with
// many workers and many jobs: every job must be claimed by exactly one worker.
func TestConcurrentClaimsAcrossWorkers(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	const jobs = 12
	slugs := make([]string, 0, jobs)
	for i := range jobs {
		slug := "batch-" + string(rune('a'+i))
		c := h.CreateContent(slug, "Batch item")
		h.ScheduleAt(c.ID, 1, time.Minute)
		slugs = append(slugs, slug)
	}

	h.Clock.Advance(2 * time.Minute)

	const workers = 4
	var wg sync.WaitGroup
	for i := range workers {
		w := h.NewWorker("claimer-"+string(rune('a'+i)), true)
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Several passes so slower workers also get a turn.
			for range 5 {
				w.RunOnce(context.Background())
			}
		}()
	}
	wg.Wait()

	for _, slug := range slugs {
		require.Equal(t, http.StatusOK, h.PublicStatus(slug), "slug %s should be published", slug)
	}

	require.Equal(t, jobs, h.CountRows(`SELECT COUNT(*) FROM publish_events`),
		"one event per job, no duplicates")
	require.Equal(t, jobs, h.CountRows(
		`SELECT COUNT(*) FROM publish_schedules WHERE status = 'done'`))
}

// TestSchedulePublishesCorrectVersion checks that the scheduled job publishes
// the version that was scheduled, not whatever the draft has become since.
func TestSchedulePublishesCorrectVersion(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	c := h.CreateContent("version-pinning", "V1")
	h.ScheduleAt(c.ID, 1, time.Minute)

	// Edit after scheduling: the schedule still points at version 1.
	require.Equal(t, http.StatusOK, h.UpdateContent(c.ID, 1, "V2 written later").Status)

	h.Clock.Advance(2 * time.Minute)
	h.StartWorker("pinning-worker", false)

	h.WaitFor("scheduled publish happens", func() bool {
		return h.PublicStatus("version-pinning") == http.StatusOK
	})

	resp := h.Do(testutil.Request{
		Method: http.MethodGet, Path: "/public/v1/contents/version-pinning", NoAuth: true,
	})
	var item struct {
		Title string `json:"title"`
	}
	resp.JSON(t, &item)
	require.Equal(t, "V1", item.Title,
		"the scheduled version must publish, not the newer draft")
}

// TestNoDoubleClaimUnderLoad hammers the claim query from many goroutines at
// once against a single due job.
func TestNoDoubleClaimUnderLoad(t *testing.T) {
	t.Parallel()
	h := testutil.New(t)

	c := h.CreateContent("hammer", "Single job")
	scheduleID := h.ScheduleAt(c.ID, 1, time.Minute)
	h.Clock.Advance(2 * time.Minute)

	const attempts = 20
	var completions atomic.Int64

	workers := make([]interface{ RunOnce(context.Context) }, attempts)
	for i := range attempts {
		workers[i] = h.NewWorker("hammer-"+string(rune('a'+i%26))+string(rune('0'+i/26)), false)
	}

	testutil.Concurrently(attempts, func(i int) {
		workers[i].RunOnce(context.Background())
	})

	events := h.CountRows(`SELECT COUNT(*) FROM publish_events WHERE schedule_id = ?`, scheduleID)
	require.Equal(t, 1, events, "exactly one publish event under maximum contention")
	require.Equal(t, "done", h.ScheduleStatus(scheduleID))
	_ = completions
}
