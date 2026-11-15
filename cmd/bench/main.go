// Command bench runs experiment scenarios end to end: for every run it
// starts a fresh cluster, waits for it, sends the load, injects any fault on
// schedule, scrapes the gateways, analyses the run, appends a row to the
// summary and tears the cluster down.
//
//	go run ./cmd/bench -scenario S1
//	go run ./cmd/bench -scenario S4,S5 -reps 1
//	go run ./cmd/bench -scenario all -dry-run
//
// Run it from the repository root, with Docker running.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/analyze"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
)

// loadLead is how far ahead the load is told to start, leaving the load
// generator's container time to come up.
const loadLead = 5 * time.Second

type options struct {
	scenarios string
	file      string
	summary   string
	rawDir    string
	compose   string
	project   string
	reps      int
	cooldown  time.Duration
	dryRun    bool
	noBuild   bool
}

func main() {
	var o options
	flag.StringVar(&o.scenarios, "scenario", "", "scenario IDs to run, comma-separated, or all (required)")
	flag.StringVar(&o.file, "file", "bench/scenarios.yaml", "scenario file")
	flag.StringVar(&o.summary, "summary", "bench/results/summary.csv", "summary file to append rows to")
	flag.StringVar(&o.rawDir, "raw", "bench/results/raw", "folder for raw CSVs and metric snapshots")
	flag.StringVar(&o.compose, "compose", "deploy/docker-compose.yml", "Compose file")
	flag.StringVar(&o.project, "project", "aegis-bench", "Compose project name")
	flag.IntVar(&o.reps, "reps", 0, "repetitions per point, overriding the scenario file")
	flag.DurationVar(&o.cooldown, "cooldown", 10*time.Second, "pause between runs so the machine cools down")
	flag.BoolVar(&o.dryRun, "dry-run", false, "print the runs without starting anything")
	flag.BoolVar(&o.noBuild, "no-build", false, "use the existing image instead of building it first")
	flag.Parse()
	if o.scenarios == "" {
		flag.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, o); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, o options) error {
	f, err := os.Open(o.file)
	if err != nil {
		return err
	}
	file, err := Load(f)
	f.Close()
	if err != nil {
		return err
	}
	jobs, err := file.Jobs(strings.Split(o.scenarios, ","), o.reps)
	if err != nil {
		return err
	}

	var total time.Duration
	for _, j := range jobs {
		total += j.Run.Duration + o.cooldown + 20*time.Second // start-up and tear-down
	}
	fmt.Printf("%d runs, roughly %v\n", len(jobs), total.Round(time.Minute))
	if o.dryRun {
		for _, j := range jobs {
			fmt.Printf("  %-40s gateways=%d  %s\n", j.Name(), j.Run.Gateways, strings.Join(j.LoadgenArgs("<csv>", time.Time{}), " "))
		}
		return nil
	}

	if err := os.MkdirAll(o.rawDir, 0o755); err != nil {
		return err
	}
	c := compose{file: o.compose, project: o.project}
	// Leftovers of an interrupted bench run would hold the ports; remove
	// them, then check nothing else does.
	if _, err := c.run(ctx, nil, "--profile", "faults", "down", "--remove-orphans"); err != nil {
		return err
	}
	if busy := busyPorts(hostPorts); len(busy) > 0 {
		return fmt.Errorf("host ports %v are in use, probably by a dev cluster; stop it with "+
			"`docker compose -f deploy/docker-compose.yml down` and run again", busy)
	}
	if !o.noBuild {
		fmt.Println("building the image")
		if _, err := c.run(ctx, nil, "build"); err != nil {
			return err
		}
	}

	for i, j := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Printf("\n[%d/%d] %s\n", i+1, len(jobs), j.Name())
		err := runJob(ctx, c, o, j)
		if _, downErr := c.run(context.Background(), nil, "--profile", "faults", "down", "--remove-orphans"); downErr != nil {
			fmt.Fprintln(os.Stderr, "  tearing down:", downErr)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "  FAILED:", err)
			continue // one broken run should not throw away the rest
		}
		if i < len(jobs)-1 {
			select {
			case <-time.After(o.cooldown):
			case <-ctx.Done():
			}
		}
	}
	return nil
}

