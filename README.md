# Content Management Service

A content management backend in Go and SQLite: versioned content with optimistic locking,
scheduled publishing that survives restarts and never double-publishes, a public read API that
cannot serve unpublished content, and the SEO surface a backend actually owns.

`docs/DESIGN.md` covers why it is built this way. This file covers how to run it.

---

## Quick start

Requires Go 1.26 or later. Nothing else — no C compiler, no SQLite installation, no Docker.

```bash
git clone https://github.com/luongthanhtung03/qdt-test.git
cd qdt-test
go mod download

ADMIN_API_TOKEN=dev-token go run ./cmd/server
```

Migrations apply on boot and the database file is created if missing. The server logs
`listening addr=:8080` when it is ready.

```bash
go test ./...        # the full suite
make help            # everything else
```

Configuration is read from the environment; see `.env.example` for every variable and its
default. Only `ADMIN_API_TOKEN` is required — the service refuses to start without it, because
defaulting an auth credential means shipping with a known password.

---

## The model in one paragraph

Editing never mutates a row. It appends a `content_versions` row and increments
`contents.current_version`. Publishing sets `contents.published_version_id` to point at a
version; unpublishing sets it to `NULL`. The public API reads **only** through that pointer, so
there is no `status` column to filter on and therefore no filter to forget. `current_version`
doubles as the optimistic-lock token, exposed as an `ETag`.

That one decision is what makes the two hardest requirements true by construction rather than by
discipline: unpublished content is unreachable publicly, and a stale write is detectable.

---

## API

### Admin — requires `Authorization: Bearer $ADMIN_API_TOKEN`

| Method | Path | Notes |
|---|---|---|
| `POST` | `/api/v1/contents` | → `201`, `ETag: "1"`. Duplicate slug → `409` |
| `GET` | `/api/v1/contents` | `?limit=` `&offset=` |
| `GET` | `/api/v1/contents/{id}` | Draft view plus publish state, with `ETag` |
| `PUT` | `/api/v1/contents/{id}` | **`If-Match` required** |
| `GET` | `/api/v1/contents/{id}/versions` | History, newest first |
| `GET` | `/api/v1/contents/{id}/versions/{n}` | One version |
| `POST` | `/api/v1/contents/{id}/publish` | `{"version": n}`, idempotent |
| `POST` | `/api/v1/contents/{id}/unpublish` | Idempotent |
| `POST` | `/api/v1/contents/{id}/schedules` | `{"version": n, "run_at": "<RFC3339>"}` |
| `GET` | `/api/v1/contents/{id}/schedules` | |
| `DELETE` | `/api/v1/schedules/{scheduleID}` | Cancel; `409` if already claimed |

### Public — no authentication

`GET /public/v1/contents` · `GET /public/v1/contents/{slug}` · `GET /sitemap.xml` ·
`GET /robots.txt` · `GET /healthz`

### Errors

Every failure uses one envelope with a stable machine-readable code:

```json
{"error": {"code": "version_conflict", "message": "…", "details": {"current_version": 3}}}
```

`unauthorized` `401` · `validation_error` `400` · `not_found` `404` · `slug_conflict` `409` ·
`schedule_conflict` `409` · `version_conflict` `412` · `precondition_required` `428` ·
`internal_error` `500`

---

## Walkthrough

Every response below was captured from a real run of this code.

```bash
B=http://localhost:8080
A='Authorization: Bearer dev-token'
```

**Create.** The response carries `ETag: "1"` — the token you need in order to edit it.

```bash
curl -i -X POST "$B/api/v1/contents" -H "$A" -H 'Content-Type: application/json' -d '{
  "slug": "lifecycle",
  "title": "Lifecycle V1",
  "body": "v1 body"
}'
# HTTP/1.1 201 Created
# Etag: "1"
# Location: /api/v1/contents/1a5f85a9-0186-45f6-ac96-b36d033c613c
```

**Edit, and watch the lock work.** `If-Match` is mandatory, not optional — an unconditional
`PUT` is exactly the lost update this design exists to prevent.

