# Design Notes — Content Management Service (Go + SQLite)

Supplementary reading. **`README.md` is the primary document** — it covers how to run the
service, the assumptions, the technical decisions, what is unfinished, and the known limitations,
and it stands alone.

This file goes a level deeper on the parts a reader might want to interrogate: why the data model
is shaped the way it is, what the schema guarantees and how, and the SQL behind the concurrency
claims. Nothing here is required to evaluate the project; it is here because "why is the write
pool capped at one connection?" deserves a longer answer than a README should carry.

---

## 1. Decisions

| Area | Decision |
|---|---|
| Scope | **Reliability-first.** All core features, depth in the risky parts, a strong concurrency test suite. |
| Scheduling | **DB polling worker with lease.** Not materialize-on-read. |
| Dependencies | chi v5, goose v3, `modernc.org/sqlite`, testify, google/uuid, stdlib `log/slog`. Handwritten SQL, no ORM, no codegen. |
| Concurrency API | **`If-Match` / `ETag`.** Stale → `412`, missing → `428`. |
| SEO | **Server-rendered page per document** at its canonical slug, plus sitemap, robots and conditional caching. Metadata is stored per version. |

chi and goose each *subtract* roughly fifty lines of chore code — subtree-scoped middleware and
migration tracking respectively — rather than hiding anything behind abstraction. All the code
that carries the design (the SQL, the transactions, the claim protocol, the tests) is
hand-written. `sqlc` was considered and rejected: it *adds* setup cost on the least interesting
part of the problem.

### Why the pure-Go SQLite driver

`modernc.org/sqlite` is a transpiled-to-Go SQLite, so it builds with `CGO_ENABLED=0` and needs no
C toolchain. That was originally forced by the development environment (no gcc available), but it
is worth keeping regardless: the service cross-compiles to a static binary for any target, and a
reviewer can `go test ./...` on any OS with nothing installed but Go.

The trade-off is real and worth naming: `mattn/go-sqlite3` wraps the canonical C library and is
the more battle-tested driver. The DSN syntax also differs — modernc uses `_pragma=name(value)`
where mattn uses `_journal_mode=`. Everything below assumes modernc.

---

## 2. The five failure modes this design targets

The feature list is the easy part. The design work lives in five places where a naive
implementation quietly breaks:

1. **Lost update** — two editors, one silently clobbers the other.
2. **Scheduled publish** must survive a restart and must not double-publish across instances.
3. **SQLite under concurrency** — `SQLITE_BUSY` storms if WAL, `busy_timeout`, and pool sizing are
   wrong. This bites hardest in exactly the concurrency tests worth writing.
4. **Public API leakage** — unpublished content must be unreachable *by construction*, not by a
   `WHERE status = 'published'` filter that someone forgets on one query.
5. **The SQLite ↔ multi-instance contradiction** — SQLite is one file on one host. Saying so
   honestly, and showing that the claim protocol ports to Postgres unchanged, is worth more than
   pretending otherwise.

### Core structural choice: immutable versions + a published pointer

This is what makes #4 true by construction.

- Editing **never mutates** a row — it appends a `content_versions` row and bumps
  `contents.current_version`.
- Publishing sets `contents.published_version_id`; unpublishing sets it to `NULL`.
- The public API reads **only** through that pointer, so there is no filter to forget. A draft is
  not "a row with the wrong status" — it is a row nothing points at.
- `contents.current_version` doubles as the optimistic-lock token, exposed as the `ETag`.

### Time storage

All timestamps are `INTEGER` **Unix milliseconds UTC**, rendered as RFC3339 in JSON.

SQLite has no datetime type. ISO-8601 `TEXT` only sorts correctly with a fixed-width format — and
Go's `time.RFC3339Nano` **trims trailing zeros**, which silently breaks the lexicographic ordering
that `WHERE run_at <= ?` depends on. Integers dodge the entire bug class.

---

## 3. Schema — `migrations/00001_init.sql`

```sql
CREATE TABLE contents (
  id                   TEXT    PRIMARY KEY,              -- uuid v4
  slug                 TEXT    NOT NULL UNIQUE,          -- SEO URL key
  current_version      INTEGER NOT NULL,                 -- optimistic-lock token / ETag
  published_version_id INTEGER NULL REFERENCES content_versions(id),
  published_at         INTEGER NULL,
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
  UNIQUE (content_id, version)          -- second line of defense against lost updates
);

CREATE TABLE publish_schedules (
  id              TEXT    PRIMARY KEY,
  content_id      TEXT    NOT NULL REFERENCES contents(id) ON DELETE CASCADE,
  version_id      INTEGER NOT NULL REFERENCES content_versions(id) ON DELETE CASCADE,
  run_at          INTEGER NOT NULL,
  status          TEXT    NOT NULL
                  CHECK (status IN ('pending','claimed','done','cancelled','failed')),
  attempts        INTEGER NOT NULL DEFAULT 0,
  locked_by       TEXT    NULL,          -- instance id
  lock_expires_at INTEGER NULL,          -- lease; enables crash recovery
  last_error      TEXT    NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);
CREATE UNIQUE INDEX ux_schedule_active_per_content
  ON publish_schedules(content_id) WHERE status IN ('pending','claimed');
CREATE INDEX idx_schedule_due ON publish_schedules(status, run_at);

CREATE TABLE publish_events (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  content_id  TEXT    NOT NULL REFERENCES contents(id) ON DELETE CASCADE,
  version_id  INTEGER NULL REFERENCES content_versions(id),
  action      TEXT    NOT NULL,     -- publish | unpublish
  source      TEXT    NOT NULL,     -- manual | scheduled
  schedule_id TEXT    NULL,
  actor       TEXT    NOT NULL DEFAULT '',
  occurred_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX ux_event_per_schedule
  ON publish_events(schedule_id) WHERE schedule_id IS NOT NULL;
```

