// Package clock makes "now" injectable so scheduling behaviour can be tested
// without sleeping.
//
// Clock is deliberately only a time *source*, not a full scheduler abstraction.
// The worker's poll ticker stays a real time.Ticker with a configurable
// interval (one second in production, a few milliseconds in tests). Tests then
// advance a Fake clock to make a job due and wait for the real ticker to notice.
// That combination gets deterministic due-times without the complexity of a
// virtual time implementation.
package clock

import (
	"sync"
	"time"
)

// Clock reports the current time.
type Clock interface {
	Now() time.Time
}

// Real reports the system time.
type Real struct{}

// Now returns the current system time in UTC.
func (Real) Now() time.Time { return time.Now().UTC() }

// Fake is a manually advanced clock for tests. It is safe for concurrent use,
// which matters because the concurrency tests read it from many goroutines
// while the test body advances it.
type Fake struct {
	mu  sync.RWMutex
	now time.Time
}

// NewFake returns a Fake clock started at t.
func NewFake(t time.Time) *Fake {
	return &Fake{now: t.UTC()}
}

// Now returns the fake clock's current time.
func (f *Fake) Now() time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.now
}

// Advance moves the clock forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// Set moves the clock to an absolute time.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t.UTC()
}

// ToMillis converts a time to Unix milliseconds, the on-disk representation for
// every timestamp in this service. See docs/DESIGN.md section 2 for why the
// schema stores integers rather than ISO-8601 text.
func ToMillis(t time.Time) int64 { return t.UTC().UnixMilli() }

// FromMillis converts Unix milliseconds back to a UTC time.
func FromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