```bash
ID=1a5f85a9-0186-45f6-ac96-b36d033c613c

curl -X PUT "$B/api/v1/contents/$ID" -H "$A" -H 'Content-Type: application/json' \
     -H 'If-Match: "1"' -d '{"title":"Lifecycle V2","body":"v2 body"}'      # 200, ETag: "2"

curl -X PUT "$B/api/v1/contents/$ID" -H "$A" -H 'Content-Type: application/json' \
     -H 'If-Match: "1"' -d '{"title":"Clobber","body":"x"}'                  # 412
curl -X PUT "$B/api/v1/contents/$ID" -H "$A" -H 'Content-Type: application/json' \
     -d '{"title":"Clobber","body":"x"}'                                     # 428
```

The `412` body tells you what to rebase onto, so a client can retry without a second `GET`:

```json
{"error":{"code":"version_conflict","message":"The content was modified by someone else. Re-read it and retry.","details":{"current_version":2,"etag":"\"2\""}}}
```

**Nothing is public yet.**

```bash
curl -o /dev/null -w '%{http_code}\n' "$B/public/v1/contents/lifecycle"   # 404
curl "$B/public/v1/contents"
# {"items":[],"total":0,"limit":20,"offset":0}
```

**Schedule it.** A `run_at` in the past is a `400`, not a silent immediate publish.

```bash
RUN_AT=$(date -u -d '+30 seconds' +%Y-%m-%dT%H:%M:%SZ)
curl -X POST "$B/api/v1/contents/$ID/schedules" -H "$A" -H 'Content-Type: application/json' \
     -d "{\"version\":2,\"run_at\":\"$RUN_AT\"}"                            # 201
```

The worker publishes it within one poll interval. On the run captured here, with
`SCHEDULER_POLL_INTERVAL=500ms`, the publish landed 444 ms after `run_at`:

```bash
curl "$B/public/v1/contents/lifecycle"
# {"slug":"lifecycle","title":"Lifecycle V2","body":"v2 body",
#  "seo":{...},"published_at":"2026-07-30T15:45:45.444Z","updated_at":"2026-07-30T15:45:45.444Z"}
```

**Edit again. The public API does not follow the draft.**

```bash
curl -X PUT "$B/api/v1/contents/$ID" -H "$A" -H 'Content-Type: application/json' \
     -H 'If-Match: "2"' -d '{"title":"Lifecycle V3 DRAFT","body":"unreleased"}'   # 200

curl "$B/public/v1/contents/lifecycle" | grep -o '"title":"[^"]*"'
# "title":"Lifecycle V2"          <- still the published version

curl "$B/api/v1/contents/$ID" -H "$A" | grep -o '"status":"[^"]*"'
# "status":"published_with_draft"
```

**Sitemap and conditional requests.**

```bash
curl "$B/sitemap.xml"
```
```xml
<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url>
    <loc>http://localhost:8080/public/v1/contents/lifecycle</loc>
    <lastmod>2026-07-30T15:45:45Z</lastmod>
  </url>
</urlset>
```
```bash
ETAG=$(curl -sI "$B/public/v1/contents/lifecycle" | grep -i '^etag:' | tr -d '\r' | cut -d' ' -f2)
curl -o /dev/null -w '%{http_code}\n' -H "If-None-Match: $ETAG" "$B/public/v1/contents/lifecycle"  # 304
curl -o /dev/null -w '%{http_code}\n' -H 'If-None-Match: "0"'   "$B/public/v1/contents/lifecycle"  # 200
```

**Roll back** by publishing an older version. Metadata is per-version, so it rolls back too.

```bash
curl -X POST "$B/api/v1/contents/$ID/publish" -H "$A" -H 'Content-Type: application/json' \
     -d '{"version":1}'
curl "$B/public/v1/contents/lifecycle" | grep -o '"title":"[^"]*"'
# "title":"Lifecycle V1"
```

### Verifying restart survival by hand

