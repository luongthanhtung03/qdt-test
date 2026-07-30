-- +goose Up

-- Content is stored as an immutable append-only version history plus a pointer
-- to whichever version is currently public. Editing never mutates a row: it
-- appends to content_versions and bumps contents.current_version.
--
-- The consequence that matters: the public API reads only through
-- published_version_id, so there is no "status" column that a query could
-- forget to filter on. Unpublished content is unreachable by construction.
CREATE TABLE contents (
  id                   TEXT    PRIMARY KEY,              -- uuid v4
  slug                 TEXT    NOT NULL UNIQUE,          -- SEO URL key
  current_version      INTEGER NOT NULL,                 -- optimistic-lock token, exposed as ETag
  published_version_id INTEGER NULL REFERENCES content_versions(id),
  published_at         INTEGER NULL,                     -- unix millis UTC
  created_at           INTEGER NOT NULL,
  updated_at           INTEGER NOT NULL
);

CREATE TABLE content_versions (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  content_id       TEXT    NOT NULL REFERENCES contents(id) ON DELETE CASCADE,
  version          INTEGER NOT NULL,
  title            TEXT    NOT NULL,
  body             TEXT    NOT NULL,
  meta_title       TEXT    NOT NULL DEFAULT '',
  meta_description TEXT    NOT NULL DEFAULT '',
  canonical_url    TEXT    NOT NULL DEFAULT '',
  og_image_url     TEXT    NOT NULL DEFAULT '',
  noindex          INTEGER NOT NULL DEFAULT 0,
  created_by       TEXT    NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL,
  -- Second line of defense against lost updates: even if the compare-and-swap
  -- on contents.current_version were wrong, two writers could not both land
  -- version N.
  UNIQUE (content_id, version)
);

CREATE INDEX idx_versions_content ON content_versions(content_id, version DESC);

CREATE TABLE publish_schedules (
  id              TEXT    PRIMARY KEY,
  content_id      TEXT    NOT NULL REFERENCES contents(id) ON DELETE CASCADE,
  version_id      INTEGER NOT NULL REFERENCES content_versions(id) ON DELETE CASCADE,
  run_at          INTEGER NOT NULL,
  status          TEXT    NOT NULL
                  CHECK (status IN ('pending','claimed','done','cancelled','failed')),
  attempts        INTEGER NOT NULL DEFAULT 0,
  locked_by       TEXT    NULL,          -- worker instance id holding the lease
  lock_expires_at INTEGER NULL,          -- lease deadline; enables crash recovery
  last_error      TEXT    NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);

-- Makes "at most one active schedule per content" a database invariant rather
-- than an application convention. Concurrent schedule requests race safely:
-- the loser takes a UNIQUE violation, which the API maps to 409.
CREATE UNIQUE INDEX ux_schedule_active_per_content
  ON publish_schedules(content_id) WHERE status IN ('pending','claimed');

CREATE INDEX idx_schedule_due ON publish_schedules(status, run_at);

CREATE TABLE publish_events (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  content_id  TEXT    NOT NULL REFERENCES contents(id) ON DELETE CASCADE,
  version_id  INTEGER NULL REFERENCES content_versions(id),
  action      TEXT    NOT NULL CHECK (action IN ('publish','unpublish')),
  source      TEXT    NOT NULL CHECK (source IN ('manual','scheduled')),
  schedule_id TEXT    NULL,
  actor       TEXT    NOT NULL DEFAULT '',
  occurred_at INTEGER NOT NULL
);

-- Makes a double-publish impossible to record, and gives the exactly-once test
-- a precise assertion target: COUNT(*) == 1.
CREATE UNIQUE INDEX ux_event_per_schedule
  ON publish_events(schedule_id) WHERE schedule_id IS NOT NULL;

CREATE INDEX idx_events_content ON publish_events(content_id, occurred_at DESC);

-- +goose Down
DROP INDEX idx_events_content;
DROP INDEX ux_event_per_schedule;
DROP TABLE publish_events;
DROP INDEX idx_schedule_due;
DROP INDEX ux_schedule_active_per_content;
DROP TABLE publish_schedules;
DROP INDEX idx_versions_content;
DROP TABLE content_versions;
DROP TABLE contents;
