# Content Management Service

A content management backend in Go and SQLite: versioned content with optimistic locking,
scheduled publishing that survives restarts and never double-publishes, a public API that cannot
serve unpublished content, and server-rendered pages built for search engines.

This README is self-contained: how to run it, the technical decisions, the assumptions made, and
what is deliberately missing. `docs/DESIGN.md` is optional further reading — the schema
rationale, the failure modes the design targets, and the SQL-level flows.

---

## Where everything is

Mapping each item in the brief to the code that satisfies it.

**Required features**

| Requirement | Implementation | Verified by |
|---|---|---|
| Create and edit content | `POST` / `PUT /api/v1/contents` | `internal/httpapi/admin.go` |
| Record change history | `GET /api/v1/contents/{id}/versions` — versions are append-only | `TestVersionHistory` |
| Concurrent edits must not silently overwrite | `If-Match` required → `412` on conflict, `428` if omitted | `TestConcurrentUpdate_OnlyOneWins` |
| Publish a version | `POST /api/v1/contents/{id}/publish` | `TestPublishOlderVersion` |
| Schedule a future publish | `POST /api/v1/contents/{id}/schedules` | `TestScheduledPublish_HappyPath` |
| Cancel a schedule | `DELETE /api/v1/schedules/{id}` | `TestCancelSchedule` |
| Public API returns only published content | `/public/v1/*` and `GET /{slug}` | `TestPublicNeverLeaksUnpublished` |

**The five stated assumptions**

| Assumption | How it is handled | Test |
|---|---|---|
| Many concurrent requests | Compare-and-swap on `current_version`; unique indexes resolve create/schedule races | `TestConcurrentUpdate_OnlyOneWins`, `TestConcurrentCreateSameSlug` |
| Multiple application instances | Leased claim protocol; atomic `UPDATE` on a single-writer pool | `TestScheduledPublish_ExactlyOnce_MultiWorker` (5 workers, 1 job, 1 event) |
| Process may stop or restart at any time | Schedules live in a table, not a timer; graceful shutdown releases claims | `TestGracefulShutdownReleasesClaims` |
| Scheduled publishing still works after restart | Worker reclaims on the next poll; expired leases are stealable | `TestScheduledPublish_SurvivesRestart`, `TestLeaseRecoveryAfterCrash` |
| Unpublished content must never reach the public API | Public reads join through `published_version_id`; there is no status filter to forget | `TestPublicNeverLeaksUnpublished`, `TestHTMLPage_NeverLeaksUnpublished` |

**Deliverables**

