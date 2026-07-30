package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PublishParams describes one publish effect.
type PublishParams struct {
	ContentID  string
	VersionID  int64   // content_versions.id to point at
	Source     string  // SourceManual or SourceScheduled
	Actor      string  // audit trail
	ScheduleID *string // set when the publish came from a schedule
	Now        int64
}

// Publish points contents.published_version_id at a version and records the
// event, in one transaction.
//
// Publishing a version that is already published is a no-op: the pointer is
// unchanged and no duplicate event is written. That makes the operation
// idempotent, which is what lets at-least-once job delivery produce
// exactly-once observable behaviour.
func (db *DB) Publish(ctx context.Context, p PublishParams) (Content, error) {
	var out Content

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		current, err := getContentTx(ctx, tx, p.ContentID)
		if err != nil {
			return err
		}

		// Confirm the target version belongs to this content. Without this a
		// caller could point one document at another document's version.
		var ownerID string
		err = tx.QueryRowContext(ctx,
			`SELECT content_id FROM content_versions WHERE id = ?`, p.VersionID).Scan(&ownerID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read version owner: %w", err)
		}
		if ownerID != p.ContentID {
			return ErrNotFound
		}

		if current.PublishedVersionID != nil && *current.PublishedVersionID == p.VersionID {
			// Already pointing there. Still record the event when it came from
			// a schedule, so the schedule has its exactly-one audit row.
			if p.ScheduleID != nil {
				if err := insertEventTx(ctx, tx, Event{
					ContentID: p.ContentID, VersionID: &p.VersionID,
					Action: ActionPublish, Source: p.Source,
					ScheduleID: p.ScheduleID, Actor: p.Actor, OccurredAt: p.Now,
				}); err != nil {
					return err
				}
			}
			out = current
			return nil
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE contents
			    SET published_version_id = ?, published_at = ?, updated_at = ?
			  WHERE id = ?`,
			p.VersionID, p.Now, p.Now, p.ContentID); err != nil {
			return fmt.Errorf("set published version: %w", err)
		}

		if err := insertEventTx(ctx, tx, Event{
			ContentID: p.ContentID, VersionID: &p.VersionID,
			Action: ActionPublish, Source: p.Source,
			ScheduleID: p.ScheduleID, Actor: p.Actor, OccurredAt: p.Now,
		}); err != nil {
			return err
		}

		out, err = getContentTx(ctx, tx, p.ContentID)
		return err
	})
	if err != nil {
		return Content{}, err
	}
	return out, nil
}

// Unpublish clears the published pointer. Content itself is untouched: every
// version remains in history, only the public view goes away.
func (db *DB) Unpublish(ctx context.Context, contentID, actor string, now int64) (Content, error) {
	var out Content

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		current, err := getContentTx(ctx, tx, contentID)
		if err != nil {
			return err
		}

		if current.PublishedVersionID == nil {
			out = current // already unpublished; idempotent
			return nil
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE contents
			    SET published_version_id = NULL, published_at = NULL, updated_at = ?
			  WHERE id = ?`, now, contentID); err != nil {
			return fmt.Errorf("clear published version: %w", err)
		}

		if err := insertEventTx(ctx, tx, Event{
			ContentID: contentID, VersionID: current.PublishedVersionID,
			Action: ActionUnpublish, Source: SourceManual,
			Actor: actor, OccurredAt: now,
		}); err != nil {
			return err
		}

		out, err = getContentTx(ctx, tx, contentID)
		return err
	})
	if err != nil {
		return Content{}, err
	}
	return out, nil
}

// insertEventTx appends an audit row.
//
// A unique index covers schedule_id, so a second event for the same schedule
// fails rather than recording a double-publish. Because the caller runs inside
// the job transaction, that failure rolls the whole job back.
func insertEventTx(ctx context.Context, tx *sql.Tx, e Event) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO publish_events
			(content_id, version_id, action, source, schedule_id, actor, occurred_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.ContentID, e.VersionID, e.Action, e.Source, e.ScheduleID, e.Actor, e.OccurredAt)
	if err != nil {
		return fmt.Errorf("insert publish event: %w", err)
	}
	return nil
}

// ListEvents returns a content's publish history, newest first.
func (db *DB) ListEvents(ctx context.Context, contentID string, limit int) ([]Event, error) {
	rows, err := db.Read.QueryContext(ctx,
		`SELECT id, content_id, version_id, action, source, schedule_id, actor, occurred_at
		   FROM publish_events
		  WHERE content_id = ?
		  ORDER BY occurred_at DESC, id DESC
		  LIMIT ?`, contentID, limit)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.ContentID, &e.VersionID, &e.Action,
			&e.Source, &e.ScheduleID, &e.Actor, &e.OccurredAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
