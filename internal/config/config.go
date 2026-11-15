// Package config loads the gateway's YAML configuration, applies
// environment overrides and validates the result.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
)

// Backends and error policies the gateway understands.
const (
	BackendMemory = "memory"
	BackendRedis  = "redis"
	BackendGossip = "gossip"

	FailOpen   = "fail_open"
	FailClosed = "fail_closed"
)

// Config is the whole gateway configuration, laid out as in the plan.
type Config struct {
	Gateway Gateway `yaml:"gateway"`
	Limiter Limiter `yaml:"limiter"`
	Redis   Redis   `yaml:"redis"`
	Gossip  Gossip  `yaml:"gossip"`
	Routes  []Route `yaml:"routes"`
}

type Gateway struct {
	ID                string `yaml:"id"`
	Listen            string `yaml:"listen"`
	AdminListen       string `yaml:"admin_listen"`
	Upstream          string `yaml:"upstream"`
	TrustForwardedFor bool   `yaml:"trust_forwarded_for"`
}

type Limiter struct {
	Backend string        `yaml:"backend"`
	OnError string        `yaml:"on_error"`
	Timeout time.Duration `yaml:"timeout"`
}

type Redis struct {
	Addr     string `yaml:"addr"`
	PoolSize int    `yaml:"pool_size"`
}

type Gossip struct {
	BindPort         int           `yaml:"bind_port"`
	Join             []string      `yaml:"join"`
	Interval         time.Duration `yaml:"interval"`
	Fanout           int           `yaml:"fanout"`
	Retransmit       int           `yaml:"retransmit"`
	MaxPacketBytes   int           `yaml:"max_packet_bytes"`
	FullSyncInterval time.Duration `yaml:"full_sync_interval"`
}

type Route struct {
	Name       string `yaml:"name"`
	PathPrefix string `yaml:"path_prefix"`
	Cost       int    `yaml:"cost"` // 0 or left out means 1
	Limit      Limit  `yaml:"limit"`
}

type Limit struct {
	Algorithm string        `yaml:"algorithm"`
	Rate      int           `yaml:"rate"`
	Period    time.Duration `yaml:"period"`
	Burst     int           `yaml:"burst"`
}

// LimiterLimit converts the YAML form into the limiter's type.
func (l Limit) LimiterLimit() limiter.Limit {
	return limiter.Limit{Algorithm: limiter.Algorithm(l.Algorithm), Rate: l.Rate, Period: l.Period, Burst: l.Burst}
}

// Default returns the configuration used for anything the file leaves out.
// Routes have no default: a gateway without routes is refused.
func Default() Config {
	return Config{
		Gateway: Gateway{Listen: ":8080", AdminListen: ":9090", Upstream: "http://upstream:8081"},
		Limiter: Limiter{Backend: BackendMemory, OnError: FailOpen, Timeout: 50 * time.Millisecond},
		Redis:   Redis{Addr: "redis:6379", PoolSize: 64},
		Gossip: Gossip{
			BindPort: 7946, Join: []string{"gateway:7946"},
			Interval: 100 * time.Millisecond, Fanout: 2, Retransmit: 3,
			MaxPacketBytes: 1200, FullSyncInterval: 5 * time.Second,
		},
	}
}

// Load reads the file at path, applies environment overrides from getenv
// (pass os.Getenv) and validates the result.
func Load(path string, getenv func(string) string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	defer f.Close()
	return Parse(f, getenv)
}

