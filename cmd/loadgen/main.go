// Command loadgen sends open-loop HTTP load at the gateways and records
// every request.
//
//	loadgen -targets http://gateway:8080 -rate 200 -duration 30s -key key-1
//	loadgen -targets http://gateway:8080 -rate 2000 -duration 35s -keys 10 -out run.csv
//
// A target whose host name resolves to several addresses (the Compose
// service name of a scaled gateway) is expanded to one target per address,
// and requests go round-robin across all of them.
package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/analyze"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/loadgen"
)

type options struct {
	targets  []string
	weights  []int
	path     string
	rate     float64
	duration time.Duration
	key      string
	keys     int
	dist     string
	shape    string
	idle     time.Duration
	burst    float64
	burstFor time.Duration
	timeout  time.Duration
	out      string
	seed     uint64
}

func main() {
	o, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, o, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}

func parseFlags(args []string) (options, error) {
	var (
		o       options
		targets string
		weights string
	)
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	fs.StringVar(&targets, "targets", "http://localhost:8080", "comma-separated gateway base URLs")
	fs.StringVar(&weights, "weights", "", "comma-separated share of requests per target, after DNS expansion (default equal)")
	fs.StringVar(&o.path, "path", "/api/hello", "request path")
	fs.Float64Var(&o.rate, "rate", 200, "total requests per second, across all targets")
	fs.DurationVar(&o.duration, "duration", 30*time.Second, "length of the run (steady shape)")
	fs.StringVar(&o.key, "key", "", "send every request with this X-API-Key (overrides -keys)")
	fs.IntVar(&o.keys, "keys", 1, "number of distinct API keys: key-0, key-1, ...")
	fs.StringVar(&o.dist, "dist", "uniform", "how requests spread over keys: uniform, zipf or hot (90% on key-0)")
	fs.StringVar(&o.shape, "shape", "steady", "steady, or burst: silence for -idle, then -burst-rate for -burst-for")
	fs.DurationVar(&o.idle, "idle", 5*time.Second, "silence before the burst (burst shape)")
	fs.Float64Var(&o.burst, "burst-rate", 0, "requests per second during the burst (default 10 × -rate)")
	fs.DurationVar(&o.burstFor, "burst-for", time.Second, "length of the burst (burst shape)")
	fs.DurationVar(&o.timeout, "timeout", 5*time.Second, "per-request timeout")
	fs.StringVar(&o.out, "out", "", "write every request to this CSV file")
	fs.Uint64Var(&o.seed, "seed", 1, "seed for choosing keys, so runs are repeatable")
	if err := fs.Parse(args); err != nil {
		return o, err
	}

	o.targets = splitList(targets)
	if len(o.targets) == 0 {
		return o, errors.New("-targets is empty")
	}
	for _, w := range splitList(weights) {
		n, err := strconv.Atoi(w)
		if err != nil || n < 0 {
			return o, fmt.Errorf("-weights: %q is not a whole number of zero or more", w)
		}
		o.weights = append(o.weights, n)
	}
	if o.rate <= 0 {
		return o, errors.New("-rate must be positive")
	}
	if o.keys < 1 {
		return o, errors.New("-keys must be at least 1")
	}
	switch o.dist {
	case "uniform", "zipf", "hot":
	default:
		return o, fmt.Errorf("-dist %q: want uniform, zipf or hot", o.dist)
	}
	switch o.shape {
	case "steady":
		if o.duration <= 0 {
			return o, errors.New("-duration must be positive")
		}
	case "burst":
		if o.burst == 0 {
			o.burst = 10 * o.rate
		}
		if o.burstFor <= 0 || o.idle < 0 || o.burst < 0 {
			return o, errors.New("burst shape needs -burst-for > 0, -idle >= 0 and -burst-rate >= 0")
		}
	default:
		return o, fmt.Errorf("-shape %q: want steady or burst", o.shape)
	}
	return o, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// target is one address requests are sent to.
type target struct {
	base *url.URL // scheme and host:port to dial
	host string   // the Host header: the name the user gave
}

// expandTargets resolves each target's host name and returns one target
// per address. lookup is net.LookupHost outside tests.
func expandTargets(raw []string, lookup func(string) ([]string, error)) ([]target, error) {
	var out []target
	for _, r := range raw {
		u, err := url.Parse(r)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("target %q is not an http(s) URL", r)
		}
		host, port := u.Hostname(), u.Port()
		if net.ParseIP(host) != nil {
			out = append(out, target{base: u, host: u.Host})
			continue
		}
		addrs, err := lookup(host)
		if err != nil {
			return nil, fmt.Errorf("resolving %s: %w", host, err)
		}
		sort.Strings(addrs) // a stable order makes runs comparable
		for _, a := range addrs {
			b := *u
			b.Host = a
			if port != "" {
				b.Host = net.JoinHostPort(a, port)
			} else if strings.Contains(a, ":") {
				b.Host = "[" + a + "]"
			}
			out = append(out, target{base: &b, host: u.Host})
		}
	}
	return out, nil
}

