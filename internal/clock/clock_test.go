package clock_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/luongthanhtung03/qdt-test/internal/clock"
)

func TestFake_AdvanceAndSet(t *testing.T) {
	base := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	f := clock.NewFake(base)

	require.Equal(t, base, f.Now())

	f.Advance(90 * time.Minute)
	require.Equal(t, base.Add(90*time.Minute), f.Now())

	f.Set(base)
	require.Equal(t, base, f.Now())
}

// TestFake_ConcurrentAccess matters because the concurrency tests read the
// clock from many goroutines while the test body advances it. An unguarded
// field here would be a data race in every one of them.
func TestFake_ConcurrentAccess(t *testing.T) {
	f := clock.NewFake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 200 {
				_ = f.Now()
			}
		}()
		go func() {
			defer wg.Done()
			for range 200 {
				f.Advance(time.Millisecond)
			}
		}()
	}
	wg.Wait()

	require.Equal(t, 8*200, int(f.Now().Sub(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)).Milliseconds()),
		"every advance must be accounted for")
}

// TestMillisRoundTrip guards the on-disk timestamp representation. Storing
// integers rather than ISO-8601 text is what keeps "WHERE run_at <= ?" sorting
// correctly; see docs/DESIGN.md section 2.
func TestMillisRoundTrip(t *testing.T) {
	for _, tc := range []time.Time{
		time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC),
		time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2099, 12, 31, 23, 59, 59, 999_000_000, time.UTC),
	} {
		ms := clock.ToMillis(tc)
		require.Equal(t, tc, clock.FromMillis(ms))
	}
}

// TestMillisOrderingIsMonotonic is the property the scheduler's due check
// depends on: later times must compare greater as integers.
func TestMillisOrderingIsMonotonic(t *testing.T) {
	base := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	prev := clock.ToMillis(base)

	for _, d := range []time.Duration{
		time.Millisecond, time.Second, time.Minute, time.Hour, 24 * time.Hour,
	} {
		next := clock.ToMillis(base.Add(d))
		require.Greater(t, next, prev)
		prev = next
	}
}

// TestReal_IsUTC keeps every timestamp in one zone, so a server running in a
// non-UTC locale cannot write local times into the database.
func TestReal_IsUTC(t *testing.T) {
	require.Equal(t, time.UTC, clock.Real{}.Now().Location())
}