| Asked for | Where |
|---|---|
| Source code | this repository |
| Database migration | `migrations/00001_init.sql`, embedded and applied on boot |
| How to run the project | [Quick start](#quick-start) below |
| Automated tests for high-risk scenarios | [Testing](#testing) — the table of ten and why each one matters |
| Assumptions, decisions, unfinished work, limitations | [Assumptions](#assumptions) · [Technical decisions](#technical-decisions) · [Not implemented](#not-implemented) · [Known limitations](#known-limitations) |
| *Bonus:* good for SEO | [SEO](#seo) — rendered pages, sitemap, robots, conditional caching |

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

| Method | Path | Returns |
|---|---|---|
| `GET` | `/{slug}` | **Rendered HTML page** — the canonical URL, what crawlers index |
| `GET` | `/public/v1/contents` | JSON list of published content |
| `GET` | `/public/v1/contents/{slug}` | JSON for one published document |
| `GET` | `/sitemap.xml` | Published, indexable pages |
| `GET` | `/robots.txt` | Crawler directives + sitemap pointer |
| `GET` | `/healthz` | Liveness, including a database write-pool ping |

All three content routes go through the same store query, so they share one guarantee: a draft
has no row to return. `/{slug}` is registered last as a wildcard; chi matches static segments
first, so it cannot shadow any route above it (`TestHTMLPage_DoesNotShadowOtherRoutes`).

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

**The rendered page**, which is what a crawler sees at the canonical URL:

```bash
curl "$B/lifecycle"
```
```html
<title>SQLite Concurrency, Explained</title>
<meta name="description" content="Why SQLite permits one writer, and how WAL mode keeps readers going.">
<link rel="canonical" href="http://localhost:8080/how-sqlite-handles-concurrency">
<meta property="og:type" content="article">
<meta property="og:title" content="SQLite Concurrency, Explained">
<meta property="og:image" content="http://localhost:8080/img/cover.png">
<script type="application/ld+json">{"@context":"https://schema.org","@type":"Article",
  "headline":"SQLite Concurrency, Explained","datePublished":"2026-07-30T17:01:36.774Z", ...}</script>
...
<h1>How SQLite Handles Concurrency</h1>
<div class="body">SQLite allows exactly one writer at a time.

WAL mode lets readers run alongside that writer.</div>
```

Note `<title>` and `<h1>` differ — `meta_title` drives the search result, the content title
drives the page. The body text is in the markup, so no JavaScript is needed to index it.

**Sitemap and conditional requests.**

```bash
curl "$B/sitemap.xml"
```
```xml
<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url>
    <loc>http://localhost:8080/lifecycle</loc>
    <lastmod>2026-07-30T15:45:45Z</lastmod>
  </url>
</urlset>
```

Every `<loc>` resolves to a rendered page, not a JSON endpoint — a sitemap listing URLs that
return `application/json` advertises pages no crawler can index.
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

## Assumptions

Things taken as given. Where an assumption is wrong for your deployment, the note says what
breaks.

**About the environment**

- **One host.** SQLite is a file, so every process sharing this database shares a filesystem.
  Multiple *processes* on one machine are fully supported and tested; multiple *machines* are
  not, and WAL over NFS is explicitly unsafe. See [SQLite and multiple instances](#sqlite-and-multiple-instances--the-honest-version).
- **Each instance trusts its own clock.** No skew correction. With a one-second poll a publish
  lands within about a second of `run_at`; two instances whose clocks differ by more than that
  will disagree on which jobs are due. Harmless here because claiming is atomic, but it is why
  timing is "within about a second" rather than exact.
- **The process may be killed without warning.** Nothing is held only in memory. A schedule is a
  row, and a half-finished job is recovered from an expired lease rather than from any in-process
  state.

**About the callers**

- **Admin callers are trusted.** One shared bearer token, per the brief. There are no users or
  roles, so `created_by` records an optional `X-Actor` header rather than a verified identity —
  useful as an audit hint, not as evidence.
- **Content bodies are plain text, not markup.** The rendered page escapes them and preserves
  newlines. If you want Markdown or rich text, that conversion belongs in a layer above this one;
  rendering author-supplied HTML would be a stored-XSS vector
  (`TestHTMLPage_EscapesContent` pins the current behaviour).
- **Slugs are supplied, not generated.** They are validated (lowercase, alphanumeric, single
  hyphens) rather than sanitised, because silently rewriting a caller's slug would make the
  public URL unpredictable.

**About the data**

- **Version numbers are per-document**, starting at 1, and never reused.
- **Content volume is modest.** Limit/offset pagination and a single-file sitemap are fine for
  thousands of documents. Past ~50,000 published pages the sitemap needs splitting into an index.

---

## Technical decisions

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

### SEO

The brief asks for a *website* that is good for SEO, so the content is served as crawlable HTML
at its canonical URL — not only as JSON.

`GET /{slug}` renders a page server-side with `html/template`:

- `<title>` from `meta_title`, falling back to the content title. The on-page `<h1>` keeps the
  **content** title, because the two differing is the entire reason `meta_title` exists.
- `<meta name="description">`, `<link rel="canonical">`, Open Graph and Twitter Card tags.
- **JSON-LD `Article`** — `headline`, `description`, `datePublished`, `dateModified`, `image`.
- `<meta name="robots" content="noindex, nofollow">` when the version sets `noindex`.
- The article text is **in the markup**, so a crawler needs no JavaScript.

Around it, the parts of SEO a backend genuinely owns:

| | |
|---|---|
| **Discovery** | `/sitemap.xml` from published content, `<lastmod>` from `published_at`. `noindex` pages are excluded — advertising a page you told crawlers to ignore is a contradictory signal that search consoles report as an error. |
| **Crawl budget** | `ETag` + `Last-Modified` on every public response, `304` on `If-None-Match`, `Cache-Control: public, max-age=60, stale-while-revalidate=300`. A crawler re-fetching an unchanged page costs almost nothing. |
| **Correct status codes** | Unpublished and unknown slugs both return a real `404`, not a `200` with an error body. A soft 404 is treated as a quality problem. |
| **Metadata versioning** | SEO fields live on the version, not the document, so rolling back to an older version rolls back its metadata too (`TestSEOMetadataIsPerVersion`). |
| **Syndication** | An author-supplied `canonical_url` overrides the generated one. |

What is deliberately *not* here: styling beyond a minimal readable stylesheet, images, and any
client-side behaviour. The page exists to be correct and crawlable, not to be a design.

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
go test ./...            # 55 tests plus subtests, about 18 seconds
make cover               # 81.8% across internal packages
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
| `TestHTMLPage_NeverLeaksUnpublished` | The rendered page is a second surface a draft could escape through, so it is asserted separately |
| `TestMigrations_UpDownUp` | Migrations are reversible and re-runnable |
| `TestPublicETagReturns304` | Conditional requests work, and the tag changes when content does |

Concurrency tests release every goroutine from a shared barrier. Launching them in a plain loop
staggers them enough that the test can pass without ever racing — which is worse than no test,
because it looks like coverage.

### Running under `-race`

The race detector needs `CGO_ENABLED=1` and a C compiler. If you have one:

```bash
make test-race
```

If you do not — which was the case on the machine this was written on — any Linux container
works:

```bash
docker run --rm -v "$PWD:/src" -w /src -e CGO_ENABLED=1 golang:1.26 \
  go test ./... -race -count=1
```

CI runs the suite twice on every push, once under `-race` and once with `CGO_ENABLED=0` to prove
the pure-Go path still builds and passes: `.github/workflows/ci.yml`.

This was worth doing rather than waving away. The first CI run under `-race` failed, and the
cause was real: `store.Migrate` called goose's `SetBaseFS`, `SetLogger` and `SetDialect` on every
invocation, and those write package-level globals inside goose. Tests open a database per test
and run in parallel, so they were racing on that configuration. The settings never vary, so a
`sync.Once` fixes it properly.

Nothing in the suite's outcome assertions would ever have caught that — it is a Go-level data
race, not a logic race in SQL — which is precisely the argument for running the detector
somewhere rather than reasoning that the tests look concurrent enough.

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

Called out deliberately rather than left to be discovered.

- **Real authentication and authorization.** A single static bearer token, which the brief
  waives. There are no users, roles, or per-document permissions, and `created_by` comes from an
  optional `X-Actor` header rather than a verified identity.
- **Delete, hard or soft.** There is no delete endpoint. Unpublishing withdraws content from the
  public API but keeps every version.
- **Media and asset handling.** `og_image_url` stores a URL; nothing is uploaded or served.
- **Full-text search, webhooks, rate limiting.**
- **`Idempotency-Key` on publish.** The operation is already idempotent by version, so this would
  only guard against duplicate *requests*, not duplicate effects.
- **Cursor pagination.** Limit/offset is adequate at this scale and honest about being so.
- **A sitemap index.** One sitemap file, capped at 50,000 URLs — the protocol limit before an
  index is required.

---

## Known limitations

Things that *are* implemented, but whose edges are worth knowing.

- **One host only.** The claim protocol is correct for many processes sharing one file, and
  tested that way. It is not correct across machines, because SQLite is not a network database.
  The protocol itself ports to Postgres unchanged — swap the driver, add
  `FOR UPDATE SKIP LOCKED`.
- **Scheduled publishing is accurate to roughly the poll interval**, not to the millisecond, and
  the tail is bounded by `SCHEDULER_LEASE_TTL` if a worker dies mid-job.
- **Cancellation is best-effort once `run_at` passes.** The outcome is never ambiguous — see
  [the cancel-versus-claim race](#the-cancel-versus-claim-race) — but a caller racing the
  deadline may legitimately receive `409` and find the content published.
- **A failed job retries indefinitely.** `attempts` is recorded and `last_error` is stored, but
  there is no backoff and nothing moves a permanently failing job to `failed`. In practice the
  only realistic failure is the database being unavailable, which fails everything anyway.
- **Write throughput is one transaction at a time**, by design. Correct for a CMS, where reads
  vastly outnumber writes; wrong for a write-heavy workload, which would want Postgres.
- **The rendered page is minimal.** Correct and crawlable, with no theming, images, navigation,
  or pagination — see [SEO](#seo) for exactly what it does and does not do.
- **No observability beyond structured logs.** No metrics, no tracing, no per-endpoint timing
  beyond the duration on each request log line.
