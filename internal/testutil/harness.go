// Package testutil provides the shared test harness: a real SQLite database in
// a temp directory, a fake clock, and an httptest server wired exactly as
// production is.
//
// Tests run against real SQL rather than a mock. The failure modes this project
// is built to survive -- lock contention, unique-index races, transaction
// rollback -- only exist in a real database, so a mock would test nothing that
// matters.
package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/luongthanhtung03/qdt-test/internal/clock"
	"github.com/luongthanhtung03/qdt-test/internal/config"
	"github.com/luongthanhtung03/qdt-test/internal/httpapi"
	"github.com/luongthanhtung03/qdt-test/internal/scheduler"
	"github.com/luongthanhtung03/qdt-test/internal/store"
)

// AdminToken is the bearer token every harness accepts.
const AdminToken = "test-admin-token"

// Silence request logging for the whole test binary.
//
// Done in init rather than per-harness because slog.SetDefault writes a global:
// calling it from parallel tests would be the very kind of race these tests
// exist to catch. init runs once, before any test starts.
func init() {
	slog.SetDefault(slog.New(slog.DiscardHandler))
}

// BaseTime is the fixed instant every fake clock starts at. A constant start
// makes failures reproducible in a way that time.Now() would not.
var BaseTime = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

// Harness is a running service backed by a temporary database.
type Harness struct {
	T      *testing.T
	DB     *store.DB
	Clock  *clock.Fake
	Cfg    config.Config
	Server *httptest.Server
	DBPath string
}

// Option customises a harness before it starts.
type Option func(*config.Config)

// WithPollInterval overrides the scheduler poll interval.
func WithPollInterval(d time.Duration) Option {
	return func(c *config.Config) { c.PollInterval = d }
}

// WithLeaseTTL overrides the scheduler lease duration.
func WithLeaseTTL(d time.Duration) Option {
	return func(c *config.Config) { c.LeaseTTL = d }
}

// WithPublicBaseURL overrides the base URL used in sitemap entries.
func WithPublicBaseURL(u string) Option {
	return func(c *config.Config) { c.PublicBaseURL = u }
}

// DefaultConfig returns the config a harness uses unless overridden.
//
// The poll interval is short because the ticker is real even though the clock
// is fake: tests advance fake time to make a job due, then wait for a real tick
// to notice. See internal/clock for why that split exists.
func DefaultConfig(dbPath string) config.Config {
	return config.Config{
		Addr:          "127.0.0.1:0",
		DBPath:        dbPath,
		AdminAPIToken: AdminToken,
		PublicBaseURL: "https://example.test",
		PollInterval:  20 * time.Millisecond,
		LeaseTTL:      2 * time.Second,
		BatchSize:     10,
		ShutdownGrace: 5 * time.Second,
	}
}

// New starts a harness with a migrated database and a running HTTP server.
func New(t *testing.T, opts ...Option) *Harness {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	cfg := DefaultConfig(dbPath)
	for _, opt := range opts {
		opt(&cfg)
	}

	db := OpenDB(t, dbPath)
	clk := clock.NewFake(BaseTime)

	srv := httptest.NewServer(httpapi.New(db, cfg, clk).Routes())
	t.Cleanup(srv.Close)

	return &Harness{T: t, DB: db, Clock: clk, Cfg: cfg, Server: srv, DBPath: dbPath}
}

// OpenDB opens and migrates a database at path, closing it when the test ends.
// Exported so restart and multi-worker tests can open extra handles onto the
// same file.
func OpenDB(t *testing.T, path string) *store.DB {
	t.Helper()
	db, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Migrate(t.Context()))
	return db
}

// Response is a captured HTTP response with its body already read, so tests can
// assert on status and body without worrying about closing anything.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// JSON unmarshals the body into dst.
func (r *Response) JSON(t *testing.T, dst any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(r.Body, dst),
		"response body is not valid JSON: %s", string(r.Body))
}

// ETag returns the response entity tag.
func (r *Response) ETag() string { return r.Header.Get("ETag") }

// ErrorCode extracts the machine-readable code from an error envelope.
func (r *Response) ErrorCode(t *testing.T) string {
	t.Helper()
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	r.JSON(t, &body)
	return body.Error.Code
}

