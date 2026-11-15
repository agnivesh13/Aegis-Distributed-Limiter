package analyze

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter/memory"
)

var (
	start = time.Unix(1_700_000_000, 0)
	limit = limiter.Limit{Algorithm: limiter.SlidingWindow, Rate: 100, Period: time.Second}
)

// run builds a load-generator log: ratePerKey requests per second for each
// of keys keys, for d, with every status chosen by decide.
func run(keys int, ratePerKey float64, d time.Duration, decide func(r Request) int) []Request {
	total := int(ratePerKey * float64(keys) * d.Seconds())
	gap := time.Duration(float64(time.Second) / (ratePerKey * float64(keys)))
	requests := make([]Request, total)
	for i := range requests {
		r := Request{
			Seq:       int64(i),
			Key:       fmt.Sprintf("api:key-%d", i%keys),
			Send:      start.Add(time.Duration(i) * gap),
			Latency:   time.Millisecond,
			GatewayID: fmt.Sprintf("gateway-%d", i%3),
		}
		r.Status = decide(r)
		requests[i] = r
	}
	return requests
}

// exactGateway answers the way a perfect limiter would.
func exactGateway(t *testing.T) func(Request) int {
	clock := &replayClock{}
	lim := memory.New(clock, memory.WithJanitorInterval(time.Hour))
	t.Cleanup(func() { lim.Close() })
	return func(r Request) int {
		clock.now = r.Send
		d, err := lim.Allow(context.Background(), r.Key, limit, 1)
		if err != nil {
			t.Fatalf("Allow: %v", err)
		}
		if d.Allowed {
			return 200
		}
		return 429
	}
}

