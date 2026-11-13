package app_test

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
)

// referenceLimiter is the limiter of failed sign-ins as it was written
// first: a map, and a walk of all of it to choose what to forget when it is
// full. The limiter must choose as it did.
type referenceLimiter struct {
	limit, capacity int
	window          time.Duration
	failed          map[string]*referenceWindow
}

type referenceWindow struct {
	since time.Time
	count int
}

func (l *referenceLimiter) reserve(key string, now time.Time) bool {
	w, ok := l.failed[key]
	if ok && now.Sub(w.since) < l.window {
		if w.count >= l.limit {
			return false
		}
		w.count++
		return true
	}
	if !ok && len(l.failed) >= l.capacity {
		for k, window := range l.failed {
			if now.Sub(window.since) >= l.window {
				delete(l.failed, k)
			}
		}
		if len(l.failed) >= l.capacity {
			var victim string
			var least *referenceWindow
			for k, window := range l.failed {
				if least == nil || window.count < least.count || (window.count == least.count && window.since.Before(least.since)) {
					victim, least = k, window
				}
			}
			delete(l.failed, victim)
		}
	}
	l.failed[key] = &referenceWindow{since: now, count: 1}
	return true
}

func (l *referenceLimiter) forgive(key string) {
	if w, ok := l.failed[key]; ok && w.count > 0 {
		w.count--
	}
}

func (l *referenceLimiter) live(now time.Time) map[string]int {
	live := map[string]int{}
	for key, w := range l.failed {
		if now.Sub(w.since) < l.window {
			live[key] = w.count
		}
	}
	return live
}

// TestTheAttemptLimiterForgetsAsBefore: the limiter finds what to forget
// without walking every window (review of 2026-11-13), and forgets what the
// walk forgot: a window that has ended, else the one with the fewest
// failures, the oldest of those. Random attempts, forgiven attempts and
// pauses on a small limiter are compared with the first version, step by
// step; every step is a moment later than the one before, so no two windows
// begin together and the choice is never a tie.
func TestTheAttemptLimiterForgetsAsBefore(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		limit, capacity, keys int
		window                time.Duration
	}{
		{limit: 3, capacity: 4, keys: 8, window: time.Minute},
		{limit: 5, capacity: 16, keys: 40, window: 10 * time.Second},
		{limit: 1, capacity: 1, keys: 3, window: time.Second},
		{limit: 10, capacity: 64, keys: 64, window: time.Hour},
	} {
		t.Run(fmt.Sprintf("%d per window, %d windows, %d keys", tt.limit, tt.capacity, tt.keys), func(t *testing.T) {
			t.Parallel()
			random := rand.New(rand.NewPCG(uint64(tt.capacity), uint64(tt.keys)))
			got := app.NewAttemptLimiter(tt.limit, tt.window, tt.capacity)
			want := &referenceLimiter{limit: tt.limit, capacity: tt.capacity, window: tt.window, failed: map[string]*referenceWindow{}}
			now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
			for step := range 20_000 {
				now = now.Add(time.Millisecond)
				key := fmt.Sprintf("key-%d", random.IntN(tt.keys))
				switch op := random.IntN(20); {
				case op == 0:
					now = now.Add(tt.window / 3)
				case op < 4:
					got.Forgive(key)
					want.forgive(key)
				default:
					if g, w := got.Reserve(key, now), want.reserve(key, now); g != w {
						t.Fatalf("step %d: Reserve(%s) = %v, the first version %v", step, key, g, w)
					}
				}
				if g, w := got.Live(now), want.live(now); !maps.Equal(g, w) {
					t.Fatalf("step %d: live windows %v, the first version %v", step, g, w)
				}
			}
		})
	}
}

// BenchmarkAFullAttemptLimiter: a limiter as full as a node's, given a new
// key at every attempt - each makes it forget a window.
func BenchmarkAFullAttemptLimiter(b *testing.B) {
	l := app.NewAttemptLimiter(app.SignInAttempts, time.Hour, app.SignInAddresses)
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	for i := range app.SignInAddresses {
		l.Reserve(fmt.Sprintf("filler-%d", i), now)
	}
	b.ResetTimer()
	for i := range b.N {
		now = now.Add(time.Nanosecond)
		l.Reserve(fmt.Sprintf("new-%d", i), now)
	}
}
