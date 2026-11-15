package main

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// File is bench/scenarios.yaml.
type File struct {
	Defaults  Run        `yaml:"defaults"`
	Scenarios []Scenario `yaml:"scenarios"`
}

type Scenario struct {
	ID       string   `yaml:"id"`
	Question string   `yaml:"question"`
	Backends []string `yaml:"backends"`
	Reps     int      `yaml:"reps"` // overrides the default when set
	Runs     []Run    `yaml:"runs"`
}

// Run is one point of a scenario. Zero fields inherit from the defaults.
type Run struct {
	Label     string            `yaml:"label"`
	Gateways  int               `yaml:"gateways"`
	Rate      float64           `yaml:"rate"`
	Keys      int               `yaml:"keys"`
	Dist      string            `yaml:"dist"`
	Shape     string            `yaml:"shape"`
	Duration  time.Duration     `yaml:"duration"`
	Idle      time.Duration     `yaml:"idle"`
	BurstRate float64           `yaml:"burst_rate"`
	BurstFor  time.Duration     `yaml:"burst_for"`
	Warmup    *time.Duration    `yaml:"warmup"` // a pointer, because 0s is a real choice
	Reps      int               `yaml:"reps"`
	Limit     Limit             `yaml:"limit"`
	Env       map[string]string `yaml:"env"`
	Fault     Fault             `yaml:"fault"`
}

type Limit struct {
	Rate   int           `yaml:"rate"`
	Period time.Duration `yaml:"period"`
}

// Fault is what to break during a run, timed from the start of the load.
type Fault struct {
	RedisDelay    *time.Duration `yaml:"redis_delay"` // set (even to 0) to route Redis through toxiproxy
	KillGatewayAt time.Duration  `yaml:"kill_gateway_at"`
	StopRedisAt   time.Duration  `yaml:"stop_redis_at"`
	StopFor       time.Duration  `yaml:"stop_for"`
	PartitionAt   time.Duration  `yaml:"partition_at"`
	PartitionFor  time.Duration  `yaml:"partition_for"`
}

func (f Fault) viaToxiproxy() bool { return f.RedisDelay != nil }

// Job is one fully resolved run of one scenario on one backend.
type Job struct {
	Scenario string
	Backend  string
	Rep      int
	Run      Run
}

// Name is used for the raw CSV and the metric snapshots.
func (j Job) Name() string {
	return fmt.Sprintf("%s-%s-%s-rep%d", j.Scenario, j.Run.Label, j.Backend, j.Rep)
}

// Note describes the run's settings in summary.csv, as key=value pairs the
// plot script can read back.
func (j Job) Note() string {
	r := j.Run
	parts := []string{
		"label=" + r.Label,
		fmt.Sprintf("rate=%g", r.Rate),
		"dist=" + r.Dist,
		"interval=" + r.Env["GOSSIP_INTERVAL"],
		"fanout=" + r.Env["GOSSIP_FANOUT"],
	}
	if r.Fault.RedisDelay != nil {
		parts = append(parts, "redis_delay="+r.Fault.RedisDelay.String())
	}
	if v := r.Env["LIMITER_ON_ERROR"]; v != "" {
		parts = append(parts, "on_error="+v)
	}
	return strings.Join(parts, " ")
}

