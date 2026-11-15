package metrics

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}

func TestEveryMetricFromThePlanIsExposed(t *testing.T) {
	m := New()
	m.Requests.WithLabelValues("api", Allowed).Inc()
	m.ObserveDecision("memory", "sliding_window", 12*time.Microsecond)
	m.Upstream.WithLabelValues("api").Observe(0.002)
	m.LimiterErrors.WithLabelValues("redis").Inc()
	m.TrackKeys("memory", func() int { return 42 })
	m.TrackGossip(func() GossipStats { return GossipStats{PacketsSent: 7, BytesSent: 900, Members: 3, EntriesMerged: 11} })
	m.GossipLatency.Observe(0.004)

	out := scrape(t, m)
	for _, want := range []string{
		`gateway_requests_total{decision="allowed",route="api"} 1`,
		`gateway_limiter_decision_seconds_bucket{algorithm="sliding_window",backend="memory",le="1e-05"} 0`,
		`gateway_limiter_decision_seconds_bucket{algorithm="sliding_window",backend="memory",le="1.5000000000000002e-05"} 1`,
		`gateway_upstream_seconds_count{route="api"} 1`,
		`gateway_limiter_errors_total{backend="redis"} 1`,
		`limiter_tracked_keys{backend="memory"} 42`,
		`gossip_packets_sent_total 7`,
		`gossip_bytes_sent_total 900`,
		`gossip_members 3`,
		`gossip_entries_merged_total 11`,
		`gossip_delivery_seconds_count 1`,
		`go_goroutines`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scrape is missing %q", want)
		}
	}
}

// The plan fixes the decision buckets: 10 µs, ×1.5, 24 of them.
func TestDecisionBuckets(t *testing.T) {
	m := New()
	m.ObserveDecision("memory", "token_bucket", time.Microsecond)
	out := scrape(t, m)
	n := strings.Count(out, `gateway_limiter_decision_seconds_bucket{algorithm="token_bucket"`)
	if n != 25 { // 24 buckets plus +Inf
		t.Errorf("found %d bucket lines, want 25", n)
	}
	if !strings.Contains(out, `le="1e-05"`) {
		t.Error("first bucket is not 10 µs")
	}
}

// Each Metrics has its own registry, so two in one process do not clash.
func TestTwoInstancesDoNotClash(t *testing.T) {
	a, b := New(), New()
	a.TrackKeys("memory", func() int { return 1 })
	b.TrackKeys("memory", func() int { return 2 })
	if !strings.Contains(scrape(t, b), `limiter_tracked_keys{backend="memory"} 2`) {
		t.Error("second instance does not show its own value")
	}
}