// runJob does one run, start to finish, but leaves tearing down to the
// caller so it happens whatever goes wrong.
func runJob(ctx context.Context, c compose, o options, j Job) error {
	r := j.Run
	env := map[string]string{
		"LIMITER_BACKEND": j.Backend,
		"GATEWAY_CONFIG":  "gateway-bench.yaml",
		"DEBUG_FAULTS":    "0",
	}
	for k, v := range r.Env {
		env[k] = v
	}
	services := []string{"upstream", "gateway"}
	var up []string
	if j.Backend == "redis" {
		services = append(services, "redis")
	}
	if r.Fault.viaToxiproxy() {
		up = append(up, "--profile", "faults")
		services = append(services, "toxiproxy")
		env["REDIS_ADDR"] = "toxiproxy:26379"
	}
	if r.Fault.PartitionAt > 0 {
		env["DEBUG_FAULTS"] = "1"
	}

	up = append(up, "up", "-d", "--wait", "--scale", "gateway="+strconv.Itoa(r.Gateways))
	if _, err := c.run(ctx, env, append(up, services...)...); err != nil {
		return fmt.Errorf("starting the cluster: %w", err)
	}

	admins, err := c.adminAddrs(ctx, r.Gateways)
	if err != nil {
		return err
	}
	if r.Fault.viaToxiproxy() {
		if err := setRedisDelay(ctx, *r.Fault.RedisDelay); err != nil {
			return err
		}
	}
	if j.Backend == "gossip" {
		if err := waitForMembers(ctx, admins, r.Gateways); err != nil {
			return err
		}
	}

	csvName := j.Name() + ".csv"
	// The load generator's container takes a moment to start, so the load
	// is told to begin at a fixed instant a little ahead, and faults and
	// the warm-up snapshot are timed from that same instant. Containers
	// read the host's clock, so the instant means the same thing inside.
	start := time.Now().Add(loadLead)
	faults := scheduleFaults(ctx, c, env, admins, r.Fault, start)

	// Take the "before" snapshot when the warm-up ends, so the decision
	// latencies cover the same measured window as the analyzer.
	before := make(chan Scrape, 1)
	go func() {
		select {
		case <-time.After(time.Until(start.Add(*r.Warmup))):
		case <-ctx.Done():
		}
		s, _ := scrape(ctx, admins)
		before <- s
	}()

	out, err := c.run(ctx, env, append([]string{"run", "--rm", "--no-deps", "loadgen"}, j.LoadgenArgs("/results/raw/"+csvName, start)...)...)
	fmt.Print(indent(out))
	if err != nil {
		return fmt.Errorf("load generator: %w", err)
	}
	if err := <-faults; err != nil {
		fmt.Fprintln(os.Stderr, "  fault injection:", err)
	}
	after, err := scrape(ctx, admins)
	if err != nil {
		fmt.Fprintln(os.Stderr, "  final scrape (a killed gateway is expected to fail):", err)
	}
	window := Window(<-before, after)

	// Analyse.
	f, err := os.Open(filepath.Join(o.rawDir, csvName))
	if err != nil {
		return err
	}
	requests, err := analyze.ReadCSV(f)
	f.Close()
	if err != nil {
		return err
	}
	res, err := analyze.Analyze(requests, analyze.Config{
		Limit:  limiter.Limit{Algorithm: limiter.SlidingWindow, Rate: r.Limit.Rate, Period: r.Limit.Period},
		Warmup: *r.Warmup,
	})
	if err != nil {
		return err
	}

	if err := validRun(j.Backend, r.Gateways, r.Fault, window); err != nil {
		return err
	}

	measured := res.MeasuredFor.Seconds()
	note := j.Note() + fmt.Sprintf(" decision_p50_us=%.1f decision_p99_us=%.1f limiter_errors=%.0f",
		window.DecisionQuantile(0.50)*1e6, window.DecisionQuantile(0.99)*1e6, window.LimiterErrors)
	if j.Backend == "gossip" && measured > 0 {
		note += fmt.Sprintf(" gossip_bytes_per_s_per_node=%.0f", window.GossipBytes/measured/float64(r.Gateways))
	}
	late := parseLate(out)
	if err := analyze.AppendSummary(o.summary, analyze.Run{
		Scenario: j.Scenario, Backend: j.Backend, Rep: j.Rep, Gateways: r.Gateways, LateSends: late, Note: note,
	}, res); err != nil {
		return err
	}
	fmt.Printf("  over-admission %.2f%%  false rejects %.2f%%  decision p99 %.0f µs  late %d\n",
		res.OverAdmissionPct, res.FalseRejectPct, window.DecisionQuantile(0.99)*1e6, late)
	if n := float64(max(1, res.Requests)); float64(late)/n > 0.01 {
		fmt.Println("  WARNING: more than 1% late sends; lower the rate for this scenario and say so in the report")
	}
	return nil
}

