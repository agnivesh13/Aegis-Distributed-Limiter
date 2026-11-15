package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func parse(t *testing.T, yaml string, vars map[string]string) (Config, error) {
	t.Helper()
	return Parse(strings.NewReader(yaml), env(vars))
}

const minimal = `
gateway: { id: gw-test }
routes:
  - name: api
    path_prefix: /api/
    limit: { algorithm: sliding_window, rate: 100, period: 1s }
`

// The shipped config must load as it is; the containers depend on it.
func TestShippedConfigLoads(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "deploy", "gateway.yaml"), env(map[string]string{"GATEWAY_ID": "gw-1"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gateway.ID != "gw-1" || len(cfg.Routes) != 2 {
		t.Errorf("id = %q, %d routes; want gw-1 and 2", cfg.Gateway.ID, len(cfg.Routes))
	}
	search := cfg.Routes[1]
	want := Route{Name: "search", PathPrefix: "/api/search", Cost: 5, Limit: Limit{Algorithm: "token_bucket", Rate: 50, Period: time.Second, Burst: 100}}
	if !reflect.DeepEqual(search, want) {
		t.Errorf("search route = %+v, want %+v", search, want)
	}
	if cfg.Limiter.Timeout != 50*time.Millisecond || cfg.Gossip.FullSyncInterval != 5*time.Second {
		t.Errorf("durations: timeout %v, full sync %v", cfg.Limiter.Timeout, cfg.Gossip.FullSyncInterval)
	}
}

// The bench config must load under every backend: it is what the
// experiments compare them with.
func TestBenchConfigLoadsForEveryBackend(t *testing.T) {
	for _, backend := range []string{BackendMemory, BackendRedis, BackendGossip} {
		_, err := Load(filepath.Join("..", "..", "deploy", "gateway-bench.yaml"), env(map[string]string{"GATEWAY_ID": "g", "LIMITER_BACKEND": backend}))
		if err != nil {
			t.Errorf("%s: %v", backend, err)
		}
	}
}

func TestDefaultsFillWhatTheFileLeavesOut(t *testing.T) {
	cfg, err := parse(t, minimal, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gateway.Listen != ":8080" || cfg.Limiter.Backend != BackendMemory || cfg.Limiter.OnError != FailOpen {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	if cfg.Routes[0].Cost != 1 {
		t.Errorf("cost left out = %d, want 1", cfg.Routes[0].Cost)
	}
}

func TestEnvironmentOverridesTheFile(t *testing.T) {
	cfg, err := parse(t, minimal, map[string]string{
		"GATEWAY_ID":        "gw-env",
		"LIMITER_BACKEND":   "gossip",
		"LIMITER_ON_ERROR":  "fail_closed",
		"LIMITER_TIMEOUT":   "20ms",
		"REDIS_ADDR":        "toxiproxy:26379",
		"GOSSIP_INTERVAL":   "10ms",
		"GOSSIP_FANOUT":     "4",
		"GOSSIP_JOIN":       "a:7946, b:7946",
		"GOSSIP_RETRANSMIT": "5",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := []any{cfg.Gateway.ID, cfg.Limiter.Backend, cfg.Limiter.OnError, cfg.Limiter.Timeout, cfg.Redis.Addr,
		cfg.Gossip.Interval, cfg.Gossip.Fanout, cfg.Gossip.Join, cfg.Gossip.Retransmit}
	want := []any{"gw-env", "gossip", "fail_closed", 20 * time.Millisecond, "toxiproxy:26379",
		10 * time.Millisecond, 4, []string{"a:7946", "b:7946"}, 5}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestBadEnvironmentValueIsReported(t *testing.T) {
	_, err := parse(t, minimal, map[string]string{"GOSSIP_FANOUT": "two", "LIMITER_TIMEOUT": "soon"})
	if err == nil || !strings.Contains(err.Error(), "GOSSIP_FANOUT") || !strings.Contains(err.Error(), "LIMITER_TIMEOUT") {
		t.Errorf("err = %v, want both bad variables named", err)
	}
}

func TestUnknownKeyIsRejected(t *testing.T) {
	_, err := parse(t, minimal+"limitter: { backend: redis }\n", nil)
	if err == nil || !strings.Contains(err.Error(), "limitter") {
		t.Errorf("err = %v, want the misspelt key named", err)
	}
}

// Every invalid value is reported, all at once, each naming where it is.
func TestValidationReportsEveryProblem(t *testing.T) {
	const bad = `
gateway: { id: g, upstream: "ftp://x", listen: ":1", admin_listen: ":1" }
limiter: { backend: etcd, on_error: shrug, timeout: 0s }
routes:
  - name: "a:b"
    path_prefix: api
    cost: 500
    limit: { algorithm: leaky, rate: 0, period: 0s, burst: -1 }
  - name: dup
    path_prefix: /x
    limit: { algorithm: sliding_window, rate: 10, period: 1s, burst: 3 }
  - name: dup
    path_prefix: /x
    limit: { algorithm: sliding_window, rate: 10, period: 1s }
`
	_, err := parse(t, bad, nil)
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{
		"gateway.upstream", "both", "limiter.backend", "limiter.on_error", "limiter.timeout",
		"may not contain ':'", "must start with /", "limit.algorithm", "limit.rate", "limit.period",
		"limit.burst", "burst applies only to token_bucket", "name is used twice", "path_prefix \"/x\" is used twice",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestCostAboveCapacityIsRejected(t *testing.T) {
	yaml := strings.Replace(minimal, "path_prefix: /api/", "path_prefix: /api/\n    cost: 101", 1)
	if _, err := parse(t, yaml, nil); err == nil || !strings.Contains(err.Error(), "capacity 100") {
		t.Errorf("err = %v, want cost above capacity rejected", err)
	}
}

func TestGossipRefusesTokenBucket(t *testing.T) {
	yaml := strings.Replace(minimal, "sliding_window", "token_bucket", 1)
	if _, err := parse(t, yaml, map[string]string{"LIMITER_BACKEND": "gossip"}); err == nil || !strings.Contains(err.Error(), "only sliding_window") {
		t.Errorf("err = %v, want token_bucket refused for gossip", err)
	}
	if _, err := parse(t, yaml, nil); err != nil {
		t.Errorf("memory backend with token_bucket: %v", err)
	}
}

func TestSubMillisecondPeriodOnlyForMemory(t *testing.T) {
	yaml := strings.Replace(minimal, "period: 1s", "period: 1500us", 1)
	if _, err := parse(t, yaml, nil); err != nil {
		t.Errorf("memory backend: %v", err)
	}
	if _, err := parse(t, yaml, map[string]string{"LIMITER_BACKEND": "redis"}); err == nil {
		t.Error("redis backend accepted a 1.5 ms period")
	}
}

func TestEmptyFileNeedsRoutes(t *testing.T) {
	if _, err := parse(t, "", map[string]string{"GATEWAY_ID": "g"}); err == nil || !strings.Contains(err.Error(), "at least one route") {
		t.Errorf("err = %v, want routes required", err)
	}
}

func TestIDDefaultsToHostname(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Skip("no hostname")
	}
	cfg, err := parse(t, strings.Replace(minimal, "gateway: { id: gw-test }", "", 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gateway.ID != host {
		t.Errorf("id = %q, want the hostname %q", cfg.Gateway.ID, host)
	}
}