The automated suite covers this, but it is worth seeing directly:

```bash
# 1. Schedule something ~30s out.
# 2. Ctrl+C the server while it is still pending.
# 3. Start the server again on the same DB_PATH.
# 4. The publish still fires at its run_at.
```

Captured from a real run: the server was killed at `15:46:2x`, restarted at `15:46:44`, and the
job scheduled for `15:46:46` published at `15:46:46`. The schedule lives in a table, not in a
timer, so the process holding it is irrelevant.

---

## Design decisions worth knowing about

### SQLite and multiple instances — the honest version

**SQLite is one file on one host.** This is the real constraint, so it is worth stating plainly
rather than implying a distributed system that does not exist.

What this design *does* guarantee: correctness for **multiple processes on a single host** under
WAL. That is genuinely tested — `TestScheduledPublish_ExactlyOnce_MultiWorker` runs five workers
with separate database handles against one file and asserts exactly one publish event.

What it does not: SQLite is not a network database, and WAL is explicitly unsafe over NFS. Two
machines cannot share this file.

The reason that matters less than it sounds is that **the claim protocol uses no SQLite-specific
trick**. It is an atomic conditional `UPDATE` with a lease column. Moving to Postgres means
swapping the driver and adding `FOR UPDATE SKIP LOCKED` to the claim subquery; the worker, the
lease logic, the recovery path, and every test stay as they are.

### Why the write pool is capped at one connection

Two pools are opened over the same file: a write pool with `SetMaxOpenConns(1)` and a wider read
pool. SQLite permits a single writer no matter what the client does, so queueing writers in Go's
connection pool is strictly better than discovering the limit as a `SQLITE_BUSY` error partway
through a transaction. Under WAL, readers are unaffected and run concurrently with the writer.

### Why `_txlock=immediate`

The write pool's DSN sets `_txlock=immediate`, so transactions begin with `BEGIN IMMEDIATE` and
take their write lock up front.

Without it, a transaction that reads before it writes has to *upgrade* its lock, and a failed
upgrade returns `SQLITE_BUSY` immediately — `busy_timeout` does **not** retry that case. It is
the classic Go-plus-SQLite deadlock, and it costs one DSN parameter to avoid entirely.

### Exactly-once publishing

Delivery is at-least-once; the effect is idempotent; observable behaviour is exactly-once. Three
mechanisms combine:

1. **The claim is atomic.** One conditional `UPDATE` on the single-writer pool, so two workers
   cannot take the same row.
2. **Completion is conditional on still holding the lease.** The publish, the audit event, and
   the status change commit in one transaction whose final `UPDATE` requires
   `status='claimed' AND locked_by=?`. A worker whose lease was stolen matches zero rows and
   rolls the whole thing back, having done nothing.
3. **A duplicate is impossible to record.** A unique index on `publish_events(schedule_id)`
   would reject a second event even if the first two mechanisms failed.

Because all three commit together, there is no window in which content is published but the job
still looks pending, or the reverse.

### The cancel-versus-claim race

`DELETE /api/v1/schedules/{id}` and a worker tick can arrive at the same instant. The cancel is
`UPDATE ... WHERE id = ? AND status = 'pending'`, so exactly one of them wins:

- **Cancel wins** → the schedule is `cancelled` and nothing is published.
- **The worker wins** → the content is published and the cancel returns `409`.

Never both, never neither. Cancellation is best-effort once `run_at` passes, but the caller is
never told the cancel succeeded when it did not. `TestCancelRacesDueTime` runs this race thirty
times and asserts the invariant on every one.

### Clock skew and timing

Every instance reads its own system clock, and no skew correction is attempted. With a
one-second poll interval a publish lands within roughly a second of `run_at`. Anything tighter
than that would need a shared time source, which is out of scope here.

Timestamps are stored as integer Unix milliseconds rather than ISO-8601 text. SQLite has no
datetime type, and Go's `time.RFC3339Nano` trims trailing zeros — which silently breaks the
lexicographic ordering that `WHERE run_at <= ?` depends on. Integers avoid the whole bug class.