Two indexes do load-bearing **correctness** work, not just performance:

- `ux_schedule_active_per_content` makes "at most one active schedule per content" a *database*
  invariant. Concurrent schedule requests race safely: the loser gets a UNIQUE violation → `409`.
- `ux_event_per_schedule` makes a double-publish **impossible to record**, and gives the
  exactly-once test a precise assertion target (`COUNT(*) == 1`).

`contents` and `content_versions` reference each other. SQLite resolves foreign keys at DML time
rather than at `CREATE TABLE`, so the forward reference is fine.

---

## 4. SQLite connection setup — `internal/store/db.go`

The highest-leverage fifteen lines in the project.

```go
base := "file:" + path +
    "?_pragma=journal_mode(WAL)" +      // concurrent readers alongside one writer
    "&_pragma=busy_timeout(5000)" +     // wait, don't instantly fail, on contention
    "&_pragma=foreign_keys(1)" +        // OFF by default in SQLite
    "&_pragma=synchronous(NORMAL)"      // safe under WAL, much faster than FULL

writeDB, _ := sql.Open("sqlite", base+"&_txlock=immediate")
writeDB.SetMaxOpenConns(1)              // serialize writers in Go, never in SQLite

readDB, _ := sql.Open("sqlite", base)
readDB.SetMaxOpenConns(max(4, runtime.NumCPU()))
```

Two pools over the same file:

- **The write pool is capped at one connection.** SQLite permits a single writer regardless, so
  queueing in Go's connection pool is strictly better than discovering the limit as a
  `SQLITE_BUSY` error deep inside a transaction.
- **`_txlock=immediate`** starts write transactions with `BEGIN IMMEDIATE`. Without it, a
  transaction that reads before it writes must *upgrade* its lock, and a failed upgrade is an
  instant, unrecoverable `SQLITE_BUSY` that `busy_timeout` does **not** retry. This is the classic
  Go + SQLite deadlock, and it is worth one DSN parameter to avoid entirely.

`store.Tx(ctx, fn)` wraps the write pool: begin, run, commit, and roll back on either an error or
a panic.

---

## 5. Package layout

```
cmd/server/main.go       wiring, migrate-on-boot, graceful shutdown
internal/config/         env parsing, fail-fast validation
internal/clock/          Clock interface + Fake (test time travel)
internal/store/          SQL only. db.go, tx.go, contents.go, schedules.go, publish.go, public.go
internal/content/        domain service: validation, Create/Update/Publish/Schedule/Cancel
internal/scheduler/      worker: claim loop, lease, graceful release
internal/httpapi/        router, middleware, errors, admin.go, public.go, seo.go, html.go
internal/httpapi/templates/  the rendered page
internal/testutil/       shared harness: temp database, fake clock, httptest server
migrations/              *.sql, embedded via embed.FS
```

Dependencies point one way: `httpapi → content → store → SQLite`. `store` knows nothing about
HTTP, and `httpapi` writes no SQL of its own. Interfaces exist only at the service boundary
(`clock.Clock` is the main one) — no speculative abstraction.

---

## 6. Key flows

### Optimistic update — `PUT /api/v1/contents/{id}`, requires `If-Match: "<current_version>"`

```
BEGIN IMMEDIATE
  UPDATE contents SET current_version = current_version + 1, updated_at = ?
    WHERE id = ? AND current_version = ?
  -- RowsAffected == 0 → SELECT to distinguish 404 (no row) from 412 (stale) → rollback
  INSERT INTO content_versions (content_id, version, title, body, ...) VALUES (?, ?, ...)
COMMIT
```

`200` with a new `ETag` · `404` unknown id · `412` stale, with the current version in the body ·
`428` missing `If-Match`.

### Scheduler claim (atomic, leased)

```sql
UPDATE publish_schedules
   SET status='claimed', locked_by=?, lock_expires_at=?, attempts=attempts+1, updated_at=?
 WHERE id IN (
   SELECT id FROM publish_schedules
    WHERE (status='pending' AND run_at <= ?)
       OR (status='claimed' AND lock_expires_at <= ?)   -- reclaim after a crash
    ORDER BY run_at LIMIT ?
 )
RETURNING id, content_id, version_id;
```

This runs on the single-writer pool, making it atomic across every worker on the host. The `LIMIT`
sits inside the subquery because `UPDATE ... LIMIT` requires a non-default SQLite compile option.