// Load reads and checks a scenario file.
func Load(r io.Reader) (File, error) {
	var f File
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return File{}, fmt.Errorf("scenarios: %w", err)
	}
	seen := map[string]bool{}
	var errs []error
	for _, s := range f.Scenarios {
		if s.ID == "" || seen[s.ID] {
			errs = append(errs, fmt.Errorf("scenario id %q is empty or repeated", s.ID))
		}
		seen[s.ID] = true
		if len(s.Backends) == 0 || len(s.Runs) == 0 {
			errs = append(errs, fmt.Errorf("%s: needs backends and runs", s.ID))
		}
		for _, b := range s.Backends {
			if b != "memory" && b != "redis" && b != "gossip" {
				errs = append(errs, fmt.Errorf("%s: unknown backend %q", s.ID, b))
			}
		}
		for _, run := range s.Runs {
			r := f.resolve(s, run)
			if r.Label == "" || r.Gateways < 1 || r.Rate <= 0 || r.Keys < 1 || r.Limit.Rate < 1 || r.Limit.Period <= 0 || r.Reps < 1 {
				errs = append(errs, fmt.Errorf("%s/%s: label, gateways, rate, keys, limit and reps must all be set and positive", s.ID, r.Label))
			}
			if r.Shape == "steady" && r.Duration <= *r.Warmup {
				errs = append(errs, fmt.Errorf("%s/%s: duration %v leaves nothing after the %v warm-up", s.ID, r.Label, r.Duration, *r.Warmup))
			}
			if r.Fault.PartitionAt > 0 && r.Gateways < 2 {
				errs = append(errs, fmt.Errorf("%s/%s: a partition needs at least 2 gateways", s.ID, r.Label))
			}
		}
	}
	return f, errors.Join(errs...)
}

// resolve fills a run's empty fields from the scenario and the defaults.
func (f File) resolve(s Scenario, r Run) Run {
	d := f.Defaults
	if r.Gateways == 0 {
		r.Gateways = d.Gateways
	}
	if r.Rate == 0 {
		r.Rate = d.Rate
	}
	if r.Keys == 0 {
		r.Keys = d.Keys
	}
	if r.Dist == "" {
		r.Dist = d.Dist
	}
	if r.Shape == "" {
		r.Shape = "steady"
	}
	if r.Duration == 0 {
		r.Duration = d.Duration
	}
	if r.Warmup == nil {
		r.Warmup = d.Warmup
	}
	if r.Warmup == nil {
		zero := time.Duration(0)
		r.Warmup = &zero
	}
	switch {
	case r.Reps != 0:
	case s.Reps != 0:
		r.Reps = s.Reps
	default:
		r.Reps = d.Reps
	}
	if r.Limit.Rate == 0 {
		r.Limit.Rate = d.Limit.Rate
	}
	if r.Limit.Period == 0 {
		r.Limit.Period = d.Limit.Period
	}
	env := maps.Clone(d.Env)
	if env == nil {
		env = map[string]string{}
	}
	maps.Copy(env, r.Env)
	r.Env = env
	return r
}

// Jobs lists every run of the chosen scenarios, in file order. ids may be
// "all"; reps above 0 overrides every scenario's repetitions.
func (f File) Jobs(ids []string, reps int) ([]Job, error) {
	want := map[string]bool{}
	for _, id := range ids {
		want[strings.ToUpper(strings.TrimSpace(id))] = true
	}
	var jobs []Job
	found := map[string]bool{}
	for _, s := range f.Scenarios {
		if !want["ALL"] && !want[strings.ToUpper(s.ID)] {
			continue
		}
		found[strings.ToUpper(s.ID)] = true
		for _, run := range s.Runs {
			r := f.resolve(s, run)
			n := r.Reps
			if reps > 0 {
				n = reps
			}
			for _, backend := range s.Backends {
				for rep := 1; rep <= n; rep++ {
					jobs = append(jobs, Job{Scenario: s.ID, Backend: backend, Rep: rep, Run: r})
				}
			}
		}
	}
	var missing []string
	for id := range want {
		if id != "ALL" && !found[id] {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return nil, fmt.Errorf("no scenario called %s", strings.Join(missing, ", "))
	}
	return jobs, nil
}

// LoadgenArgs is the load generator's command line for a job.
func (j Job) LoadgenArgs(csvPath string) []string {
	r := j.Run
	args := []string{
		"-rate", fmt.Sprint(r.Rate), "-keys", fmt.Sprint(r.Keys), "-dist", r.Dist,
		"-shape", r.Shape, "-out", csvPath, "-seed", fmt.Sprint(j.Rep),
	}
	if r.Shape == "burst" {
		args = append(args, "-idle", r.Idle.String(), "-burst-rate", fmt.Sprint(r.BurstRate), "-burst-for", r.BurstFor.String())
	} else {
		args = append(args, "-duration", r.Duration.String())
	}
	return args
}
