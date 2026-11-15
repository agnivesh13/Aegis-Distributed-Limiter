// Command gateway is the rate-limiting reverse proxy.
//
//	gateway -config deploy/gateway.yaml
//
// Environment variables override the file; see internal/config.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/admin"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/config"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter/gossip"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter/memory"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter/redis"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/metrics"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/proxy"
)

// drainTimeout is how long in-flight requests get to finish on shutdown.
const drainTimeout = 10 * time.Second

func main() {
	configPath := flag.String("config", "/etc/gateway/gateway.yaml", "path to the YAML configuration")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: gateway [-config file]\n\nEnvironment overrides: %s, DEBUG_FAULTS\n\n",
			strings.Join(config.EnvNames(), ", "))
		flag.PrintDefaults()
	}
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(*configPath, log); err != nil {
		log.Error("gateway stopped", "err", err)
		os.Exit(1)
	}
}

func run(configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath, os.Getenv)
	if err != nil {
		return err
	}
	log = log.With("gateway_id", cfg.Gateway.ID)
	upstream, _ := url.Parse(cfg.Gateway.Upstream) // validated already

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	m := metrics.New()
	b, err := newBackend(ctx, cfg, m, log)
	if err != nil {
		return err
	}

	var draining atomic.Bool
	ready := func(ctx context.Context) error {
		if draining.Load() {
			return errors.New("shutting down")
		}
		if b.ready != nil {
			return b.ready(ctx)
		}
		return nil
	}

	routes := make([]proxy.Route, len(cfg.Routes))
	for i, r := range cfg.Routes {
		routes[i] = proxy.Route{Name: r.Name, PathPrefix: r.PathPrefix, Cost: r.CostValue(), Limit: r.Limit.LimiterLimit()}
	}
	proxySrv := &http.Server{
		Addr: cfg.Gateway.Listen,
		Handler: proxy.New(proxy.Options{
			GatewayID: cfg.Gateway.ID, Upstream: upstream, Routes: routes,
			Limiter: b.limiter, Backend: cfg.Limiter.Backend,
			FailClosed: cfg.Limiter.OnError == config.FailClosed, Timeout: cfg.Limiter.Timeout,
			TrustForwardedFor: cfg.Gateway.TrustForwardedFor, Metrics: m, Logger: log,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	adminSrv := &http.Server{
		Addr: cfg.Gateway.AdminListen,
		Handler: admin.Handler(admin.Options{
			GatewayID: cfg.Gateway.ID, Backend: cfg.Limiter.Backend, Ready: ready, Metrics: m.Handler(),
			LimiterDebug: b.debug, Block: b.block,
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	checkRoutes(ctx, b.limiter, routes, cfg.Limiter.OnError, log)

	errc := make(chan error, 2)
	for _, srv := range []*http.Server{proxySrv, adminSrv} {
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("listening on %s: %w", srv.Addr, err)
			}
		}()
	}
	log.Info("gateway started", "listen", cfg.Gateway.Listen, "admin", cfg.Gateway.AdminListen,
		"backend", cfg.Limiter.Backend, "on_error", cfg.Limiter.OnError, "upstream", cfg.Gateway.Upstream, "routes", len(routes))

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down", "drain", drainTimeout.String())
	case runErr = <-errc:
	}

	// Report not-ready first, so anything checking /readyz stops sending,
	// then let in-flight requests finish before the limiter goes away.
	draining.Store(true)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	if err := proxySrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("proxy did not drain in time", "err", err)
	}
	if err := adminSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("admin server did not stop cleanly", "err", err)
	}
	if err := b.limiter.Close(); err != nil {
		log.Warn("closing the limiter", "err", err)
	}
	log.Info("stopped")
	return runErr
}

