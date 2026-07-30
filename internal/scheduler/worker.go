// Package scheduler runs scheduled publishes.
//
// The design is a polling worker with a lease, not an in-memory timer. Timers
// die with the process; a row in publish_schedules survives a restart, which is
// the actual requirement. See docs/DESIGN.md section 6.
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/luongthanhtung03/qdt-test/internal/clock"
	"github.com/luongthanhtung03/qdt-test/internal/store"
)

// Worker claims due schedules and executes them.
type Worker struct {
	db    *store.DB
	clk   clock.Clock
	log   *slog.Logger
	id    string
	poll  time.Duration
	lease time.Duration
	batch int

	// running guards the release-on-shutdown step: shutdown must not run
	// while a job transaction is mid-flight.
	running sync.WaitGroup

	// ticks counts completed poll cycles, letting tests wait for the worker to
	// have actually looked rather than sleeping and hoping.
	ticksMu sync.Mutex
	ticks   int64
}

// Config configures a Worker.
type Config struct {
	InstanceID   string
	PollInterval time.Duration
	LeaseTTL     time.Duration
	BatchSize    int
	Logger       *slog.Logger
}

// New builds a Worker.
func New(db *store.DB, clk clock.Clock, cfg Config) *Worker {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Worker{
		db:    db,
		clk:   clk,
		log:   log.With("component", "scheduler", "worker_id", cfg.InstanceID),
		id:    cfg.InstanceID,
		poll:  cfg.PollInterval,
		lease: cfg.LeaseTTL,
		batch: cfg.BatchSize,
	}
}

// Run polls for due jobs until ctx is cancelled.
//
// The ticker is real even though time itself comes from the injected clock.
// That split is deliberate: production polls once a second, tests poll every
// few milliseconds and advance a fake clock to control which jobs are due. It
// gives deterministic due-times without a virtual scheduler.
//
// On return, any job this worker still holds is released back to pending, so a
// planned restart does not make the next worker wait out a lease nobody holds.
func (w *Worker) Run(ctx context.Context) {
	w.log.Info("scheduler started", "poll_interval", w.poll, "lease_ttl", w.lease)

	ticker := time.NewTicker(w.poll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.shutdown()
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// tick claims and runs whatever is due right now.
func (w *Worker) tick(ctx context.Context) {
	defer func() {
		w.ticksMu.Lock()
		w.ticks++
		w.ticksMu.Unlock()
	}()

	now := clock.ToMillis(w.clk.Now())
	leaseExpiry := clock.ToMillis(w.clk.Now().Add(w.lease))

	jobs, err := w.db.ClaimDueSchedules(ctx, w.id, now, leaseExpiry, w.batch)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down; not worth logging
		}
		w.log.Error("claim failed", "error", err)
		return
	}
	if len(jobs) == 0 {
		return
	}

	w.log.Info("claimed schedules", "count", len(jobs))
	for _, job := range jobs {
		w.execute(ctx, job)
	}
}

// execute performs one job.
//
// The publish effect and the job's completion commit together, so there is no
// window where content is published but the job still looks pending. If the
// lease was stolen in the meantime the whole transaction rolls back and this
// worker has no effect at all.
func (w *Worker) execute(ctx context.Context, job store.ClaimedJob) {
	w.running.Add(1)
	defer w.running.Done()

	// A cancelled context means shutdown. Use a detached context with a short
	// budget so an in-flight job can finish cleanly rather than being torn in
	// half -- the transaction would roll back safely either way, but finishing
	// avoids a pointless retry.
	execCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	now := clock.ToMillis(w.clk.Now())

	completed, err := w.db.CompleteScheduledPublish(execCtx, job, w.id, now)
	if err != nil {
		w.log.Error("scheduled publish failed",
			"schedule_id", job.ScheduleID, "content_id", job.ContentID, "error", err)

		// Release the lease so the job is retried rather than waiting for the
		// lease to expire.
		if ferr := w.db.FailSchedule(execCtx, job.ScheduleID, w.id, err.Error(), now); ferr != nil {
			w.log.Error("could not record failure", "schedule_id", job.ScheduleID, "error", ferr)
		}
		return
	}

	if !completed {
		// Another worker holds the lease now. Normal operation under
		// contention, not an error.
		w.log.Warn("lease lost before completion; another worker owns this job",
			"schedule_id", job.ScheduleID)
		return
	}

	w.log.Info("published on schedule",
		"schedule_id", job.ScheduleID, "content_id", job.ContentID, "version_id", job.VersionID)
}

// shutdown waits for in-flight jobs, then releases this worker's claims.
func (w *Worker) shutdown() {
	w.running.Wait()

	releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	released, err := w.db.ReleaseClaims(releaseCtx, w.id, clock.ToMillis(w.clk.Now()))
	if err != nil && !errors.Is(err, context.Canceled) {
		w.log.Error("could not release claims on shutdown", "error", err)
	}
	if released > 0 {
		w.log.Info("released claimed schedules for failover", "count", released)
	}
	w.log.Info("scheduler stopped")
}

// Ticks reports how many poll cycles have completed. Tests use it to wait for
// the worker to have genuinely looked at the queue, rather than sleeping for an
// arbitrary duration and hoping.
func (w *Worker) Ticks() int64 {
	w.ticksMu.Lock()
	defer w.ticksMu.Unlock()
	return w.ticks
}

// RunOnce performs a single poll cycle. Tests use it to drive the worker
// deterministically instead of racing a ticker.
func (w *Worker) RunOnce(ctx context.Context) { w.tick(ctx) }
