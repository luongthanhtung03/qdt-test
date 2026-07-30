// Package content is the domain layer: validation, identifier generation, and
// the orchestration of publish state changes.
//
// The HTTP layer above it deals only in requests and status codes; the store
// below it deals only in SQL. Anything that is a rule about content rather than
// a rule about transport or storage belongs here.
package content

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/luongthanhtung03/qdt-test/internal/clock"
	"github.com/luongthanhtung03/qdt-test/internal/store"
)

// Service carries out content operations.
type Service struct {
	db  *store.DB
	clk clock.Clock
}

// New builds a Service.
func New(db *store.DB, clk clock.Clock) *Service {
	return &Service{db: db, clk: clk}
}

// ValidationError describes a rejected input. The HTTP layer turns it into a
// 400 with the field name in the details object.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

func invalid(field, message string) error {
	return &ValidationError{Field: field, Message: message}
}

// slugPattern allows lowercase alphanumerics separated by single hyphens.
// Slugs appear in public URLs, so they are validated rather than sanitised:
// silently rewriting a caller's slug would make the URL unpredictable.
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

const (
	maxSlugLen  = 200
	maxTitleLen = 500
	maxBodyLen  = 500_000
	maxMetaLen  = 1000
	maxURLLen   = 2000
)

// CreateInput is the payload for creating content.
type CreateInput struct {
	Slug   string
	Fields store.VersionFields
	Actor  string
}

// Create validates the input and stores content at version 1.
func (s *Service) Create(ctx context.Context, in CreateInput) (store.ContentWithVersion, error) {
	slug := strings.TrimSpace(in.Slug)
	if err := validateSlug(slug); err != nil {
		return store.ContentWithVersion{}, err
	}
	fields, err := normalizeFields(in.Fields)
	if err != nil {
		return store.ContentWithVersion{}, err
	}

	now := clock.ToMillis(s.clk.Now())
	return s.db.CreateContent(ctx, uuid.NewString(), slug, fields, in.Actor, now)
}

// UpdateInput is the payload for editing content.
type UpdateInput struct {
	ID              string
	ExpectedVersion int64
	Fields          store.VersionFields
	Actor           string
}

// Update appends a new version if ExpectedVersion still matches.
//
// Returns store.ErrVersionConflict when another writer got there first and
// store.ErrNotFound when the id is unknown.
func (s *Service) Update(ctx context.Context, in UpdateInput) (store.ContentWithVersion, error) {
	fields, err := normalizeFields(in.Fields)
	if err != nil {
		return store.ContentWithVersion{}, err
	}
	now := clock.ToMillis(s.clk.Now())
	return s.db.UpdateContent(ctx, in.ID, in.ExpectedVersion, fields, in.Actor, now)
}

// Get returns content with its latest version -- the admin (draft) view.
func (s *Service) Get(ctx context.Context, id string) (store.ContentWithVersion, error) {
	return s.db.GetContentWithCurrentVersion(ctx, id)
}

// List returns content with current versions, plus the total row count.
func (s *Service) List(ctx context.Context, limit, offset int) ([]store.ContentWithVersion, int, error) {
	items, err := s.db.ListContents(ctx, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	total, err := s.db.CountContents(ctx)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// ListVersions returns a content's version history.
func (s *Service) ListVersions(ctx context.Context, contentID string, limit, offset int) ([]store.Version, error) {
	return s.db.ListVersions(ctx, contentID, limit, offset)
}

// GetVersion returns one version by number.
func (s *Service) GetVersion(ctx context.Context, contentID string, version int64) (store.Version, error) {
	return s.db.GetVersion(ctx, contentID, version)
}

// Now exposes the service clock so handlers can stamp responses consistently.
func (s *Service) Now() int64 { return clock.ToMillis(s.clk.Now()) }

func validateSlug(slug string) error {
	switch {
	case slug == "":
		return invalid("slug", "must not be empty")
	case len(slug) > maxSlugLen:
		return invalid("slug", fmt.Sprintf("must be at most %d characters", maxSlugLen))
	case !slugPattern.MatchString(slug):
		return invalid("slug", "must be lowercase alphanumerics separated by single hyphens")
	}
	return nil
}

// normalizeFields trims whitespace and enforces length limits.
func normalizeFields(f store.VersionFields) (store.VersionFields, error) {
	f.Title = strings.TrimSpace(f.Title)
	f.MetaTitle = strings.TrimSpace(f.MetaTitle)
	f.MetaDescription = strings.TrimSpace(f.MetaDescription)
	f.CanonicalURL = strings.TrimSpace(f.CanonicalURL)
	f.OGImageURL = strings.TrimSpace(f.OGImageURL)
	// Body keeps its leading and trailing whitespace: it is the document, and
	// trimming it would silently alter content.

	if f.Title == "" {
		return f, invalid("title", "must not be empty")
	}
	for _, c := range []struct {
		field string
		value string
		max   int
	}{
		{"title", f.Title, maxTitleLen},
		{"body", f.Body, maxBodyLen},
		{"meta_title", f.MetaTitle, maxMetaLen},
		{"meta_description", f.MetaDescription, maxMetaLen},
		{"canonical_url", f.CanonicalURL, maxURLLen},
		{"og_image_url", f.OGImageURL, maxURLLen},
	} {
		if utf8.RuneCountInString(c.value) > c.max {
			return f, invalid(c.field, fmt.Sprintf("must be at most %d characters", c.max))
		}
	}
	return f, nil
}

// AsValidationError extracts a *ValidationError from an error chain.
func AsValidationError(err error) (*ValidationError, bool) {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve, true
	}
	return nil, false
}