### Job execution — the exactly-once mechanism

The publish effect and the job completion **commit in one transaction**:

```
BEGIN IMMEDIATE
  UPDATE contents SET published_version_id=?, published_at=?, updated_at=? WHERE id=?
  INSERT INTO publish_events (..., source='scheduled', schedule_id=?)
  UPDATE publish_schedules SET status='done', locked_by=NULL, lock_expires_at=NULL, updated_at=?
    WHERE id=? AND status='claimed' AND locked_by=?     -- lost the lease? roll back, do nothing
COMMIT
```

There is no window in which content is published but the job still looks pending, or the reverse.
Delivery is at-least-once; the effect is idempotent (same pointer, same value) and
`ux_event_per_schedule` blocks a duplicate event — so the *observable* behaviour is exactly-once.

- **Crash recovery:** a dead worker leaves `lock_expires_at` in the past, and the next poll
  reclaims the job.
- **Graceful shutdown:** on SIGINT/SIGTERM, stop claiming, wait a bounded time for in-flight jobs,
  then reset still-claimed rows to `pending` so a failover does not have to wait out the lease.
- **Cancel:** `UPDATE ... SET status='cancelled' WHERE id=? AND status='pending'`. If
  `RowsAffected == 0` the worker already claimed it → `409`. Cancellation is best-effort once
  `run_at` passes, but the outcome is never ambiguous.

---

## 7. Route isolation

The full endpoint list is in the README. One structural point belongs here, though, because it is
a design decision rather than documentation:

**Admin and public are separate chi subtrees.** The bearer-token middleware is attached inside
the `/api/v1` group, so it covers everything below it and nothing outside it. Public routes are
siblings, not children. That means an admin endpoint cannot lose authentication by being moved,
and a public endpoint cannot acquire it by accident — the nesting is the policy.

The rendered page at `GET /{slug}` is a wildcard mounted at the root, registered after every
static route. chi resolves static segments before wildcards, so `/healthz`, `/robots.txt`,
`/sitemap.xml`, `/api/...` and `/public/...` all still win. That precedence is relied upon, so
it is asserted rather than assumed (`TestHTMLPage_DoesNotShadowOtherRoutes`).

---

## 8. Test plan — high-risk scenarios

`httptest` against a real SQLite database in `t.TempDir()`. Time is injected through
`clock.Clock`; tests advance a `FakeClock` and assert with `require.Eventually` instead of
sleeping.

1. `TestConcurrentUpdate_OnlyOneWins` — 20 goroutines `PUT` with the same `If-Match`: exactly one
   `200`, nineteen `412`, and `content_versions` holds exactly 2 rows.
2. `TestScheduledPublish_ExactlyOnce_MultiWorker` — 5 workers, each with its own database handle,
   on one file with one due job → `publish_events` count is exactly 1. The multi-instance
   requirement, actually tested.
3. `TestScheduledPublish_SurvivesRestart` — schedule, shut the worker down, open a fresh store and
   worker on the same file, advance the clock → the publish happens.
4. `TestLeaseRecoveryAfterCrash` — worker A claims a job then dies before its transaction; after
   the lease expires worker B reclaims it → published exactly once.
5. `TestPublicNeverLeaksUnpublished` — draft, scheduled-but-not-due, and edited-after-publish
   content is excluded from the public list, public get returns `404`, and a published-then-edited
   item still serves the **old** version.
6. `TestCancelRacesDueTime` — a concurrent `DELETE` and worker tick resolve to either
   (cancelled, not published) or (published, `DELETE` → 409). Never both, never neither.
7. `TestPublishOlderVersion` — publish v1 while v3 is the draft; the public API serves v1.
8. `TestConcurrentCreateSameSlug` — one `201`, the rest `409`.
9. `TestMigrations_UpDownUp` — migrations are reversible and re-runnable.
10. `TestPublicETagReturns304`.

Two notes on how these are written, since the technique matters as much as the coverage.

**The concurrency tests use a shared start barrier.** Goroutines launched in a plain loop are
staggered by the cost of the `go` statement itself, often enough that each finishes before the
next begins. That turns a concurrency test into a sequential one that passes for the wrong
reason — worse than no test, because it reads as coverage. `testutil.Concurrently` releases every
goroutine from one `sync.WaitGroup`.

**The scheduler tests use a real ticker and a fake clock.** Only *time* is faked, not the
scheduling machinery: production polls every second, tests poll every 20ms and advance fake time
to decide which jobs are due. Tests then wait on `require.Eventually` for a genuine tick rather
than sleeping a guessed duration. This keeps due-times deterministic without building a virtual
scheduler, and it means the code under test is the same code that runs in production.

---

## 9. Further reading

The README is the place for everything operational — see in particular:

- **Assumptions** and **Known limitations** — what is taken as given, and where the edges are.
- **SQLite and multiple instances** — the honest account of what "multi-instance" means here, and
  exactly what would change to move to Postgres.
- **Exactly-once publishing** — the three mechanisms, summarised.
- **SEO** — the rendered page, sitemap, robots, and conditional caching.
