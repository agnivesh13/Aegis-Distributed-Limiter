// Package analyze turns one load-generator run into the numbers the report
// needs: how many requests were admitted, how many an ideal limiter would
// have admitted, and how long requests took.
package analyze

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter/memory"
)

// Request is one row of the load generator's CSV.
type Request struct {
	Seq       int64
	Key       string
	Send      time.Time     // when the request was scheduled to leave
	Latency   time.Duration // client-side time to the response
	Status    int           // HTTP status; 0 when the client got no response
	GatewayID string
}

// Admitted reports whether the gateway let the request through.
func (r Request) Admitted() bool { return r.Status >= 200 && r.Status < 400 }

// Rejected reports whether the gateway rate limited the request.
func (r Request) Rejected() bool { return r.Status == 429 }

// Config says how a run was limited and which part of it to measure.
type Config struct {
	Limit  limiter.Limit
	Cost   int           // what each request cost; 0 means 1
	Warmup time.Duration // discarded from the start of the run
}

// Result is one row of summary.csv.
type Result struct {
	Requests int // in the measured window
	Admitted int
	Rejected int
	Errors   int // neither admitted nor rate limited
	Ideal    int // what a single exact limiter admits for the same requests

	OverAdmissionPct  float64 // admitted beyond ideal, as % of ideal
	FalseRejectPct    float64 // ideal beyond admitted, as % of ideal
	WorstKeyOverPct   float64 // the highest over-admission % of any one key
	PeakWindowRatio   float64 // most admitted for one key in any span of one Period, over the limit
	P50, P95, P99     time.Duration
	MeasuredFor       time.Duration
	Keys              int
	FirstSend, Cutoff time.Time
}

// replayClock is the clock the reference limiter reads during a replay.
type replayClock struct{ now time.Time }

func (c *replayClock) Now() time.Time { return c.now }

// Analyze follows "How accuracy is computed" in the plan.
//
// The reference limiter replays the whole run, warm-up included, and only
// its decisions inside the measured window are counted. Starting it cold at
// the cutoff would hand it an empty previous window that the real gateways
// did not have, and bias the first second of every run.
func Analyze(requests []Request, cfg Config) (Result, error) {
	if len(requests) == 0 {
		return Result{}, errors.New("analyze: the run has no requests")
	}
	cost := cfg.Cost
	if cost == 0 {
		cost = 1
	}

	// 1. Order by send time.
	reqs := append([]Request(nil), requests...)
	sort.SliceStable(reqs, func(i, j int) bool { return reqs[i].Send.Before(reqs[j].Send) })
	cutoff := reqs[0].Send.Add(cfg.Warmup)

	// 2. Replay through the reference: the memory backend on a clock set
	// to each request's send time.
	clock := &replayClock{}
	ref := memory.New(clock, memory.WithJanitorInterval(time.Hour))
	defer ref.Close()

	type perKey struct {
		admitted, ideal int
		admittedAt      []time.Time
	}
	keys := make(map[string]*perKey)
	res := Result{FirstSend: reqs[0].Send, Cutoff: cutoff}
	var latencies []time.Duration

	for _, r := range reqs {
		clock.now = r.Send
		d, err := ref.Allow(context.Background(), r.Key, cfg.Limit, cost)
		if err != nil {
			return Result{}, fmt.Errorf("analyze: reference limiter: %w", err)
		}
		if r.Send.Before(cutoff) {
			continue
		}

		k := keys[r.Key]
		if k == nil {
			k = &perKey{}
			keys[r.Key] = k
		}
		res.Requests++
		latencies = append(latencies, r.Latency)
		if d.Allowed {
			k.ideal++
		}
		switch {
		case r.Admitted():
			res.Admitted++
			k.admitted++
			k.admittedAt = append(k.admittedAt, r.Send)
		case r.Rejected():
			res.Rejected++
		default:
			res.Errors++
		}
	}
	if res.Requests == 0 {
		return Result{}, fmt.Errorf("analyze: no requests after the %v warm-up", cfg.Warmup)
	}
	res.Keys = len(keys)
	res.MeasuredFor = reqs[len(reqs)-1].Send.Sub(cutoff)

	// 3 and 4. Over-admission and false rejects, overall and per key.
	for _, k := range keys {
		res.Ideal += k.ideal
		res.WorstKeyOverPct = math.Max(res.WorstKeyOverPct, pctOfIdeal(k.admitted-k.ideal, k.ideal))

		// 5. Peak: the most admissions inside any span of one Period.
		// Each admission counts cost against the limit.
		res.PeakWindowRatio = math.Max(res.PeakWindowRatio,
			float64(peakInSpan(k.admittedAt, cfg.Limit.Period)*cost)/float64(cfg.Limit.Rate))
	}
	res.OverAdmissionPct = pctOfIdeal(res.Admitted-res.Ideal, res.Ideal)
	res.FalseRejectPct = pctOfIdeal(res.Ideal-res.Admitted, res.Ideal)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	res.P50 = Percentile(latencies, 50)
	res.P95 = Percentile(latencies, 95)
	res.P99 = Percentile(latencies, 99)
	return res, nil
}