// Request describes one HTTP call.
type Request struct {
	Method  string
	Path    string
	Body    any               // marshalled as JSON when non-nil
	Raw     string            // used verbatim when Body is nil and Raw is set
	Headers map[string]string // additional headers
	NoAuth  bool              // omit the bearer token
}

// Do performs a request against the harness server.
func (h *Harness) Do(req Request) *Response {
	h.T.Helper()

	var body io.Reader
	switch {
	case req.Body != nil:
		encoded, err := json.Marshal(req.Body)
		require.NoError(h.T, err)
		body = bytes.NewReader(encoded)
	case req.Raw != "":
		body = bytes.NewReader([]byte(req.Raw))
	}

	opCtx, cancelOp := h.opContext()
	defer cancelOp()

	httpReq, err := http.NewRequestWithContext(opCtx, req.Method, h.Server.URL+req.Path, body)
	require.NoError(h.T, err)

	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if !req.NoAuth {
		httpReq.Header.Set("Authorization", "Bearer "+AdminToken)
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := h.Server.Client().Do(httpReq)
	require.NoError(h.T, err)
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(h.T, err)

	return &Response{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: raw}
}

// Convenience wrappers for the common verbs.

func (h *Harness) GET(path string) *Response {
	h.T.Helper()
	return h.Do(Request{Method: http.MethodGet, Path: path})
}

func (h *Harness) POST(path string, body any) *Response {
	h.T.Helper()
	return h.Do(Request{Method: http.MethodPost, Path: path, Body: body})
}

func (h *Harness) PUT(path string, body any, headers map[string]string) *Response {
	h.T.Helper()
	return h.Do(Request{Method: http.MethodPut, Path: path, Body: body, Headers: headers})
}

func (h *Harness) DELETE(path string) *Response {
	h.T.Helper()
	return h.Do(Request{Method: http.MethodDelete, Path: path})
}

// CreateContentBody is the JSON shape the create endpoint accepts.
type CreateContentBody struct {
	Slug  string  `json:"slug"`
	Title string  `json:"title"`
	Body  string  `json:"body"`
	SEO   SEOBody `json:"seo"`
}

// SEOBody is the SEO sub-object of a content payload.
type SEOBody struct {
	MetaTitle       string `json:"meta_title"`
	MetaDescription string `json:"meta_description"`
	CanonicalURL    string `json:"canonical_url"`
	OGImageURL      string `json:"og_image_url"`
	NoIndex         bool   `json:"noindex"`
}

// UpdateContentBody is the JSON shape the update endpoint accepts.
type UpdateContentBody struct {
	Title string  `json:"title"`
	Body  string  `json:"body"`
	SEO   SEOBody `json:"seo"`
}

// ContentResponse mirrors the admin content DTO.
type ContentResponse struct {
	ID               string  `json:"id"`
	Slug             string  `json:"slug"`
	Version          int64   `json:"version"`
	Status           string  `json:"status"`
	PublishedAt      *string `json:"published_at"`
	PublishedVersion *int64  `json:"published_version"`
	Draft            struct {
		Version int64   `json:"version"`
		Title   string  `json:"title"`
		Body    string  `json:"body"`
		SEO     SEOBody `json:"seo"`
	} `json:"draft"`
}

// CreateContent creates content and asserts it succeeded, returning the
// decoded response. Most tests need content to exist before the interesting
// part starts; this keeps that setup to one line.
func (h *Harness) CreateContent(slug, title string) ContentResponse {
	h.T.Helper()
	resp := h.POST("/api/v1/contents", CreateContentBody{
		Slug: slug, Title: title, Body: "body of " + title,
	})
	require.Equal(h.T, http.StatusCreated, resp.Status,
		"create content failed: %s", string(resp.Body))

	var out ContentResponse
	resp.JSON(h.T, &out)
	return out
}

// UpdateContent edits content with an explicit If-Match version.
func (h *Harness) UpdateContent(id string, ifMatchVersion int64, title string) *Response {
	h.T.Helper()
	return h.PUT("/api/v1/contents/"+id,
		UpdateContentBody{Title: title, Body: "body of " + title},
		map[string]string{"If-Match": ETag(ifMatchVersion)})
}

// ETag formats a version number the way the API does.
func ETag(version int64) string {
	return `"` + strconv.FormatInt(version, 10) + `"`
}

// Concurrently runs fn n times in parallel, releasing every goroutine at the
// same moment.
//
// The shared start barrier is the point: launching goroutines in a plain loop
// staggers them by the cost of go statement setup, which is often enough for
// each one to finish before the next begins. That turns a concurrency test into
// a sequential one that passes for the wrong reason.
func Concurrently(n int, fn func(i int)) {
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	done.Add(n)

	for i := range n {
		go func() {
			defer done.Done()
			start.Wait()
			fn(i)
		}()
	}

	start.Done()
	done.Wait()
}

// CountRows returns the number of rows matching a query, for assertions that
// need to look past the API at what actually landed on disk.
func (h *Harness) CountRows(query string, args ...any) int {
	h.T.Helper()
	ctx, cancel := h.opContext()
	defer cancel()

	var n int
	require.NoError(h.T, h.DB.Read.QueryRowContext(ctx, query, args...).Scan(&n))
	return n
}

// opContext returns the context for a test-side observation.
//
// Deliberately not t.Context(). These helpers are called from inside
// require.Eventually conditions, which testify evaluates on its own goroutine;
// one of those can still be in flight when the assertion is satisfied and the
// test moves on. Tied to the test's context, that straggler would see a
// cancelled context and report a spurious failure. A plain timeout has no such
// race and still cannot hang the suite.
func (h *Harness) opContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// NewWorker builds a scheduler worker against this harness's database.
//
// Each worker gets its own DB handle when openOwnDB is set, which is how the
// multi-instance tests simulate separate processes contending for the same
// SQLite file.
func (h *Harness) NewWorker(workerID string, openOwnDB bool) *scheduler.Worker {
	h.T.Helper()

	db := h.DB
	if openOwnDB {
		db = OpenDB(h.T, h.DBPath)
	}
	return scheduler.New(db, h.Clock, scheduler.Config{
		InstanceID:   workerID,
		PollInterval: h.Cfg.PollInterval,
		LeaseTTL:     h.Cfg.LeaseTTL,
		BatchSize:    h.Cfg.BatchSize,
		Logger:       slog.New(slog.DiscardHandler),
	})
}

// StartWorker runs a worker in the background until the test ends, returning a
// stop function that blocks until the worker has fully shut down (including
// releasing its claims).
func (h *Harness) StartWorker(workerID string, openOwnDB bool) (*scheduler.Worker, func()) {
	h.T.Helper()

	w := h.NewWorker(workerID, openOwnDB)
	ctx, cancel := context.WithCancel(context.Background())

	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		w.Run(ctx)
	}()

	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		done.Wait()
	}
	h.T.Cleanup(stop)
	return w, stop
}

