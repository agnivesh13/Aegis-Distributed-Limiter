// Command analyze reads one load-generator CSV, works out the run's
// accuracy and latency, prints them, and appends one row to the summary.
//
//	go run ./cmd/analyze -in bench/results/raw/S1-gossip-1.csv \
//	    -scenario S1 -backend gossip -rep 1 -gateways 3
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/analyze"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "analyze:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		in        = flag.String("in", "", "the load generator's CSV for one run (required)")
		out       = flag.String("out", "bench/results/summary.csv", "summary file to append to; empty to only print")
		rate      = flag.Int("rate", 100, "the limit: requests per period, per key")
		period    = flag.Duration("period", time.Second, "the limit's period")
		algorithm = flag.String("algorithm", string(limiter.SlidingWindow), "the limit's algorithm")
		burst     = flag.Int("burst", 0, "the limit's burst (token bucket only)")
		cost      = flag.Int("cost", 1, "what each request cost")
		warmup    = flag.Duration("warmup", 5*time.Second, "time discarded from the start of the run")
		scenario  = flag.String("scenario", "", "scenario ID, for the summary row")
		backend   = flag.String("backend", "", "limiter backend, for the summary row")
		rep       = flag.Int("rep", 1, "which repetition this run is")
		gateways  = flag.Int("gateways", 1, "how many gateways served the run")
		late      = flag.Int64("late", 0, "late sends reported by the load generator")
		note      = flag.String("note", "", "free text for the summary row")
	)
	flag.Parse()
	if *in == "" {
		flag.Usage()
		return fmt.Errorf("-in is required")
	}

	f, err := os.Open(*in)
	if err != nil {
		return err
	}
	defer f.Close()
	requests, err := analyze.ReadCSV(f)
	if err != nil {
		return err
	}

	res, err := analyze.Analyze(requests, analyze.Config{
		Limit:  limiter.Limit{Algorithm: limiter.Algorithm(*algorithm), Rate: *rate, Period: *period, Burst: *burst},
		Cost:   *cost,
		Warmup: *warmup,
	})
	if err != nil {
		return err
	}

	fmt.Printf("measured     %.1f s, %d requests over %d keys\n", res.MeasuredFor.Seconds(), res.Requests, res.Keys)
	fmt.Printf("admitted     %d   rejected %d   errors %d   late sends %d\n", res.Admitted, res.Rejected, res.Errors, *late)
	fmt.Printf("ideal        %d\n", res.Ideal)
	fmt.Printf("over-admit   %.2f%% overall, %.2f%% on the worst key\n", res.OverAdmissionPct, res.WorstKeyOverPct)
	fmt.Printf("false reject %.2f%%\n", res.FalseRejectPct)
	fmt.Printf("peak window  %.2fx the limit\n", res.PeakWindowRatio)
	fmt.Printf("latency      p50 %v   p95 %v   p99 %v\n", res.P50, res.P95, res.P99)

	// The plan's sizing rule: above 1% the machine could not sustain the run.
	if n := float64(res.Requests); float64(res.Errors)/n > 0.01 || float64(*late)/n > 0.01 {
		fmt.Println("WARNING: errors or late sends exceed 1% of requests; lower the rate and do not trust this run")
	}

	if *out == "" {
		return nil
	}
	return analyze.AppendSummary(*out, analyze.Run{
		Scenario: *scenario, Backend: *backend, Rep: *rep, Gateways: *gateways, LateSends: *late, Note: *note,
	}, res)
}