// spread returns, for a run of n requests, which target each one goes to:
// round-robin, with each target repeated by its weight.
func spread(n int64, targets int, weights []int) ([]int, error) {
	var order []int
	if len(weights) == 0 {
		for i := range targets {
			order = append(order, i)
		}
	} else {
		if len(weights) != targets {
			return nil, fmt.Errorf("-weights has %d values but there are %d targets after DNS expansion", len(weights), targets)
		}
		for i, w := range weights {
			for range w {
				order = append(order, i)
			}
		}
		if len(order) == 0 {
			return nil, errors.New("-weights are all zero")
		}
	}
	out := make([]int, n)
	for i := range out {
		out[i] = order[i%len(order)]
	}
	return out, nil
}

// chooseKeys returns the API key for each of n requests.
func chooseKeys(n int64, o options) []string {
	out := make([]string, n)
	if o.key != "" {
		for i := range out {
			out[i] = o.key
		}
		return out
	}
	names := make([]string, o.keys)
	for i := range names {
		names[i] = "key-" + strconv.Itoa(i)
	}
	rng := rand.New(rand.NewPCG(o.seed, o.seed^0x9e3779b97f4a7c15))
	var zipf *rand.Zipf
	if o.dist == "zipf" && o.keys > 1 {
		zipf = rand.NewZipf(rng, 1.1, 1, uint64(o.keys-1))
	}
	for i := range out {
		switch {
		case o.keys == 1:
			out[i] = names[0]
		case o.dist == "zipf":
			out[i] = names[zipf.Uint64()]
		case o.dist == "hot":
			if rng.Float64() < 0.9 {
				out[i] = names[0]
			} else {
				out[i] = names[1+rng.IntN(o.keys-1)]
			}
		default:
			out[i] = names[rng.IntN(o.keys)]
		}
	}
	return out
}

// segment is one stretch of constant rate.
type segment struct {
	rate float64
	dur  time.Duration
}

func segments(o options) []segment {
	if o.shape == "burst" {
		return []segment{{0, o.idle}, {o.burst, o.burstFor}}
	}
	return []segment{{o.rate, o.duration}}
}

type result struct {
	send    time.Time
	latency time.Duration
	status  int
	gateway string
}

