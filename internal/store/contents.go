package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// versionColumns is the shared SELECT list for content_versions, kept in one
// place so the scan order can never drift between queries.
const versionColumns = `v.id, v.content_id, v.version, v.title, v.body,
	v.meta_title, v.meta_description, v.canonical_url, v.og_image_url,
	v.noindex, v.created_by, v.created_at`

const contentColumns = `c.id, c.slug, c.current_version, c.published_version_id,
	c.published_at, c.created_at, c.updated_at`

func scanVersion(s interface{ Scan(...any) error }) (Version, error) {
	var v Version
	var noindex int
	err := s.Scan(&v.ID, &v.ContentID, &v.Version, &v.Fields.Title, &v.Fields.Body,
		&v.Fields.MetaTitle, &v.Fields.MetaDescription, &v.Fields.CanonicalURL,
		&v.Fields.OGImageURL, &noindex, &v.CreatedBy, &v.CreatedAt)
	v.Fields.NoIndex = noindex != 0
	return v, err
}

func scanContent(s interface{ Scan(...any) error }) (Content, error) {
	var c Content
	err := s.Scan(&c.ID, &c.Slug, &c.CurrentVersion, &c.PublishedVersionID,
		&c.PublishedAt, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

// CreateContent inserts a content row and its first version in one
// transaction. The two must land together: a content row whose
// current_version points at no version would break every later read.
func (db *DB) CreateContent(ctx context.Context, id, slug string, f VersionFields, actor string, now int64) (ContentWithVersion, error) {
	var out ContentWithVersion

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO contents (id, slug, current_version, published_version_id,
				published_at, created_at, updated_at)
			 VALUES (?, ?, 1, NULL, NULL, ?, ?)`,
			id, slug, now, now)
		if err != nil {
			if IsUniqueViolation(err) {
				return ErrSlugTaken
			}
			return fmt.Errorf("insert content: %w", err)
		}

		versionID, err := insertVersion(ctx, tx, id, 1, f, actor, now)
		if err != nil {
			return err
		}

		out = ContentWithVersion{
			Content: Content{
				ID: id, Slug: slug, CurrentVersion: 1,
				CreatedAt: now, UpdatedAt: now,
			},
			Version: Version{
				ID: versionID, ContentID: id, Version: 1,
				CreatedBy: actor, CreatedAt: now, Fields: f,
			},
		}
		return nil
	})
	if err != nil {
		return ContentWithVersion{}, err
	}
	return out, nil
}

// insertVersion appends an immutable version row and returns its rowid.
func insertVersion(ctx context.Context, tx *sql.Tx, contentID string, version int64, f VersionFields, actor string, now int64) (int64, error) {
	noindex := 0
	if f.NoIndex {
		noindex = 1
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO content_versions (content_id, version, title, body,
			meta_title, meta_description, canonical_url, og_image_url,
			noindex, created_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		contentID, version, f.Title, f.Body, f.MetaTitle, f.MetaDescription,
		f.CanonicalURL, f.OGImageURL, noindex, actor, now)
	if err != nil {
		return 0, fmt.Errorf("insert version: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read version id: %w", err)
	}
	return id, nil
}

// UpdateContent appends a new version, but only if expectedVersion still
// matches contents.current_version.
//
// This is the lost-update defense. The compare-and-swap lives in the UPDATE's
// WHERE clause, so two concurrent editors holding the same If-Match token
// cannot both succeed: SQLite serialises the two statements, the first moves
// current_version, and the second matches zero rows.
//
// Returns ErrVersionConflict when the token is stale and ErrNotFound when the
// content does not exist. Distinguishing the two costs one extra SELECT, but
// only on the failure path.
func (db *DB) UpdateContent(ctx context.Context, id string, expectedVersion int64, f VersionFields, actor string, now int64) (ContentWithVersion, error) {
	var out ContentWithVersion

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE contents
			    SET current_version = current_version + 1, updated_at = ?
			  WHERE id = ? AND current_version = ?`,
			now, id, expectedVersion)
		if err != nil {
			return fmt.Errorf("bump content version: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("read affected rows: %w", err)
		}

		if affected == 0 {
			// Either the row is gone or the token is stale. One SELECT tells
			// us which, so the caller gets 404 rather than a misleading 412.
			var exists int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM contents WHERE id = ?`, id).Scan(&exists); err != nil {
				return fmt.Errorf("check content exists: %w", err)
			}
			if exists == 0 {
				return ErrNotFound
			}
			return ErrVersionConflict
		}

		newVersion := expectedVersion + 1
		versionID, err := insertVersion(ctx, tx, id, newVersion, f, actor, now)
		if err != nil {
			return err
		}

		// Re-read so the response reflects committed state rather than a
		// value assembled in memory.
		c, err := getContentTx(ctx, tx, id)
		if err != nil {
			return err
		}
		out = ContentWithVersion{
			Content: c,
			Version: Version{
				ID: versionID, ContentID: id, Version: newVersion,
				CreatedBy: actor, CreatedAt: now, Fields: f,
			},
		}
		return nil
	})
	if err != nil {
		return ContentWithVersion{}, err
	}
	return out, nil
}

func getContentTx(ctx context.Context, tx *sql.Tx, id string) (Content, error) {
	c, err := scanContent(tx.QueryRowContext(ctx,
		`SELECT `+contentColumns+` FROM contents c WHERE c.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Content{}, ErrNotFound
	}
	if err != nil {
		return Content{}, fmt.Errorf("read content: %w", err)
	}
	return c, nil
}

// GetContent returns a content row by id.
func (db *DB) GetContent(ctx context.Context, id string) (Content, error) {
	c, err := scanContent(db.Read.QueryRowContext(ctx,
		`SELECT `+contentColumns+` FROM contents c WHERE c.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Content{}, ErrNotFound
	}
	if err != nil {
		return Content{}, fmt.Errorf("read content: %w", err)
	}
	return c, nil
}

// GetContentWithCurrentVersion returns a content row alongside its latest
// (draft) version -- the admin view.
func (db *DB) GetContentWithCurrentVersion(ctx context.Context, id string) (ContentWithVersion, error) {
	row := db.Read.QueryRowContext(ctx,
		`SELECT `+contentColumns+`, `+versionColumns+`
		   FROM contents c
		   JOIN content_versions v
		     ON v.content_id = c.id AND v.version = c.current_version
		  WHERE c.id = ?`, id)

	cwv, err := scanContentWithVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ContentWithVersion{}, ErrNotFound
	}
	if err != nil {
		return ContentWithVersion{}, fmt.Errorf("read content with version: %w", err)
	}
	return cwv, nil
}

func scanContentWithVersion(s interface{ Scan(...any) error }) (ContentWithVersion, error) {
	var c Content
	var v Version
	var noindex int
	err := s.Scan(
		&c.ID, &c.Slug, &c.CurrentVersion, &c.PublishedVersionID,
		&c.PublishedAt, &c.CreatedAt, &c.UpdatedAt,
		&v.ID, &v.ContentID, &v.Version, &v.Fields.Title, &v.Fields.Body,
		&v.Fields.MetaTitle, &v.Fields.MetaDescription, &v.Fields.CanonicalURL,
		&v.Fields.OGImageURL, &noindex, &v.CreatedBy, &v.CreatedAt,
	)
	v.Fields.NoIndex = noindex != 0
	return ContentWithVersion{Content: c, Version: v}, err
}

// ListContents returns content rows with their current versions, newest first.
func (db *DB) ListContents(ctx context.Context, limit, offset int) ([]ContentWithVersion, error) {
	rows, err := db.Read.QueryContext(ctx,
		`SELECT `+contentColumns+`, `+versionColumns+`
		   FROM contents c
		   JOIN content_versions v
		     ON v.content_id = c.id AND v.version = c.current_version
		  ORDER BY c.created_at DESC, c.id DESC
		  LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list contents: %w", err)
	}
	defer rows.Close()

	var out []ContentWithVersion
	for rows.Next() {
		cwv, err := scanContentWithVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("scan content: %w", err)
		}
		out = append(out, cwv)
	}
	return out, rows.Err()
}

// CountContents returns the total number of content rows, for list pagination.
func (db *DB) CountContents(ctx context.Context) (int, error) {
	var n int
	if err := db.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM contents`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count contents: %w", err)
	}
	return n, nil
}

// ListVersions returns a content's version history, newest first.
func (db *DB) ListVersions(ctx context.Context, contentID string, limit, offset int) ([]Version, error) {
	// Distinguish "no versions" from "no such content": an empty history is
	// impossible for a real row, so an empty result means the id is unknown.
	if _, err := db.GetContent(ctx, contentID); err != nil {
		return nil, err
	}

	rows, err := db.Read.QueryContext(ctx,
		`SELECT `+versionColumns+`
		   FROM content_versions v
		  WHERE v.content_id = ?
		  ORDER BY v.version DESC
		  LIMIT ? OFFSET ?`, contentID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list versions: %w", err)
	}
	defer rows.Close()

	var out []Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("scan version: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetVersion returns one version of a content by its version number.
func (db *DB) GetVersion(ctx context.Context, contentID string, version int64) (Version, error) {
	v, err := scanVersion(db.Read.QueryRowContext(ctx,
		`SELECT `+versionColumns+`
		   FROM content_versions v
		  WHERE v.content_id = ? AND v.version = ?`, contentID, version))
	if errors.Is(err, sql.ErrNoRows) {
		return Version{}, ErrNotFound
	}
	if err != nil {
		return Version{}, fmt.Errorf("read version: %w", err)
	}
	return v, nil
}

// GetVersionByID returns a version by its rowid, which is what
// contents.published_version_id stores.
func (db *DB) GetVersionByID(ctx context.Context, versionID int64) (Version, error) {
	v, err := scanVersion(db.Read.QueryRowContext(ctx,
		`SELECT `+versionColumns+` FROM content_versions v WHERE v.id = ?`, versionID))
	if errors.Is(err, sql.ErrNoRows) {
		return Version{}, ErrNotFound
	}
	if err != nil {
		return Version{}, fmt.Errorf("read version by id: %w", err)
	}
	return v, nil
}
