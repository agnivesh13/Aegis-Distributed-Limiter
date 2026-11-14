package redis

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
)

// These tests need no Redis: every case is settled before a connection is
// made. The tests that talk to Redis are in integration_test.go.

func TestAllowRejectsBadInputWithoutCallingRedis(t *testing.T) {
	b := New("127.0.0.1:1") // nothing listens here; a call that reached it would fail differently
	defer b.Close()

	bucket := limiter.Limit{Algorithm: limiter.TokenBucket, Rate: 10, Period: time.Second, Burst: 20}
	for _, tc := range []struct {
		name  string
		limit limiter.Limit
		cost  int
		want  error
	}{
		{"unknown algorithm", limiter.Limit{Algorithm: "no_such_algorithm", Rate: 10, Period: time.Second}, 1, limiter.ErrUnsupported},
		{"cost of zero", bucket, 0, limiter.ErrInvalidCost},
		{"cost above capacity", bucket, 21, limiter.ErrInvalidCost},
		{"period under a millisecond", limiter.Limit{Algorithm: limiter.TokenBucket, Rate: 10, Period: time.Microsecond}, 1, ErrInvalidPeriod},
		{"period with a fraction of a millisecond", limiter.Limit{Algorithm: limiter.TokenBucket, Rate: 10, Period: 1500 * time.Microsecond}, 1, ErrInvalidPeriod},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := b.Allow(context.Background(), "route:client", tc.limit, tc.cost)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestDecisionFromScriptReply(t *testing.T) {
	l := limiter.Limit{Algorithm: limiter.SlidingWindow, Rate: 100, Period: time.Second}

	got, err := decisionFrom([]int64{1, 42, 0, 1750}, l)
	want := limiter.Decision{Allowed: true, Limit: 100, Remaining: 42, ResetAfter: 1750 * time.Millisecond}
	if err != nil || got != want {
		t.Errorf("allowed reply: got %+v, %v; want %+v", got, err, want)
	}

	got, err = decisionFrom([]int64{0, 0, 750, 1750}, l)
	want = limiter.Decision{Limit: 100, RetryAfter: 750 * time.Millisecond, ResetAfter: 1750 * time.Millisecond}
	if err != nil || got != want {
		t.Errorf("rejected reply: got %+v, %v; want %+v", got, err, want)
	}

	if _, err := decisionFrom([]int64{1, 2, 3}, l); err == nil {
		t.Error("a reply with three values was accepted, want an error")
	}
}

func TestHasCode(t *testing.T) {
	for src, want := range map[string]bool{
		"":                                 false,
		"-- only a comment\n\n  -- more\n": false,
		"-- comment\nreturn 1\n":           true,
		"local x = 1 -- trailing comment":  true,
	} {
		if got := hasCode(src); got != want {
			t.Errorf("hasCode(%q) = %v, want %v", src, got, want)
		}
	}
	if !hasCode(tokenBucketSrc) {
		t.Error("token_bucket.lua reads as having no code")
	}
}