// validRun refuses a run that measured a broken setup instead of a design,
// so it never reaches the summary looking like a result.
//
//   - The limiter failed when nothing was meant to make it fail. With
//     fail_open such a run lets everything through. Only a Redis outage
//     (F2) is meant to cause limiter errors.
//   - Gossip gateways never learned anything from each other. Each then
//     enforces the full limit alone, which is not the design under test
//     (for example while Store.Merge is unwritten).
func validRun(backend string, gateways int, f Fault, w Snapshot) error {
	if w.LimiterErrors > 0 && f.StopRedisAt == 0 {
		return fmt.Errorf("the limiter failed %.0f times in a run with no outage, so the result measures a broken "+
			"setup; check the gateway logs (an unwritten script shows up there at start-up). No row was written", w.LimiterErrors)
	}
	if backend == "gossip" && gateways > 1 && w.GossipMerged == 0 {
		return errors.New("the gossip gateways merged nothing from each other, so each limited alone; " +
			"is Store.Merge written? No row was written")
	}
	return nil
}

var lateRe = regexp.MustCompile(`sent\s+\d+ \(late (\d+)`)

func parseLate(loadgenOutput string) int64 {
	m := lateRe.FindStringSubmatch(loadgenOutput)
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n
}

func indent(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if strings.HasPrefix(line, "target ") || strings.Contains(line, "Container ") || line == "" {
			continue
		}
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}

// compose runs docker compose for one project.
type compose struct {
	file    string
	project string
}

func (c compose) run(ctx context.Context, env map[string]string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose", "-p", c.project, "-f", c.file}, args...)...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("docker compose %s: %w\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String(), nil
}

// hostPorts are the ports the gateway replicas publish on the host.
var hostPorts = []int{8080, 8081, 8082, 8083, 8084, 9090, 9091, 9092, 9093, 9094}

// busyPorts returns the ports something on this machine is listening on.
func busyPorts(ports []int) []int {
	var busy []int
	for _, p := range ports {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(p), 300*time.Millisecond)
		if err == nil {
			conn.Close()
			busy = append(busy, p)
		}
	}
	return busy
}

// adminAddrs returns the host address of each gateway replica's admin port.
func (c compose) adminAddrs(ctx context.Context, n int) ([]string, error) {
	var addrs []string
	for i := 1; i <= n; i++ {
		out, err := c.run(ctx, nil, "port", "--index", strconv.Itoa(i), "gateway", "9090")
		if err != nil {
			return nil, err
		}
		_, port, ok := strings.Cut(strings.TrimSpace(out), ":")
		if !ok {
			return nil, fmt.Errorf("unexpected port output %q", out)
		}
		addrs = append(addrs, "127.0.0.1:"+port)
	}
	return addrs, nil
}

