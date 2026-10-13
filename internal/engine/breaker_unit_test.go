package engine

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"easbridge/internal/store"
	"easbridge/internal/upstream"
)

// Unit tests of the circuit breaker with a clock the test moves by hand. The
// tests that run it inside an engine are in breaker_test.go.

type handClock struct{ t time.Time }

func (c *handClock) now() time.Time          { return c.t }
func (c *handClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestBreaker(c *handClock) *breaker {
	return newBreaker(Config{Clock: c.now, BreakerBase: 5 * time.Second, BreakerMax: 15 * time.Minute, BreakerThreshold: 3},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestBreakerOpensAfterThresholdConsecutiveFailures(t *testing.T) {
	c := &handClock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	b := newTestBreaker(c)
	b.upstreamTrouble("HTTP 500", false, 0)
	b.upstreamTrouble("HTTP 500", false, 0)
	if b.remaining() != 0 {
		t.Fatal("two failures must not open the breaker")
	}
	b.upstreamTrouble("HTTP 500", false, 0)
	if got := b.remaining(); got != 5*time.Second {
		t.Fatalf("pause = %v, want 5s", got)
	}
	c.advance(5*time.Second + time.Millisecond)
	if b.remaining() != 0 {
		t.Fatal("the pause must end")
	}
}

func TestBreakerAnAnswerBetweenFailuresStartsTheCountAgain(t *testing.T) {
	c := &handClock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	b := newTestBreaker(c)
	for i := 0; i < 10; i++ {
		b.upstreamTrouble("HTTP 500", false, 0)
		b.upstreamTrouble("HTTP 500", false, 0)
		b.upstreamOK()
	}
	if b.remaining() != 0 {
		t.Fatal("failures that never come three in a row must not open the breaker")
	}
}

func TestBreakerOpensAtOnceWhenTheServerAsksToSlowDown(t *testing.T) {
	c := &handClock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	b := newTestBreaker(c)
	b.upstreamTrouble("HTTP 429", true, 0)
	if got := b.remaining(); got != 5*time.Second {
		t.Fatalf("pause = %v, want 5s", got)
	}
}

func TestBreakerPauseDoublesWithEveryTripAndIsCapped(t *testing.T) {
	c := &handClock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	b := newTestBreaker(c)
	var got []time.Duration
	for i := 0; i < 11; i++ {
		b.upstreamTrouble("HTTP 503", true, 0)
		d := b.remaining()
		got = append(got, d)
		c.advance(d + time.Millisecond) // the pause ends, the next call fails again
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second,
		160 * time.Second, 320 * time.Second, 640 * time.Second, 15 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pauses = %v, want %v", got, want)
		}
	}
}

func TestBreakerHonoursRetryAfterButNotBeyondTheCap(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after time.Duration
		want  time.Duration
	}{
		{"shorter than the base: the base wins", time.Second, 5 * time.Second},
		{"longer: the server's wish wins", 10 * time.Minute, 10 * time.Minute},
		{"absurd: capped at fifteen minutes", 48 * time.Hour, 15 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &handClock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
			b := newTestBreaker(c)
			b.upstreamTrouble("HTTP 429", true, tc.after)
			if got := b.remaining(); got != tc.want {
				t.Fatalf("pause = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBreakerNeverShortensAPauseThatIsRunning(t *testing.T) {
	c := &handClock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	b := newTestBreaker(c)
	b.upstreamTrouble("HTTP 429", true, 10*time.Minute)
	b.upstreamTrouble("HTTP 500", true, 0) // another loop failed at the same time
	if got := b.remaining(); got != 10*time.Minute {
		t.Fatalf("pause = %v, want it to stay 10m", got)
	}
}

func TestBreakerStragglersOfTheSameIncidentDoNotEscalate(t *testing.T) {
	c := &handClock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	b := newTestBreaker(c)
	// Four requests were in flight when the first answer opened the breaker.
	for i := 0; i < 4; i++ {
		b.upstreamTrouble("HTTP 503", true, 0)
	}
	if got := b.remaining(); got != 5*time.Second {
		t.Fatalf("pause = %v, want 5s", got)
	}
	if _, _, trips := b.state(); trips != 1 {
		t.Fatalf("trips = %d, want 1", trips)
	}
	c.advance(5*time.Second + time.Millisecond)
	b.upstreamTrouble("HTTP 503", true, 0) // the first request after the pause fails again: now it escalates
	if got := b.remaining(); got != 10*time.Second {
		t.Fatalf("pause = %v, want 10s", got)
	}
}

func TestBreakerStartsSmallAgainAfterASuccess(t *testing.T) {
	c := &handClock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	b := newTestBreaker(c)
	for i := 0; i < 4; i++ {
		b.upstreamTrouble("HTTP 503", true, 0)
		c.advance(b.remaining() + time.Millisecond)
	}
	b.upstreamOK()
	b.upstreamTrouble("HTTP 503", true, 0)
	if got := b.remaining(); got != 5*time.Second {
		t.Fatalf("after recovery the pause is %v, want 5s", got)
	}
}

func TestBreakerCountsFailedDatabaseWrites(t *testing.T) {
	c := &handClock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	b := newTestBreaker(c)
	b.localFailure(store.ErrInjected)
	b.localFailure(store.ErrInjected)
	b.localOK() // one write worked: the count starts again
	b.localFailure(store.ErrInjected)
	b.localFailure(store.ErrInjected)
	if b.remaining() != 0 {
		t.Fatal("not three in a row")
	}
	b.localFailure(store.ErrInjected)
	if b.remaining() != 5*time.Second {
		t.Fatalf("pause = %v", b.remaining())
	}
	if _, reason, _ := b.state(); !strings.Contains(reason, "database") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestTroubleOfClassifiesUpstreamErrors(t *testing.T) {
	wrap := func(err error) error { return fmt.Errorf("Sync: %w", err) }
	for _, tc := range []struct {
		name         string
		err          error
		trouble, exp bool
		after        time.Duration
	}{
		{"429 with Retry-After", wrap(&upstream.ThrottleError{HTTP: 429, RetryAfter: 90 * time.Second, Explicit: true}), true, true, 90 * time.Second},
		{"503", wrap(&upstream.ThrottleError{HTTP: 503, Explicit: true}), true, true, 0},
		{"502", wrap(&upstream.ThrottleError{HTTP: 502}), true, false, 0},
		{"EAS 111 retry later", &upstream.StatusError{Op: "Sync", Code: 111}, true, true, 0},
		{"EAS 110 server error", &upstream.StatusError{Op: "Sync", Code: 110}, true, false, 0},
		{"a permanent EAS status is about the request", &upstream.StatusError{Op: "Sync", Code: 8, Perm: true}, false, false, 0},
		{"a refused connection is not throttling", wrap(upstream.ErrUnavailable), false, false, 0},
		{"a plain error", errors.New("boom"), false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, trouble, exp, after := troubleOf(tc.err)
			if trouble != tc.trouble || exp != tc.exp || after != tc.after {
				t.Fatalf("troubleOf = trouble %v explicit %v after %v; want %v %v %v", trouble, exp, after, tc.trouble, tc.exp, tc.after)
			}
		})
	}
}

func TestPausedErrorIsUnavailableAndRecognised(t *testing.T) {
	var err error = &pausedError{until: time.Date(2026, 10, 9, 12, 0, 5, 0, time.UTC), reason: "HTTP 429"}
	if !errors.Is(err, upstream.ErrUnavailable) || !errors.Is(err, ErrUnavailable) ||
		!isPaused(fmt.Errorf("wrapped: %w", err)) || isPaused(upstream.ErrUnavailable) {
		t.Fatal("a paused error must behave as both ErrUnavailable values and be recognisable")
	}
}
