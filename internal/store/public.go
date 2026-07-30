package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Every query in this file joins content_versions through
// contents.published_version_id.
//
// That join is the entire access-control mechanism. There is no status column
// to filter on and therefore no filter to forget: a draft is not a row with the
// wrong status, it is a row that published_version_id does not point at. An
// INNER JOIN on a NULL pointer yields nothing, so unpublished content cannot
// appear in a public result set even if someone writes a careless query.

// PublicContent is a published document as the public API sees it.
type PublicContent struct {
	Slug        string
	Title       string
	Body        string
	Fields      VersionFields
	PublishedAt int64
	UpdatedAt   int64
}

const publicSelect = `SELECT c.slug, v.title, v.body,
		v.meta_title, v.meta_description, v.canonical_url, v.og_image_url,
		v.noindex, c.published_at, c.updated_at
	   FROM contents c
	   JOIN content_versions v ON v.id = c.published_version_id`

func scanPublic(s interface{ Scan(...any) error }) (PublicContent, error) {
	var p PublicContent
	var noindex int
	var publishedAt sql.NullInt64
	err := s.Scan(&p.Slug, &p.Title, &p.Body,
		&p.Fields.MetaTitle, &p.Fields.MetaDescription, &p.Fields.CanonicalURL,
		&p.Fields.OGImageURL, &noindex, &publishedAt, &p.UpdatedAt)
	p.Fields.NoIndex = noindex != 0
	p.Fields.Title = p.Title
	p.Fields.Body = p.Body
	p.PublishedAt = publishedAt.Int64
	return p, err
}

// GetPublicBySlug returns the published version for a slug.
//
// Returns ErrNotFound both when the slug is unknown and when the content exists
// but is not published. Those are deliberately indistinguishable: a different
// status for "exists but unpublished" would let anyone enumerate drafts.
func (db *DB) GetPublicBySlug(ctx context.Context, slug string) (PublicContent, error) {
	p, err := scanPublic(db.Read.QueryRowContext(ctx,
		publicSelect+` WHERE c.slug = ?`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return PublicContent{}, ErrNotFound
	}
	if err != nil {
		return PublicContent{}, fmt.Errorf("read public content: %w", err)
	}
	return p, nil
}

// ListPublic returns published content, most recently published first.
func (db *DB) ListPublic(ctx context.Context, limit, offset int) ([]PublicContent, error) {
	rows, err := db.Read.QueryContext(ctx,
		publicSelect+`
		  ORDER BY c.published_at DESC, c.id DESC
		  LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list public content: %w", err)
	}
	defer rows.Close()

	var out []PublicContent
	for rows.Next() {
		p, err := scanPublic(rows)
		if err != nil {
			return nil, fmt.Errorf("scan public content: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountPublic returns the number of published documents.
func (db *DB) CountPublic(ctx context.Context) (int, error) {
	var n int
	err := db.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM contents c
		   JOIN content_versions v ON v.id = c.published_version_id`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count public content: %w", err)
	}
	return n, nil
}

// SitemapEntry is one URL in the sitemap.
type SitemapEntry struct {
	Slug         string
	LastModified int64
}

// ListSitemapEntries returns published content eligible for the sitemap.
//
// Rows marked noindex are excluded: listing a URL in a sitemap while telling
// crawlers not to index it is a contradictory signal that search engines report
// as an error.
func (db *DB) ListSitemapEntries(ctx context.Context, limit int) ([]SitemapEntry, error) {
	rows, err := db.Read.QueryContext(ctx,
		`SELECT c.slug, COALESCE(c.published_at, c.updated_at)
		   FROM contents c
		   JOIN content_versions v ON v.id = c.published_version_id
		  WHERE v.noindex = 0
		  ORDER BY c.published_at DESC, c.id DESC
		  LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list sitemap entries: %w", err)
	}
	defer rows.Close()

	var out []SitemapEntry
	for rows.Next() {
		var e SitemapEntry
		if err := rows.Scan(&e.Slug, &e.LastModified); err != nil {
			return nil, fmt.Errorf("scan sitemap entry: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LatestPublishedAt returns the newest publish timestamp, used as the
// sitemap's collection-level validator for ETag and Last-Modified.
func (db *DB) LatestPublishedAt(ctx context.Context) (int64, error) {
	var ts sql.NullInt64
	err := db.Read.QueryRowContext(ctx,
		`SELECT MAX(c.updated_at)
		   FROM contents c
		   JOIN content_versions v ON v.id = c.published_version_id`).Scan(&ts)
	if err != nil {
		return 0, fmt.Errorf("read latest publish time: %w", err)
	}
	return ts.Int64, nil
}
