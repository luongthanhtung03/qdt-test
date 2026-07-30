package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const scheduleColumns = `id, content_id, version_id, run_at, status, attempts,
	locked_by, lock_expires_at, last_error, created_at, updated_at`

func scanSchedule(s interface{ Scan(...any) error }) (Schedule, error) {
	var sc Schedule
	err := s.Scan(&sc.ID, &sc.ContentID, &sc.VersionID, &sc.RunAt, &sc.Status,
		&sc.Attempts, &sc.LockedBy, &sc.LockExpiresAt, &sc.LastError,
		&sc.CreatedAt, &sc.UpdatedAt)
	return sc, err
}

// CreateSchedule records a future publish.
//
// The "one active schedule per content" rule is enforced by
// ux_schedule_active_per_content rather than by checking first and inserting
// second. Two concurrent requests therefore race safely: the loser takes a
// UNIQUE violation, which becomes ErrScheduleExists and then a 409.
func (db *DB) CreateSchedule(ctx context.Context, id, contentID string, versionID, runAt, now int64) (Schedule, error) {
	var out Schedule

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		// Verify the version exists and belongs to this content before
		// scheduling work against it.
		var ownerID string
		err := tx.QueryRowContext(ctx,
			`SELECT content_id FROM content_versions WHERE id = ?`, versionID).Scan(&ownerID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && ownerID != contentID) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read version owner: %w", err)
		}

		_, err = tx.ExecContext(ctx,
			`INSERT INTO publish_schedules
				(id, content_id, version_id, run_at, status, attempts,
				 locked_by, lock_expires_at, last_error, created_at, updated_at)
			 VALUES (?, ?, ?, ?, 'pending', 0, NULL, NULL, '', ?, ?)`,
			id, contentID, versionID, runAt, now, now)
		if err != nil {
			if IsUniqueViolation(err) {
				return ErrScheduleExists
			}
			return fmt.Errorf("insert schedule: %w", err)
		}

		out = Schedule{
			ID: id, ContentID: contentID, VersionID: versionID,
			RunAt: runAt, Status: SchedulePending,
			CreatedAt: now, UpdatedAt: now,
		}
		return nil
	})
	if err != nil {
		return Schedule{}, err
	}
	return out, nil
}

// GetSchedule returns one schedule by id.
func (db *DB) GetSchedule(ctx context.Context, id string) (Schedule, error) {
	sc, err := scanSchedule(db.Read.QueryRowContext(ctx,
		`SELECT `+scheduleColumns+` FROM publish_schedules WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Schedule{}, ErrNotFound
	}
	if err != nil {
		return Schedule{}, fmt.Errorf("read schedule: %w", err)
	}
	return sc, nil
}

// ListSchedules returns a content's schedules, newest first.
func (db *DB) ListSchedules(ctx context.Context, contentID string) ([]Schedule, error) {
	rows, err := db.Read.QueryContext(ctx,
		`SELECT `+scheduleColumns+`
		   FROM publish_schedules
		  WHERE content_id = ?
		  ORDER BY created_at DESC, id DESC`, contentID)
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	defer rows.Close()

	var out []Schedule
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, fmt.Errorf("scan schedule: %w", err)
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// CancelSchedule cancels a schedule that is still pending.
//
// The status check is in the WHERE clause, so cancel and claim cannot both
// succeed. If a worker claimed the job first, zero rows match and the caller
// gets ErrNotCancellable, which the API reports as 409. Cancellation is
// best-effort once run_at passes, but the outcome is never ambiguous: either
// the content was published or it was cancelled, never both.
func (db *DB) CancelSchedule(ctx context.Context, id string, now int64) error {
	return db.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE publish_schedules
			    SET status = 'cancelled', locked_by = NULL, lock_expires_at = NULL, updated_at = ?
			  WHERE id = ? AND status = 'pending'`, now, id)
		if err != nil {
			return fmt.Errorf("cancel schedule: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("read affected rows: %w", err)
		}
		if affected == 1 {
			return nil
		}

		// Distinguish "no such schedule" from "not cancellable any more".
		var status string
		err = tx.QueryRowContext(ctx,
			`SELECT status FROM publish_schedules WHERE id = ?`, id).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read schedule status: %w", err)
		}
		return ErrNotCancellable
	})
}

// ClaimedJob is a schedule a worker has taken a lease on.
type ClaimedJob struct {
	ScheduleID string
	ContentID  string
	VersionID  int64
}

