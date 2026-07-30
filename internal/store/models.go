package store

import "errors"

// Sentinel errors the service layer maps onto HTTP status codes. Callers use
// errors.Is rather than inspecting driver errors directly.
var (
	// ErrNotFound means no row matched the identifier.
	ErrNotFound = errors.New("not found")
	// ErrVersionConflict means the caller's If-Match token was stale: someone
	// else wrote first.
	ErrVersionConflict = errors.New("version conflict")
	// ErrSlugTaken means another content row already uses that slug.
	ErrSlugTaken = errors.New("slug already in use")
	// ErrScheduleExists means the content already has a pending or claimed
	// schedule. Enforced by ux_schedule_active_per_content.
	ErrScheduleExists = errors.New("content already has an active schedule")
	// ErrNotCancellable means the schedule was already claimed, done, or
	// cancelled by the time the cancel arrived.
	ErrNotCancellable = errors.New("schedule is no longer cancellable")
)

// Content is a row from contents: the identity, the optimistic-lock token, and
// the pointer to whichever version is public.
type Content struct {
	ID                 string
	Slug               string
	CurrentVersion     int64
	PublishedVersionID *int64
	PublishedAt        *int64 // unix millis UTC
	CreatedAt          int64
	UpdatedAt          int64
}

// IsPublished reports whether a version pointer is currently set.
func (c Content) IsPublished() bool { return c.PublishedVersionID != nil }

// Version is an immutable row from content_versions. Editing appends one of
// these; nothing ever updates one in place.
type Version struct {
	ID        int64
	ContentID string
	Version   int64
	CreatedBy string
	CreatedAt int64
	Fields    VersionFields
}

// VersionFields is the mutable payload a caller supplies when creating or
// editing content. Splitting it out keeps the write path from having to name
// every column twice.
type VersionFields struct {
	Title           string
	Body            string
	MetaTitle       string
	MetaDescription string
	CanonicalURL    string
	OGImageURL      string
	NoIndex         bool
}

// ContentWithVersion pairs a content row with one of its versions, which is
// what almost every read actually wants.
type ContentWithVersion struct {
	Content Content
	Version Version
}

// Schedule is a row from publish_schedules.
type Schedule struct {
	ID            string
	ContentID     string
	VersionID     int64
	RunAt         int64 // unix millis UTC
	Status        string
	Attempts      int64
	LockedBy      *string
	LockExpiresAt *int64
	LastError     string
	CreatedAt     int64
	UpdatedAt     int64
}

// Schedule status values.
const (
	SchedulePending   = "pending"
	ScheduleClaimed   = "claimed"
	ScheduleDone      = "done"
	ScheduleCancelled = "cancelled"
	ScheduleFailed    = "failed"
)

// Publish event actions and sources.
const (
	ActionPublish   = "publish"
	ActionUnpublish = "unpublish"

	SourceManual    = "manual"
	SourceScheduled = "scheduled"
)

// Event is a row from publish_events: the audit trail, and the table whose
// unique index on schedule_id makes a recorded double-publish impossible.
type Event struct {
	ID         int64
	ContentID  string
	VersionID  *int64
	Action     string
	Source     string
	ScheduleID *string
	Actor      string
	OccurredAt int64
}