func run(ctx context.Context, o options, stdout io.Writer) error {
	targets, err := expandTargets(o.targets, func(host string) ([]string, error) {
		return net.DefaultResolver.LookupHost(ctx, host)
	})
	if err != nil {
		return err
	}
	for _, t := range targets {
		fmt.Fprintf(os.Stderr, "target %s (Host: %s)\n", t.base, t.host)
	}

	segs := segments(o)
	var total int64
	for _, s := range segs {
		total += loadgen.Schedule{Rate: max(s.rate, 1e-9)}.Count(s.dur)
	}
	keys := chooseKeys(total, o)
	which, err := spread(total, len(targets), o.weights)
	if err != nil {
		return err
	}

	client := &http.Client{
		Timeout: o.timeout,
		Transport: &http.Transport{
			Proxy:               nil,
			MaxIdleConns:        4096,
			MaxIdleConnsPerHost: 4096,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		},
	}
	results := make([]result, total)
	send := func(seq int64, scheduled time.Time) {
		t := targets[which[seq]]
		u := *t.base
		u.Path = o.path
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			results[seq] = result{send: scheduled}
			return
		}
		req.Host = t.host
		req.Header.Set("X-API-Key", keys[seq])
		resp, err := client.Do(req)
		r := result{send: scheduled, latency: time.Since(scheduled)}
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			r.latency = time.Since(scheduled)
			r.status = resp.StatusCode
			r.gateway = resp.Header.Get("X-Gateway-ID")
		}
		results[seq] = r
	}

	var (
		stats  loadgen.Stats
		offset int64
		start  = time.Now()
	)
	for _, s := range segs {
		if s.rate == 0 {
			select {
			case <-time.After(s.dur):
			case <-ctx.Done():
			}
			start = start.Add(s.dur)
			continue
		}
		sched := loadgen.Schedule{Start: start, Rate: s.rate}
		base := offset
		st := loadgen.Run(ctx, sched, s.dur, loadgen.SystemTimer{}, func(seq int64, at time.Time) { send(base+seq, at) })
		stats.Sent += st.Sent
		stats.Late += st.Late
		offset += sched.Count(s.dur)
		start = start.Add(s.dur)
	}
	results = results[:offset]

	summarise(stdout, results, stats)
	if o.out != "" {
		if err := writeCSV(o.out, results, keys); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "wrote      %s\n", o.out)
	}
	return nil
}

func summarise(w io.Writer, results []result, stats loadgen.Stats) {
	var allowed, rejected, errs int
	perGateway := map[string]int{}
	var lat []time.Duration
	for _, r := range results {
		switch {
		case r.send.IsZero():
			continue // never sent (run cancelled)
		case r.status >= 200 && r.status < 400:
			allowed++
		case r.status == http.StatusTooManyRequests:
			rejected++
		default:
			errs++
		}
		if r.gateway != "" {
			perGateway[r.gateway]++
		}
		lat = append(lat, r.latency)
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) time.Duration {
		if len(lat) == 0 {
			return 0
		}
		i := int(float64(len(lat))*p/100+0.999999) - 1
		return lat[max(0, min(i, len(lat)-1))]
	}

	fmt.Fprintf(w, "sent       %d (late %d, %.2f%%)\n", stats.Sent, stats.Late, 100*float64(stats.Late)/float64(max(1, stats.Sent)))
	fmt.Fprintf(w, "allowed    %d\nrejected   %d\nerrors     %d\n", allowed, rejected, errs)
	fmt.Fprintf(w, "latency    p50 %v  p95 %v  p99 %v\n", pct(50).Round(time.Microsecond), pct(95).Round(time.Microsecond), pct(99).Round(time.Microsecond))
	ids := make([]string, 0, len(perGateway))
	for id := range perGateway {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Fprintf(w, "gateway    %s: %d\n", id, perGateway[id])
	}
	n := float64(max(1, stats.Sent))
	if float64(stats.Late)/n > 0.01 {
		fmt.Fprintln(w, "WARNING: more than 1% of sends were late; this machine cannot sustain the rate, so do not trust this run")
	}
	if float64(errs)/n > 0.01 {
		fmt.Fprintln(w, "NOTE: more than 1% of requests got neither 2xx nor 429; expected during a fault run, a problem otherwise")
	}
}

func writeCSV(path string, results []result, keys []string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write(analyze.Header); err != nil {
		return err
	}
	for seq, r := range results {
		if r.send.IsZero() {
			continue
		}
		if err := w.Write([]string{
			strconv.Itoa(seq), keys[seq], strconv.FormatInt(r.send.UnixNano(), 10),
			strconv.FormatInt(int64(r.latency), 10), strconv.Itoa(r.status), r.gateway,
		}); err != nil {
			return err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	return f.Close()
}