// ScheduleAt schedules a version to publish at the given offset from BaseTime,
// returning the schedule id.
func (h *Harness) ScheduleAt(contentID string, version int64, offset time.Duration) string {
	h.T.Helper()

	resp := h.POST("/api/v1/contents/"+contentID+"/schedules", map[string]any{
		"version": version,
		"run_at":  BaseTime.Add(offset).Format(time.RFC3339),
	})
	require.Equal(h.T, http.StatusCreated, resp.Status,
		"schedule failed: %s", string(resp.Body))

	var out struct {
		ID string `json:"id"`
	}
	resp.JSON(h.T, &out)
	return out.ID
}

// ScheduleStatus reads a schedule's status straight from the database.
func (h *Harness) ScheduleStatus(scheduleID string) string {
	h.T.Helper()
	ctx, cancel := h.opContext()
	defer cancel()

	var status string
	require.NoError(h.T, h.DB.Read.QueryRowContext(ctx,
		`SELECT status FROM publish_schedules WHERE id = ?`, scheduleID).Scan(&status))
	return status
}

// PublicStatus returns the status code the public API gives for a slug.
func (h *Harness) PublicStatus(slug string) int {
	h.T.Helper()
	return h.Do(Request{
		Method: http.MethodGet, Path: "/public/v1/contents/" + slug, NoAuth: true,
	}).Status
}

// WaitFor polls cond until it holds or the deadline passes.
//
// Used instead of a fixed sleep: the worker's ticker is real, so the test needs
// to wait for an actual tick rather than guess how long one takes.
func (h *Harness) WaitFor(what string, cond func() bool) {
	h.T.Helper()
	require.Eventually(h.T, cond, 5*time.Second, 5*time.Millisecond, what)
}
