package httpapi

import (
	"github.com/luongthanhtung03/qdt-test/internal/clock"
	"github.com/luongthanhtung03/qdt-test/internal/store"
)

// Wire types are declared separately from the store models on purpose. The
// database stores timestamps as integer millis and booleans as integers; the
// API speaks RFC3339 and JSON booleans. Keeping the two apart means a schema
// change does not silently alter the public contract.

type seoDTO struct {
	MetaTitle       string `json:"meta_title"`
	MetaDescription string `json:"meta_description"`
	CanonicalURL    string `json:"canonical_url"`
	OGImageURL      string `json:"og_image_url"`
	NoIndex         bool   `json:"noindex"`
}

type versionDTO struct {
	Version   int64  `json:"version"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	SEO       seoDTO `json:"seo"`
	CreatedBy string `json:"created_by,omitempty"`
	CreatedAt string `json:"created_at"`
}

// contentDTO is the admin view: the draft (latest) version plus publish state.
type contentDTO struct {
	ID          string     `json:"id"`
	Slug        string     `json:"slug"`
	Version     int64      `json:"version"` // also the ETag value
	Status      string     `json:"status"`  // draft | published | published_with_draft
	PublishedAt *string    `json:"published_at"`
	CreatedAt   string     `json:"created_at"`
	UpdatedAt   string     `json:"updated_at"`
	Draft       versionDTO `json:"draft"`
	// PublishedVersion is the version number currently served publicly, if any.
	PublishedVersion *int64 `json:"published_version"`
}

// publicContentDTO is what unauthenticated callers see. It deliberately omits
// the draft, the version history, and every internal identifier -- a public
// consumer has no use for them, and not serialising them means they cannot
// leak through a forgotten field.
type publicContentDTO struct {
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	SEO         seoDTO `json:"seo"`
	PublishedAt string `json:"published_at"`
	UpdatedAt   string `json:"updated_at"`
}

type listResponse[T any] struct {
	Items  []T `json:"items"`
	Total  int `json:"total"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

func toSEO(f store.VersionFields) seoDTO {
	return seoDTO{
		MetaTitle:       f.MetaTitle,
		MetaDescription: f.MetaDescription,
		CanonicalURL:    f.CanonicalURL,
		OGImageURL:      f.OGImageURL,
		NoIndex:         f.NoIndex,
	}
}

func toVersionDTO(v store.Version) versionDTO {
	return versionDTO{
		Version:   v.Version,
		Title:     v.Fields.Title,
		Body:      v.Fields.Body,
		SEO:       toSEO(v.Fields),
		CreatedBy: v.CreatedBy,
		CreatedAt: formatMillis(v.CreatedAt),
	}
}

// contentStatus derives the publish state from the version pointer rather than
// storing it. There is no status column to fall out of sync.
func contentStatus(c store.Content, publishedVersion *int64) string {
	if c.PublishedVersionID == nil {
		return "draft"
	}
	if publishedVersion != nil && *publishedVersion != c.CurrentVersion {
		return "published_with_draft"
	}
	return "published"
}

func toContentDTO(cwv store.ContentWithVersion, publishedVersion *int64) contentDTO {
	c := cwv.Content
	return contentDTO{
		ID:               c.ID,
		Slug:             c.Slug,
		Version:          c.CurrentVersion,
		Status:           contentStatus(c, publishedVersion),
		PublishedAt:      formatMillisPtr(c.PublishedAt),
		CreatedAt:        formatMillis(c.CreatedAt),
		UpdatedAt:        formatMillis(c.UpdatedAt),
		Draft:            toVersionDTO(cwv.Version),
		PublishedVersion: publishedVersion,
	}
}

func formatMillis(ms int64) string {
	return clock.FromMillis(ms).Format(rfc3339Milli)
}

func formatMillisPtr(ms *int64) *string {
	if ms == nil {
		return nil
	}
	s := formatMillis(*ms)
	return &s
}