---

## Testing

```bash
go test ./...            # 47 tests plus subtests, about 15 seconds
make cover               # 77.3% across internal packages
```

The suite runs against **real SQLite** in a temp directory, not a mock. The failure modes this
project exists to survive — lock contention, unique-index races, transaction rollback — only
exist in a real database.

Ten scenarios from `docs/DESIGN.md` §9 carry most of the weight:

| Scenario | Asserts |
|---|---|
| `TestConcurrentUpdate_OnlyOneWins` | 20 writers on one version → exactly one `200`, nineteen `412`, exactly 2 version rows |
| `TestScheduledPublish_ExactlyOnce_MultiWorker` | 5 workers, one due job → exactly 1 publish event |
| `TestScheduledPublish_SurvivesRestart` | Worker stops, new one starts on the same file → publish still happens |
| `TestLeaseRecoveryAfterCrash` | A claim orphaned by a crash is reclaimed after the lease expires, and published once |
| `TestPublicNeverLeaksUnpublished` | Draft, scheduled-not-due, edited-after-publish, and withdrawn content all stay private |
| `TestCancelRacesDueTime` | 30 rounds; every one resolves to cancelled-or-published, never both |
| `TestPublishOlderVersion` | Publishing v1 while v3 is the draft serves v1 |
| `TestConcurrentCreateSameSlug` | 15 concurrent creates → one `201`, fourteen `409` |
| `TestMigrations_UpDownUp` | Migrations are reversible and re-runnable |
| `TestPublicETagReturns304` | Conditional requests work, and the tag changes when content does |

Concurrency tests release every goroutine from a shared barrier. Launching them in a plain loop
staggers them enough that the test can pass without ever racing — which is worse than no test,
because it looks like coverage.

### A note on `-race`

The race detector needs `CGO_ENABLED=1` and a C compiler. The machine this was written on has
neither, so `-race` runs in CI instead, on `ubuntu-latest`, on every push — see
`.github/workflows/ci.yml`. CI runs the suite twice: once under `-race`, and once with
`CGO_ENABLED=0` to prove the pure-Go path still works.

This matters less than it might seem. The scenarios above assert *outcomes* — "exactly one
`200`", "`COUNT(*) == 1`", "never both" — and those are logic races in SQL, which the race
detector cannot see in any case. It catches Go-level data races, which is a real but different
category, and CI covers it.

Locally: `make test-race` if you do have a C toolchain.

---

## Notable dependencies

| Package | Why |
|---|---|
| `modernc.org/sqlite` | Pure-Go SQLite. Builds with `CGO_ENABLED=0`, so the binary is static and the tests run anywhere Go does. `mattn/go-sqlite3` is more battle-tested but needs a C toolchain. |
| `go-chi/chi/v5` | Subtree-scoped middleware, which is what keeps the admin token attached to `/api/v1` and structurally unable to reach `/public`. |
| `pressly/goose/v3` | Migration tracking, with the SQL embedded via `embed.FS`. |
| `stretchr/testify` | Assertions and `require.Eventually`. |
| `google/uuid` | Content and schedule identifiers. |

No ORM and no code generation. All SQL is hand-written.

---

## Not implemented

Called out deliberately rather than left to be discovered:

- **Real authentication and authorization.** A single static bearer token, which the brief
  waives. There are no users, roles, or per-document permissions, and `created_by` is populated
  from an optional `X-Actor` header rather than a verified identity.
- **Soft delete.** There is no delete endpoint at all.
- Media and asset handling, full-text search, webhooks, rate limiting.
- `Idempotency-Key` on publish. The operation is already idempotent by version, so this would
  only guard against duplicate *requests*, not duplicate effects.
- Cursor pagination. Limit/offset is adequate at this scale and is honest about being so.
- Server-rendered HTML. The SEO work here is metadata, sitemap, robots, and conditional caching —
  the parts a backend owns. Rendered markup belongs to whatever consumes this API.
