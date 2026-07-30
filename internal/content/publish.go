package content

import (
	"context"

	"github.com/google/uuid"

	"github.com/luongthanhtung03/qdt-test/internal/clock"
	"github.com/luongthanhtung03/qdt-test/internal/store"
)

// Publish makes a version public immediately.
//
// The version is addressed by its number rather than its rowid, because the
// number is what the API exposes. Publishing an older version while a newer
// draft exists is legitimate and supported: it is how a rollback works.
func (s *Service) Publish(ctx context.Context, contentID string, version int64, actor string) (store.Content, error) {
	v, err := s.db.GetVersion(ctx, contentID, version)
	if err != nil {
		return store.Content{}, err
	}

	return s.db.Publish(ctx, store.PublishParams{
		ContentID: contentID,
		VersionID: v.ID,
		Source:    store.SourceManual,
		Actor:     actor,
		Now:       clock.ToMillis(s.clk.Now()),
	})
}

// Unpublish removes content from the public API without deleting anything.
func (s *Service) Unpublish(ctx context.Context, contentID, actor string) (store.Content, error) {
	return s.db.Unpublish(ctx, contentID, actor, clock.ToMillis(s.clk.Now()))
}

// ScheduleInput describes a future publish.
type ScheduleInput struct {
	ContentID string
	Version   int64
	RunAtMs   int64
}

// Schedule records a publish to happen at a future time.
//
// A run_at in the past is rejected rather than silently executed. Accepting it
// would make "schedule" and "publish now" the same call with different
// observability, and it usually means the caller sent the wrong timezone.
func (s *Service) Schedule(ctx context.Context, in ScheduleInput) (store.Schedule, error) {
	now := clock.ToMillis(s.clk.Now())
	if in.RunAtMs <= now {
		return store.Schedule{}, invalid("run_at",
			"must be in the future; use the publish endpoint to publish immediately")
	}

	v, err := s.db.GetVersion(ctx, in.ContentID, in.Version)
	if err != nil {
		return store.Schedule{}, err
	}

	return s.db.CreateSchedule(ctx, uuid.NewString(), in.ContentID, v.ID, in.RunAtMs, now)
}

// ListSchedules returns a content's schedules.
func (s *Service) ListSchedules(ctx context.Context, contentID string) ([]store.Schedule, error) {
	if _, err := s.db.GetContent(ctx, contentID); err != nil {
		return nil, err
	}
	return s.db.ListSchedules(ctx, contentID)
}

// CancelSchedule cancels a pending schedule.
func (s *Service) CancelSchedule(ctx context.Context, scheduleID string) error {
	return s.db.CancelSchedule(ctx, scheduleID, clock.ToMillis(s.clk.Now()))
}

// GetPublicBySlug returns the published version for a slug.
func (s *Service) GetPublicBySlug(ctx context.Context, slug string) (store.PublicContent, error) {
	return s.db.GetPublicBySlug(ctx, slug)
}

// ListPublic returns published content with the total count.
func (s *Service) ListPublic(ctx context.Context, limit, offset int) ([]store.PublicContent, int, error) {
	items, err := s.db.ListPublic(ctx, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	total, err := s.db.CountPublic(ctx)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}