func httpGet(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// scrape reads every gateway's metrics. A gateway that cannot be reached
// is left out of the result, so Window ignores it.
func scrape(ctx context.Context, admins []string) (Scrape, error) {
	out := Scrape{}
	var errs []error
	for _, a := range admins {
		body, err := httpGet(ctx, "http://"+a+"/metrics")
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s := newSnapshot()
		if err := s.Add(bytes.NewReader(body)); err != nil {
			errs = append(errs, err)
			continue
		}
		out[a] = s
	}
	return out, errors.Join(errs...)
}

// waitForMembers waits until every gossip gateway sees the whole cluster,
// so no run starts with a node still limiting alone.
func waitForMembers(ctx context.Context, admins []string, n int) error {
	want := fmt.Sprintf("gossip_members %d", n)
	deadline := time.Now().Add(60 * time.Second)
	for {
		all := true
		for _, a := range admins {
			body, err := httpGet(ctx, "http://"+a+"/metrics")
			if err != nil || !bytes.Contains(body, []byte(want)) {
				all = false
				break
			}
		}
		if all {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("gossip gateways did not all see %d members within 60 s", n)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// setRedisDelay adds a fixed latency to replies from Redis through
// toxiproxy's API. Zero removes it.
func setRedisDelay(ctx context.Context, d time.Duration) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := httpGet(ctx, "http://127.0.0.1:8474/version"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("toxiproxy's API did not come up on :8474")
		}
		time.Sleep(500 * time.Millisecond)
	}
	if d == 0 {
		return nil
	}
	body := fmt.Sprintf(`{"name":"delay","type":"latency","stream":"downstream","attributes":{"latency":%d,"jitter":0}}`, d.Milliseconds())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:8474/proxies/redis/toxics", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("toxiproxy: %s: %s", resp.Status, msg)
	}
	return nil
}

// scheduleFaults starts the run's faults at their times after start. The
// returned channel delivers the first error, or nil, once all are done.
func scheduleFaults(ctx context.Context, c compose, env map[string]string, admins []string, f Fault, start time.Time) <-chan error {
	done := make(chan error, 1)
	go func() {
		var errs []error
		wait := func(at time.Duration) bool {
			select {
			case <-time.After(time.Until(start.Add(at))):
				return true
			case <-ctx.Done():
				return false
			}
		}
		switch {
		case f.KillGatewayAt > 0:
			if wait(f.KillGatewayAt) {
				out, err := c.run(ctx, env, "ps", "-q", "gateway")
				if err == nil {
					ids := strings.Fields(out)
					if len(ids) > 0 {
						fmt.Println("  fault: killing gateway container", ids[0][:12])
						_, err = runDocker(ctx, "kill", ids[0])
					}
				}
				errs = append(errs, err)
			}
		case f.StopRedisAt > 0:
			if wait(f.StopRedisAt) {
				fmt.Println("  fault: stopping redis")
				_, err := c.run(ctx, env, "stop", "redis")
				errs = append(errs, err)
				if wait(f.StopRedisAt + f.StopFor) {
					fmt.Println("  fault: starting redis")
					_, err := c.run(ctx, env, "start", "redis")
					errs = append(errs, err)
				}
			}
		case f.PartitionAt > 0:
			if wait(f.PartitionAt) {
				ids, err := gatewayIDs(ctx, admins)
				if err != nil {
					errs = append(errs, err)
					break
				}
				half := len(admins) / 2
				fmt.Printf("  fault: partitioning %v from %v\n", ids[:half], ids[half:])
				errs = append(errs, partition(ctx, admins, ids, half, http.MethodPost))
				if wait(f.PartitionAt + f.PartitionFor) {
					fmt.Println("  fault: healing the partition")
					errs = append(errs, partition(ctx, admins, ids, half, http.MethodDelete))
				}
			}
		}
		done <- errors.Join(errs...)
	}()
	return done
}

func runDocker(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

var idRe = regexp.MustCompile(`"gateway_id":"([^"]+)"`)

func gatewayIDs(ctx context.Context, admins []string) ([]string, error) {
	var ids []string
	for _, a := range admins {
		body, err := httpGet(ctx, "http://"+a+"/healthz")
		if err != nil {
			return nil, err
		}
		m := idRe.FindSubmatch(body)
		if m == nil {
			return nil, fmt.Errorf("no gateway_id in %s/healthz", a)
		}
		ids = append(ids, string(m[1]))
	}
	return ids, nil
}

// partition blocks (POST) or unblocks (DELETE) every gateway in the first
// half from every one in the second half, in both directions.
func partition(ctx context.Context, admins, ids []string, half int, method string) error {
	var errs []error
	for i := range admins {
		for k := range admins {
			if (i < half) == (k < half) {
				continue // same side
			}
			req, _ := http.NewRequestWithContext(ctx, method, "http://"+admins[i]+"/debug/gossip/block?peer="+ids[k], nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs = append(errs, fmt.Errorf("%s %s: %s", method, admins[i], resp.Status))
			}
		}
	}
	return errors.Join(errs...)
}