// ClaimDueSchedules atomically leases up to limit due jobs to workerID.
//
// The single UPDATE is the whole concurrency story. It runs on the one-writer
// pool, so it is atomic with respect to every other worker on the host: two
// workers cannot claim the same row, because the second one's UPDATE no longer
// matches it.
//
// The second WHERE branch reclaims jobs whose lease has expired, which is how a
// crashed worker's work gets picked up without any external supervisor.
//
// LIMIT sits inside the subquery because UPDATE ... LIMIT requires a
// non-default SQLite compile option.
func (db *DB) ClaimDueSchedules(ctx context.Context, workerID string, now, leaseExpiry int64, limit int) ([]ClaimedJob, error) {
	var jobs []ClaimedJob

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`UPDATE publish_schedules
			    SET status = 'claimed',
			        locked_by = ?,
			        lock_expires_at = ?,
			        attempts = attempts + 1,
			        updated_at = ?
			  WHERE id IN (
			        SELECT id FROM publish_schedules
			         WHERE (status = 'pending' AND run_at <= ?)
			            OR (status = 'claimed' AND lock_expires_at <= ?)
			         ORDER BY run_at
			         LIMIT ?
			  )
			  RETURNING id, content_id, version_id`,
			workerID, leaseExpiry, now, now, now, limit)
		if err != nil {
			return fmt.Errorf("claim schedules: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var j ClaimedJob
			if err := rows.Scan(&j.ScheduleID, &j.ContentID, &j.VersionID); err != nil {
				return fmt.Errorf("scan claimed job: %w", err)
			}
			jobs = append(jobs, j)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return jobs, nil
}

// CompleteScheduledPublish performs the publish effect and closes out the job
// in a single transaction. This is the exactly-once mechanism.
//
// Because the pointer move, the audit event, and the status change all commit
// together, there is no window in which content is published but the job still
// looks pending, or the reverse.
//
// The final UPDATE requires status='claimed' AND locked_by=workerID. A worker
// whose lease expired and was stolen therefore matches zero rows and rolls the
// whole transaction back, publishing nothing. Combined with the unique index on
// publish_events(schedule_id), a duplicate publish is impossible to record.
//
// Returns true when this worker completed the job.
func (db *DB) CompleteScheduledPublish(ctx context.Context, job ClaimedJob, workerID string, now int64) (bool, error) {
	completed := false

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		// Move the published pointer.
		if _, err := tx.ExecContext(ctx,
			`UPDATE contents
			    SET published_version_id = ?, published_at = ?, updated_at = ?
			  WHERE id = ?`,
			job.VersionID, now, now, job.ContentID); err != nil {
			return fmt.Errorf("set published version: %w", err)
		}

		// Record the audit event. The unique index on schedule_id means a
		// second attempt for this schedule cannot land.
		scheduleID := job.ScheduleID
		if err := insertEventTx(ctx, tx, Event{
			ContentID: job.ContentID, VersionID: &job.VersionID,
			Action: ActionPublish, Source: SourceScheduled,
			ScheduleID: &scheduleID, OccurredAt: now,
		}); err != nil {
			return err
		}

		// Close the job out, but only if we still hold the lease.
		res, err := tx.ExecContext(ctx,
			`UPDATE publish_schedules
			    SET status = 'done', locked_by = NULL, lock_expires_at = NULL, updated_at = ?
			  WHERE id = ? AND status = 'claimed' AND locked_by = ?`,
			now, job.ScheduleID, workerID)
		if err != nil {
			return fmt.Errorf("mark schedule done: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("read affected rows: %w", err)
		}
		if affected == 0 {
			// The lease was lost to another worker. Roll back so this worker
			// has no effect at all.
			return errLeaseLost
		}

		completed = true
		return nil
	})

	if errors.Is(err, errLeaseLost) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return completed, nil
}

// errLeaseLost is internal: it aborts the job transaction without surfacing as
// a caller-visible failure, because losing a lease is normal operation rather
// than an error.
var errLeaseLost = errors.New("lease lost to another worker")

// FailSchedule records a failed attempt and releases the lease so the job can
// be retried.
func (db *DB) FailSchedule(ctx context.Context, scheduleID, workerID, reason string, now int64) error {
	return db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE publish_schedules
			    SET status = 'pending', locked_by = NULL, lock_expires_at = NULL,
			        last_error = ?, updated_at = ?
			  WHERE id = ? AND status = 'claimed' AND locked_by = ?`,
			reason, now, scheduleID, workerID)
		if err != nil {
			return fmt.Errorf("record schedule failure: %w", err)
		}
		return nil
	})
}

// ReleaseClaims returns this worker's still-claimed jobs to pending.
//
// Called during graceful shutdown so a planned restart does not force the next
// worker to wait out a lease that nobody is holding any more.
func (db *DB) ReleaseClaims(ctx context.Context, workerID string, now int64) (int64, error) {
	var released int64

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE publish_schedules
			    SET status = 'pending', locked_by = NULL, lock_expires_at = NULL, updated_at = ?
			  WHERE status = 'claimed' AND locked_by = ?`, now, workerID)
		if err != nil {
			return fmt.Errorf("release claims: %w", err)
		}
		released, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("read affected rows: %w", err)
		}
		return nil
	})
	return released, err
}