// Parse is Load for an already open file.
func Parse(r io.Reader, getenv func(string) string) (Config, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}

	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // a misspelt key is an error, not a silent default
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if err := applyEnv(&cfg, getenv); err != nil {
		return Config{}, err
	}
	if cfg.Gateway.ID == "" {
		host, err := os.Hostname()
		if err != nil {
			return Config{}, fmt.Errorf("config: gateway.id is empty and the hostname is unknown: %w", err)
		}
		cfg.Gateway.ID = host
	}
	for i := range cfg.Routes {
		if cfg.Routes[i].Cost == 0 {
			cfg.Routes[i].Cost = 1
		}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// envOverrides lists every environment variable the gateway reads. The
// bench runner changes one of these per run.
var envOverrides = []struct {
	name  string
	apply func(c *Config, v string) error
}{
	{"GATEWAY_ID", func(c *Config, v string) error { c.Gateway.ID = v; return nil }},
	{"GATEWAY_UPSTREAM", func(c *Config, v string) error { c.Gateway.Upstream = v; return nil }},
	{"LIMITER_BACKEND", func(c *Config, v string) error { c.Limiter.Backend = v; return nil }},
	{"LIMITER_ON_ERROR", func(c *Config, v string) error { c.Limiter.OnError = v; return nil }},
	{"LIMITER_TIMEOUT", func(c *Config, v string) error { return setDuration(&c.Limiter.Timeout, v) }},
	{"REDIS_ADDR", func(c *Config, v string) error { c.Redis.Addr = v; return nil }},
	{"GOSSIP_JOIN", func(c *Config, v string) error { c.Gossip.Join = splitList(v); return nil }},
	{"GOSSIP_INTERVAL", func(c *Config, v string) error { return setDuration(&c.Gossip.Interval, v) }},
	{"GOSSIP_FANOUT", func(c *Config, v string) error { return setInt(&c.Gossip.Fanout, v) }},
	{"GOSSIP_RETRANSMIT", func(c *Config, v string) error { return setInt(&c.Gossip.Retransmit, v) }},
	{"GOSSIP_FULL_SYNC_INTERVAL", func(c *Config, v string) error { return setDuration(&c.Gossip.FullSyncInterval, v) }},
}

// EnvNames returns the names of the environment overrides, for help text.
func EnvNames() []string {
	names := make([]string, len(envOverrides))
	for i, o := range envOverrides {
		names[i] = o.name
	}
	return names
}

func applyEnv(c *Config, getenv func(string) string) error {
	var errs []error
	for _, o := range envOverrides {
		if v := strings.TrimSpace(getenv(o.name)); v != "" {
			if err := o.apply(c, v); err != nil {
				errs = append(errs, fmt.Errorf("%s=%q: %w", o.name, v, err))
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("config: environment: %w", err)
	}
	return nil
}

func setDuration(dst *time.Duration, v string) error {
	d, err := time.ParseDuration(v)
	if err != nil {
		return err
	}
	*dst = d
	return nil
}

func setInt(dst *int, v string) error {
	n, err := strconv.Atoi(v)
	if err != nil {
		return err
	}
	*dst = n
	return nil
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Validate reports every invalid value at once, so one start-up attempt
// shows everything that needs fixing.
func (c Config) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Gateway.Listen == "" {
		bad("gateway.listen is empty")
	}
	if c.Gateway.AdminListen == "" {
		bad("gateway.admin_listen is empty")
	}
	if c.Gateway.Listen != "" && c.Gateway.Listen == c.Gateway.AdminListen {
		bad("gateway.listen and gateway.admin_listen are both %q", c.Gateway.Listen)
	}
	if u, err := url.Parse(c.Gateway.Upstream); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		bad("gateway.upstream %q is not an http or https URL", c.Gateway.Upstream)
	}

	switch c.Limiter.Backend {
	case BackendMemory, BackendRedis, BackendGossip:
	default:
		bad("limiter.backend %q: want memory, redis or gossip", c.Limiter.Backend)
	}
	switch c.Limiter.OnError {
	case FailOpen, FailClosed:
	default:
		bad("limiter.on_error %q: want fail_open or fail_closed", c.Limiter.OnError)
	}
	if c.Limiter.Timeout <= 0 {
		bad("limiter.timeout must be positive, got %v", c.Limiter.Timeout)
	}

	if c.Limiter.Backend == BackendRedis {
		if c.Redis.Addr == "" {
			bad("redis.addr is empty")
		}
		if c.Redis.PoolSize < 1 {
			bad("redis.pool_size must be at least 1, got %d", c.Redis.PoolSize)
		}
	}
	if c.Limiter.Backend == BackendGossip {
		g := c.Gossip
		if g.BindPort < 1 || g.BindPort > 65535 {
			bad("gossip.bind_port %d is not a port", g.BindPort)
		}
		if len(g.Join) == 0 {
			bad("gossip.join is empty")
		}
		if g.Interval <= 0 {
			bad("gossip.interval must be positive, got %v", g.Interval)
		}
		if g.Fanout < 1 {
			bad("gossip.fanout must be at least 1, got %d", g.Fanout)
		}
		if g.Retransmit < 1 {
			bad("gossip.retransmit must be at least 1, got %d", g.Retransmit)
		}
		if g.MaxPacketBytes < 64 {
			bad("gossip.max_packet_bytes must be at least 64, got %d", g.MaxPacketBytes)
		}
		if g.FullSyncInterval <= 0 {
			bad("gossip.full_sync_interval must be positive, got %v", g.FullSyncInterval)
		}
	}

	if len(c.Routes) == 0 {
		bad("routes: at least one route is needed")
	}
	names, prefixes := map[string]bool{}, map[string]bool{}
	for i, r := range c.Routes {
		where := fmt.Sprintf("routes[%d]", i)
		if r.Name != "" {
			where = fmt.Sprintf("route %q", r.Name)
		}
		switch {
		case r.Name == "":
			bad("%s: name is empty", where)
		case strings.ContainsAny(r.Name, ": "):
			bad("%s: name may not contain ':' or spaces; it is part of the limiter key", where)
		case names[r.Name]:
			bad("%s: name is used twice", where)
		}
		names[r.Name] = true
		if !strings.HasPrefix(r.PathPrefix, "/") {
			bad("%s: path_prefix %q must start with /", where, r.PathPrefix)
		} else if prefixes[r.PathPrefix] {
			bad("%s: path_prefix %q is used twice", where, r.PathPrefix)
		}
		prefixes[r.PathPrefix] = true
		errs = append(errs, validateLimit(where, r, c.Limiter.Backend)...)
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("config is invalid:\n%w", err)
	}
	return nil
}

func validateLimit(where string, r Route, backend string) []error {
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: "+format, append([]any{where}, args...)...))
	}
	l := r.Limit
	switch limiter.Algorithm(l.Algorithm) {
	case limiter.TokenBucket:
		if backend == BackendGossip {
			bad("the gossip backend supports only sliding_window, not token_bucket")
		}
	case limiter.SlidingWindow:
		if l.Burst != 0 {
			bad("burst applies only to token_bucket")
		}
	default:
		bad("limit.algorithm %q: want token_bucket or sliding_window", l.Algorithm)
	}
	if l.Rate < 1 {
		bad("limit.rate must be at least 1, got %d", l.Rate)
	}
	if l.Period <= 0 {
		bad("limit.period must be positive, got %v", l.Period)
	} else if backend != BackendMemory && (l.Period < time.Millisecond || l.Period%time.Millisecond != 0) {
		bad("limit.period %v must be a whole number of milliseconds for the %s backend", l.Period, backend)
	}
	if l.Burst < 0 {
		bad("limit.burst must not be negative, got %d", l.Burst)
	}
	if capacity := l.LimiterLimit().Capacity(); r.Cost < 1 || (l.Rate >= 1 && r.Cost > capacity) {
		bad("cost %d must be between 1 and the limit's capacity %d", r.Cost, capacity)
	}
	return errs
}
