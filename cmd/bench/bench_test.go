package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The real scenario file must load, and every scenario in the plan's matrix
// must be in it.
func TestScenarioFileLoads(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "bench", "scenarios.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	file, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range file.Scenarios {
		ids = append(ids, s.ID)
	}
	if got := strings.Join(ids, " "); got != "S0 S1 S2 S3 S4 S5 S6 S7 S8 S9 F1 F2 F3" {
		t.Errorf("scenarios = %s", got)
	}

	jobs, err := file.Jobs([]string{"S1"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 6 { // 2 backends × 3 reps
		t.Fatalf("S1 has %d jobs, want 6", len(jobs))
	}
	r := jobs[0].Run
	if r.Gateways != 3 || r.Rate != 2000 || r.Keys != 10 || *r.Warmup != 5*time.Second || r.Duration != 35*time.Second ||
		r.Limit.Rate != 100 || r.Env["GOSSIP_INTERVAL"] != "100ms" {
		t.Errorf("defaults not applied: %+v", r)
	}

	s7, _ := file.Jobs([]string{"s7"}, 1)
	if *s7[0].Run.Warmup != 0 || s7[0].Run.Shape != "burst" {
		t.Errorf("S7 should have no warm-up and the burst shape: %+v", s7[0].Run)
	}
	args := strings.Join(s7[0].LoadgenArgs("x.csv"), " ")
	if !strings.Contains(args, "-shape burst -out x.csv") || !strings.Contains(args, "-burst-rate 10000") || strings.Contains(args, "-duration") {
		t.Errorf("S7 load generator args: %s", args)
	}

	s4, _ := file.Jobs([]string{"S4"}, 1)
	if s4[0].Run.Env["GOSSIP_INTERVAL"] != "10ms" || s4[0].Run.Env["GOSSIP_FANOUT"] != "2" {
		t.Errorf("S4 env should override the interval and keep the fanout: %v", s4[0].Run.Env)
	}

	s6, _ := file.Jobs([]string{"S6"}, 1)
	if !s6[0].Run.Fault.viaToxiproxy() || *s6[0].Run.Fault.RedisDelay != 0 {
		t.Error("S6 at 0 ms must still go through toxiproxy")
	}
	if !strings.Contains(s6[2].Note(), "redis_delay=5ms") {
		t.Errorf("note = %q", s6[2].Note())
	}

	if _, err := file.Jobs([]string{"S1", "S99"}, 0); err == nil || !strings.Contains(err.Error(), "S99") {
		t.Errorf("unknown scenario: err = %v", err)
	}
	all, _ := file.Jobs([]string{"all"}, 1)
	if len(all) < 40 {
		t.Errorf("all scenarios at 1 rep = %d jobs, want the whole matrix", len(all))
	}
}

func TestBadScenarioFile(t *testing.T) {
	const bad = `
defaults: { gateways: 3, rate: 100, keys: 1, duration: 5s, warmup: 10s, reps: 1, limit: { rate: 10, period: 1s } }
scenarios:
  - { id: X, backends: [etcd], runs: [ { label: a } ] }
  - { id: X, backends: [gossip], runs: [ { label: b, gateways: 1, fault: { partition_at: 1s } } ] }
`
	_, err := Load(strings.NewReader(bad))
	for _, want := range []string{"repeated", "unknown backend", "nothing after", "at least 2 gateways"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

const scrapeA = `# HELP gateway_limiter_decision_seconds x
gateway_limiter_decision_seconds_bucket{algorithm="sliding_window",backend="redis",le="0.001"} 50
gateway_limiter_decision_seconds_bucket{algorithm="sliding_window",backend="redis",le="0.002"} 90
gateway_limiter_decision_seconds_bucket{algorithm="sliding_window",backend="redis",le="+Inf"} 100
gossip_bytes_sent_total 1000
gateway_limiter_errors_total{backend="redis"} 2
`

func TestSnapshotSumsGatewaysAndSubtracts(t *testing.T) {
	s := newSnapshot()
	for range 2 {
		if err := s.Add(strings.NewReader(scrapeA)); err != nil {
			t.Fatal(err)
		}
	}
	if s.DecisionBuckets[0.001] != 100 || s.DecisionBuckets[math.Inf(1)] != 200 || s.GossipBytes != 2000 || s.LimiterErrors != 4 {
		t.Errorf("summed snapshot = %+v", s)
	}
	half := newSnapshot()
	half.Add(strings.NewReader(scrapeA))
	d := s.Sub(half)
	if d.DecisionBuckets[0.002] != 90 || d.GossipBytes != 1000 {
		t.Errorf("difference = %+v", d)
	}
}

func TestDecisionQuantile(t *testing.T) {
	s := newSnapshot()
	s.Add(strings.NewReader(scrapeA))
	// 100 observations: 50 at or below 1 ms, 90 at or below 2 ms.
	for q, want := range map[float64]float64{
		0.25: 0.0005, // halfway into the first bucket, which starts at 0
		0.50: 0.001,  // exactly the first bound
		0.70: 0.0015, // halfway between 1 ms and 2 ms
		0.99: 0.002,  // in +Inf: report the last finite bound
	} {
		if got := s.DecisionQuantile(q); math.Abs(got-want) > 1e-12 {
			t.Errorf("q%.2f = %v, want %v", q, got, want)
		}
	}
	if got := newSnapshot().DecisionQuantile(0.99); got != 0 {
		t.Errorf("empty histogram = %v, want 0", got)
	}
}

func TestParseLate(t *testing.T) {
	if got := parseLate("sent       6000 (late 12, 0.20%)\nallowed 1"); got != 12 {
		t.Errorf("parseLate = %d, want 12", got)
	}
	if got := parseLate("nothing here"); got != 0 {
		t.Errorf("parseLate = %d, want 0", got)
	}
}