// pctOfIdeal returns diff as a percentage of ideal, or 0 when diff is not
// positive. Both error measures are stated against the ideal count:
// over-admission is (admitted - ideal) / ideal, and false rejects are
// (ideal - admitted) / ideal.
func pctOfIdeal(diff, ideal int) float64 {
	if diff <= 0 || ideal == 0 {
		return 0
	}
	return float64(diff) / float64(ideal) * 100
}

// peakInSpan returns the most times that fall inside any half-open span
// [t, t+span). times must be in ascending order.
func peakInSpan(times []time.Time, span time.Duration) int {
	peak, lo := 0, 0
	for hi, t := range times {
		for t.Sub(times[lo]) >= span {
			lo++
		}
		peak = max(peak, hi-lo+1)
	}
	return peak
}

// Percentile returns the p-th percentile of sorted by the nearest-rank
// method: the smallest value with at least p% of the values at or below it.
// The load generator uses it too, so both tools print the same figure for
// the same run.
func Percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	return sorted[max(rank, 1)-1]
}

// Header is the first line of a load-generator CSV.
var Header = []string{"seq", "key", "send_unix_ns", "latency_ns", "status", "gateway_id"}

// ReadCSV reads a load-generator run.
func ReadCSV(r io.Reader) ([]Request, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = len(Header)
	head, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("analyze: reading the header: %w", err)
	}
	for i, name := range Header {
		if head[i] != name {
			return nil, fmt.Errorf("analyze: column %d is %q, want %q", i+1, head[i], name)
		}
	}

	var requests []Request
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if err == io.EOF {
			return requests, nil
		}
		if err != nil {
			return nil, fmt.Errorf("analyze: line %d: %w", line, err)
		}
		seq, err1 := strconv.ParseInt(rec[0], 10, 64)
		send, err2 := strconv.ParseInt(rec[2], 10, 64)
		latency, err3 := strconv.ParseInt(rec[3], 10, 64)
		status, err4 := strconv.Atoi(rec[4])
		if err := errors.Join(err1, err2, err3, err4); err != nil {
			return nil, fmt.Errorf("analyze: line %d: %w", line, err)
		}
		requests = append(requests, Request{
			Seq:       seq,
			Key:       rec[1],
			Send:      time.Unix(0, send),
			Latency:   time.Duration(latency),
			Status:    status,
			GatewayID: rec[5],
		})
	}
}

// Run identifies the run a Result belongs to in summary.csv.
type Run struct {
	Scenario  string
	Backend   string
	Rep       int // which repetition of the scenario
	Gateways  int
	LateSends int64 // from the load generator's own count
	Note      string
}

// SummaryHeader is the first line of summary.csv.
var SummaryHeader = []string{
	"scenario", "backend", "rep", "gateways", "keys", "measured_s",
	"requests", "admitted", "rejected", "errors", "late_sends", "ideal",
	"over_admission_pct", "false_reject_pct", "worst_key_over_pct", "peak_window_ratio",
	"p50_ms", "p95_ms", "p99_ms", "note",
}

// AppendSummary adds one row to the summary file at path, creating the file
// with its header when it does not exist yet.
func AppendSummary(path string, run Run, res Result) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}

	w := csv.NewWriter(f)
	if info.Size() == 0 {
		if err := w.Write(SummaryHeader); err != nil {
			return err
		}
	}
	ms := func(d time.Duration) string {
		return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 3, 64)
	}
	pct := func(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }
	row := []string{
		run.Scenario, run.Backend, strconv.Itoa(run.Rep), strconv.Itoa(run.Gateways),
		strconv.Itoa(res.Keys), strconv.FormatFloat(res.MeasuredFor.Seconds(), 'f', 1, 64),
		strconv.Itoa(res.Requests), strconv.Itoa(res.Admitted), strconv.Itoa(res.Rejected),
		strconv.Itoa(res.Errors), strconv.FormatInt(run.LateSends, 10), strconv.Itoa(res.Ideal),
		pct(res.OverAdmissionPct), pct(res.FalseRejectPct), pct(res.WorstKeyOverPct), pct(res.PeakWindowRatio),
		ms(res.P50), ms(res.P95), ms(res.P99), run.Note,
	}
	if err := w.Write(row); err != nil {
		return err
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	return f.Close()
}