// A run produced by an exact limiter must score zero on both error
// measures. This holds only because the reference replays the warm-up too:
// started cold at the cutoff it would admit extra requests in the first
// window and report false rejects that never happened.
func TestExactRunScoresZero(t *testing.T) {
	requests := run(10, 200, 35*time.Second, exactGateway(t))
	res, err := Analyze(requests, Config{Limit: limit, Warmup: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.OverAdmissionPct != 0 || res.FalseRejectPct != 0 || res.WorstKeyOverPct != 0 {
		t.Errorf("over = %v%%, false rejects = %v%%, worst key = %v%%; want all 0", res.OverAdmissionPct, res.FalseRejectPct, res.WorstKeyOverPct)
	}
	if res.Admitted != res.Ideal {
		t.Errorf("admitted %d, ideal %d; want them equal", res.Admitted, res.Ideal)
	}
	if res.Requests != 60_000 || res.Keys != 10 || res.Errors != 0 {
		t.Errorf("requests = %d, keys = %d, errors = %d; want 60000, 10, 0", res.Requests, res.Keys, res.Errors)
	}
	// 2x overload for 30 s at 100 per second per key: about 3,000 per key.
	if res.Admitted < 29_500 || res.Admitted > 30_500 {
		t.Errorf("admitted %d, want about 30000", res.Admitted)
	}
	if res.PeakWindowRatio > 1 {
		t.Errorf("peak window ratio = %v, want at most 1 for an exact limiter", res.PeakWindowRatio)
	}
}

// A gateway that admits everything at twice the limit is 100% over.
func TestAdmitEverythingAtDoubleLoad(t *testing.T) {
	requests := run(4, 200, 35*time.Second, func(Request) int { return 200 })
	res, err := Analyze(requests, Config{Limit: limit, Warmup: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.OverAdmissionPct < 95 || res.OverAdmissionPct > 105 {
		t.Errorf("over-admission = %.1f%%, want about 100%%", res.OverAdmissionPct)
	}
	if res.FalseRejectPct != 0 {
		t.Errorf("false rejects = %v%%, want 0", res.FalseRejectPct)
	}
	if res.PeakWindowRatio != 2 {
		t.Errorf("peak window ratio = %v, want 2 (200 admitted in one second against a limit of 100)", res.PeakWindowRatio)
	}
}

// A gateway that rejects everything has 100% false rejects.
func TestRejectEverything(t *testing.T) {
	requests := run(2, 50, 10*time.Second, func(Request) int { return 429 })
	res, err := Analyze(requests, Config{Limit: limit})
	if err != nil {
		t.Fatal(err)
	}
	if res.FalseRejectPct != 100 || res.OverAdmissionPct != 0 {
		t.Errorf("false rejects = %v%%, over = %v%%; want 100, 0", res.FalseRejectPct, res.OverAdmissionPct)
	}
	if res.Admitted != 0 || res.Rejected != res.Requests {
		t.Errorf("admitted %d, rejected %d of %d", res.Admitted, res.Rejected, res.Requests)
	}
}

// One key far over the limit must show up even when the overall figure is
// diluted by many well-behaved keys.
func TestWorstKeyIsReported(t *testing.T) {
	exact := exactGateway(t)
	requests := run(10, 200, 10*time.Second, func(r Request) int {
		if r.Key == "api:key-3" {
			return 200 // this key alone is never limited
		}
		return exact(r)
	})
	res, err := Analyze(requests, Config{Limit: limit})
	if err != nil {
		t.Fatal(err)
	}
	if res.WorstKeyOverPct < 90 {
		t.Errorf("worst key over-admission = %.1f%%, want about 100%%", res.WorstKeyOverPct)
	}
	if res.OverAdmissionPct < 8 || res.OverAdmissionPct > 12 {
		t.Errorf("overall over-admission = %.1f%%, want about 10%% (one key in ten)", res.OverAdmissionPct)
	}
}

func TestWarmupIsDiscarded(t *testing.T) {
	cutoff := start.Add(5 * time.Second)
	requests := run(1, 50, 10*time.Second, func(r Request) int {
		if r.Send.Before(cutoff) {
			return 500 // chaos during warm-up must not reach the result
		}
		return 200
	})
	res, err := Analyze(requests, Config{Limit: limit, Warmup: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors != 0 || res.Requests != 250 {
		t.Errorf("errors = %d, requests = %d; want 0 and 250 (the second half only)", res.Errors, res.Requests)
	}
}

func TestErrorsAreCountedApart(t *testing.T) {
	requests := run(1, 10, 3*time.Second, func(r Request) int {
		return []int{200, 429, 503, 0, 502}[r.Seq%5]
	})
	res, err := Analyze(requests, Config{Limit: limit})
	if err != nil {
		t.Fatal(err)
	}
	if res.Admitted != 6 || res.Rejected != 6 || res.Errors != 18 {
		t.Errorf("admitted %d, rejected %d, errors %d; want 6, 6, 18", res.Admitted, res.Rejected, res.Errors)
	}
}

func TestPeakInSpan(t *testing.T) {
	at := func(ms ...int) []time.Time {
		times := make([]time.Time, len(ms))
		for i, m := range ms {
			times[i] = start.Add(time.Duration(m) * time.Millisecond)
		}
		return times
	}
	for _, tc := range []struct {
		name  string
		times []time.Time
		want  int
	}{
		{"none", nil, 0},
		{"one", at(0), 1},
		{"exactly one span apart does not share a span", at(0, 1000), 1},
		{"just inside one span", at(0, 999), 2},
		{"the densest span is in the middle", at(0, 900, 1000, 1100, 1899, 3000), 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := peakInSpan(tc.times, time.Second); got != tc.want {
				t.Errorf("peakInSpan = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPercentile(t *testing.T) {
	var sorted []time.Duration
	for ms := 1; ms <= 100; ms++ {
		sorted = append(sorted, time.Duration(ms)*time.Millisecond)
	}
	for p, want := range map[float64]time.Duration{50: 50 * time.Millisecond, 95: 95 * time.Millisecond, 99: 99 * time.Millisecond, 100: 100 * time.Millisecond} {
		if got := Percentile(sorted, p); got != want {
			t.Errorf("p%v = %v, want %v", p, got, want)
		}
	}
	if got := Percentile([]time.Duration{7}, 99); got != 7 {
		t.Errorf("p99 of one value = %v, want that value", got)
	}
	if got := Percentile(nil, 50); got != 0 {
		t.Errorf("p50 of nothing = %v, want 0", got)
	}
}

func TestReadCSV(t *testing.T) {
	const good = `seq,key,send_unix_ns,latency_ns,status,gateway_id
0,api:key-1,1700000000000000000,1500000,200,gateway-1
1,api:key-2,1700000000005000000,900000,429,gateway-2
`
	requests, err := ReadCSV(strings.NewReader(good))
	if err != nil {
		t.Fatal(err)
	}
	want := Request{Seq: 1, Key: "api:key-2", Send: start.Add(5 * time.Millisecond), Latency: 900 * time.Microsecond, Status: 429, GatewayID: "gateway-2"}
	if len(requests) != 2 || requests[1] != want {
		t.Errorf("got %+v, want the second row to be %+v", requests, want)
	}

	for name, bad := range map[string]string{
		"wrong header":    "seq,key,sent,latency_ns,status,gateway_id\n",
		"missing column":  "seq,key,send_unix_ns,latency_ns,status\n",
		"not a number":    "seq,key,send_unix_ns,latency_ns,status,gateway_id\n0,k,soon,1,200,g\n",
		"empty file":      "",
		"too few columns": "seq,key,send_unix_ns,latency_ns,status,gateway_id\n0,k,1\n",
	} {
		if _, err := ReadCSV(strings.NewReader(bad)); err == nil {
			t.Errorf("%s: accepted, want an error", name)
		}
	}
}

func TestAppendSummaryWritesTheHeaderOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.csv")
	res := Result{Requests: 60000, Admitted: 3210, Rejected: 56790, Ideal: 3000, OverAdmissionPct: 7, Keys: 10,
		MeasuredFor: 30 * time.Second, P99: 2500 * time.Microsecond}
	for rep := 1; rep <= 2; rep++ {
		if err := AppendSummary(path, Run{Scenario: "S1", Backend: "gossip", Rep: rep, Gateways: 3}, res); err != nil {
			t.Fatal(err)
		}
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("summary has %d lines, want 3 (one header, two runs)", len(rows))
	}
	col := func(row []string, name string) string {
		for i, h := range rows[0] {
			if h == name {
				return row[i]
			}
		}
		t.Fatalf("no column %q", name)
		return ""
	}
	if got := col(rows[2], "rep"); got != "2" {
		t.Errorf("second run's rep = %q, want 2", got)
	}
	if got := col(rows[1], "over_admission_pct"); got != "7.000" {
		t.Errorf("over_admission_pct = %q, want 7.000", got)
	}
	if got := col(rows[1], "p99_ms"); got != "2.500" {
		t.Errorf("p99_ms = %q, want 2.500", got)
	}
}

func TestAnalyzeRefusesAnEmptyRun(t *testing.T) {
	if _, err := Analyze(nil, Config{Limit: limit}); err == nil {
		t.Error("an empty run was accepted")
	}
	short := run(1, 10, time.Second, func(Request) int { return 200 })
	if _, err := Analyze(short, Config{Limit: limit, Warmup: time.Minute}); err == nil {
		t.Error("a run shorter than its warm-up was accepted")
	}
}