// checkRoutes asks the limiter once per route, under a key no client can
// send, whether it can decide that route's algorithm at all. A route it
// cannot decide would otherwise fall to the on_error policy on every single
// request, which looks like a working gateway until someone reads the error
// counter.
func checkRoutes(ctx context.Context, lim limiter.Limiter, routes []proxy.Route, onError string, log *slog.Logger) {
	for _, r := range routes {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		// A NUL byte cannot appear in an HTTP header, so no real client
		// key can collide with this one.
		_, err := lim.Allow(probeCtx, "\x00startup-probe:"+r.Name, r.Limit, 1)
		cancel()
		if errors.Is(err, limiter.ErrUnsupported) {
			log.Error("this backend cannot decide this route's algorithm yet; every request on the route will follow on_error",
				"route", r.Name, "algorithm", string(r.Limit.Algorithm), "on_error", onError)
		}
	}
}

// backend is the configured limiter plus the extras each one offers.
type backend struct {
	limiter limiter.Limiter
	ready   func(context.Context) error
	debug   func(key string) any
	block   func(peer string, block bool)
}

func newBackend(ctx context.Context, cfg config.Config, m *metrics.Metrics, log *slog.Logger) (backend, error) {
	switch cfg.Limiter.Backend {
	case config.BackendMemory:
		b := memory.New(limiter.SystemClock{})
		m.TrackKeys(config.BackendMemory, b.Len)
		return backend{limiter: b}, nil

	case config.BackendRedis:
		b := redis.New(cfg.Redis.Addr, redis.WithPoolSize(cfg.Redis.PoolSize), redis.WithTimeout(cfg.Limiter.Timeout))
		return backend{limiter: b, ready: b.Ping}, nil

	case config.BackendGossip:
		return newGossip(ctx, cfg, m, log)
	}
	return backend{}, fmt.Errorf("unknown backend %q", cfg.Limiter.Backend)
}

func newGossip(ctx context.Context, cfg config.Config, m *metrics.Metrics, log *slog.Logger) (backend, error) {
	g := cfg.Gossip
	node := gossip.NewNode(cfg.Gateway.ID, limiter.SystemClock{}, gossip.Config{
		Interval: g.Interval, Fanout: g.Fanout, Retransmit: g.Retransmit, MaxPacketBytes: g.MaxPacketBytes,
		OnDelivery: func(age time.Duration) { m.GossipLatency.Observe(max(0, age.Seconds())) },
	})
	ml, err := gossip.NewMemberlist(gossip.MemberlistConfig{
		Name: cfg.Gateway.ID, BindPort: g.BindPort, FullSyncInterval: g.FullSyncInterval,
	}, node)
	if err != nil {
		return backend{}, err
	}
	node.Start(ml)
	go joinCluster(ctx, ml, g.Join, log)

	m.TrackKeys(config.BackendGossip, node.Store().Len)
	m.TrackGossip(func() metrics.GossipStats {
		s := node.Stats()
		return metrics.GossipStats{PacketsSent: s.PacketsSent, BytesSent: s.BytesSent, Members: node.Members(), EntriesMerged: s.EntriesMerged}
	})

	b := backend{
		limiter: node,
		debug:   func(key string) any { return node.Store().Counts(key) },
	}
	if os.Getenv("DEBUG_FAULTS") == "1" {
		log.Warn("fault injection is on: /debug/gossip/block can cut this gateway off from its peers")
		b.block = func(peer string, block bool) {
			if block {
				node.Block(peer)
			} else {
				node.Unblock(peer)
			}
			log.Info("gossip block changed", "peer", peer, "blocked", block)
		}
	}
	return b, nil
}

// joinCluster keeps trying the join addresses until this gateway can see
// another member. Replicas start in no particular order, so the first
// attempts often find nobody but themselves. A gateway that never finds a
// peer still works; it just limits alone.
func joinCluster(ctx context.Context, ml *gossip.Memberlist, addrs []string, log *slog.Logger) {
	const give = 60 * time.Second
	deadline := time.Now().Add(give)
	for attempt := 1; ; attempt++ {
		n, err := ml.Join(addrs)
		if peers := len(ml.Peers()); peers > 0 {
			log.Info("joined gossip cluster", "peers", peers, "contacted", n, "attempts", attempt)
			return
		}
		if time.Now().After(deadline) {
			log.Warn("found no gossip peers; limiting alone until one joins", "join", addrs, "err", err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
