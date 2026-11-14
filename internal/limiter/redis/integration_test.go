//go:build integration

package redis

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter/limitertest"
)

// These tests need a real Redis. Run them with:
//
//	go test -tags integration ./internal/limiter/redis/
//
// By default they start redis:7-alpine in Docker through testcontainers.
// Set REDIS_ADDR (for example 127.0.0.1:6379) to use a Redis that is
// already running. The tests flush that Redis, so never point them at one
// whose data matters.

var redisAddr string

func TestMain(m *testing.M) {
	os.Exit(runWithRedis(m))
}

func runWithRedis(m *testing.M) int {
	if redisAddr = os.Getenv("REDIS_ADDR"); redisAddr != "" {
		return m.Run()
	}

	ctx := context.Background()
	container, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		fmt.Fprintln(os.Stderr, "starting Redis:", err)
		return 1
	}
	defer container.Terminate(ctx)

	url, err := container.ConnectionString(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Redis address:", err)
		return 1
	}
	opts, err := goredis.ParseURL(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Redis address:", err)
		return 1
	}
	redisAddr = opts.Addr
	return m.Run()
}

// testTimeout is generous so a slow laptop does not fail a test that is
// about limiter behaviour and not about speed.
const testTimeout = 5 * time.Second

func flush(t *testing.T) {
	t.Helper()
	rdb := goredis.NewClient(&goredis.Options{Addr: redisAddr})
	defer rdb.Close()
	if err := rdb.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("flushing Redis: %v", err)
	}
}

// supported is what the backend implements right now: the sliding window
// joins once sliding_window.lua has been written.
func supported() []limiter.Algorithm {
	if slidingWindowWritten {
		return []limiter.Algorithm{limiter.TokenBucket, limiter.SlidingWindow}
	}
	return []limiter.Algorithm{limiter.TokenBucket}
}

func limitFor(a limiter.Algorithm) limiter.Limit {
	if a == limiter.TokenBucket {
		return limiter.Limit{Algorithm: a, Rate: 10, Period: time.Second, Burst: 100}
	}
	return limiter.Limit{Algorithm: a, Rate: 100, Period: time.Second}
}

// The Redis backend must behave exactly like the memory backend. Time is
// injected through the scripts' now_ms argument.
func TestConformance(t *testing.T) {
	limitertest.Run(t, func(t *testing.T, clock limiter.Clock) limiter.Limiter {
		flush(t)
		return New(redisAddr, WithClock(clock), WithTimeout(testTimeout))
	}, supported()...)
}

// hammer sends 10,000 requests for one key from 50 goroutines, spread over
// the given limiters, and returns how many were admitted.
func hammer(t *testing.T, limiters []limiter.Limiter, l limiter.Limit) int64 {
	t.Helper()
	const goroutines, perGoroutine = 50, 200

	var (
		wg       sync.WaitGroup
		admitted atomic.Int64
		failed   atomic.Pointer[error]
	)
	for g := range goroutines {
		lim := limiters[g%len(limiters)]
		wg.Go(func() {
			for range perGoroutine {
				d, err := lim.Allow(context.Background(), "route:client", l, 1)
				if err != nil {
					failed.CompareAndSwap(nil, &err)
					return
				}
				if d.Allowed {
					admitted.Add(1)
				}
			}
		})
	}
	wg.Wait()
	if err := failed.Load(); err != nil {
		t.Fatalf("Allow: %v", *err)
	}
	return admitted.Load()
}

// Three gateways sharing one Redis admit exactly the limit, however their
// requests interleave. The clock is frozen so nothing refills.
func TestThreeInstancesAdmitExactlyTheLimit(t *testing.T) {
	for _, a := range supported() {
		t.Run(string(a), func(t *testing.T) {
			flush(t)
			clock := limitertest.NewFakeClock(time.Unix(1_700_000_000, 0))
			var limiters []limiter.Limiter
			for range 3 {
				b := New(redisAddr, WithClock(clock), WithTimeout(testTimeout))
				defer b.Close()
				limiters = append(limiters, b)
			}

			l := limitFor(a)
			if got, want := hammer(t, limiters, l), int64(l.Capacity()); got != want {
				t.Errorf("admitted %d of 10000, want exactly %d", got, want)
			}
		})
	}
}

// The same load against a backend that reads, decides in Go and writes back
// admits more than the limit: two gateways read the same count and both
// allow. This is the bug the Lua scripts exist to prevent. The number it
// logs goes in the report.
func TestNaiveBackendAdmitsMoreThanTheLimit(t *testing.T) {
	flush(t)
	clock := limitertest.NewFakeClock(time.Unix(1_700_000_000, 0))
	var limiters []limiter.Limiter
	for range 3 {
		n := &naiveBackend{rdb: goredis.NewClient(&goredis.Options{Addr: redisAddr}), clock: clock}
		defer n.Close()
		limiters = append(limiters, n)
	}

	l := limitFor(limiter.SlidingWindow)
	got := hammer(t, limiters, l)
	t.Logf("naive GET-then-SET backend: admitted %d of 10000 requests against a limit of %d (%.0f%% over)",
		got, l.Rate, float64(got-int64(l.Rate))/float64(l.Rate)*100)
	if got <= int64(l.Rate) {
		t.Errorf("admitted %d, want more than the limit of %d: the lost-update race did not show", got, l.Rate)
	}
}

// With Redis unreachable, Allow fails fast and never hangs. When Redis is
// back, the same Backend works again with nothing restarted.
func TestRedisOutageAndRecovery(t *testing.T) {
	flush(t)
	proxy := newFlakyProxy(t, redisAddr)
	const timeout = 200 * time.Millisecond
	b := New(proxy.addr, WithTimeout(timeout))
	defer b.Close()
	l := limitFor(limiter.TokenBucket)

	if _, err := b.Allow(context.Background(), "route:client", l, 1); err != nil {
		t.Fatalf("Allow with Redis up: %v", err)
	}

	t.Run("ConnectionRefused", func(t *testing.T) {
		proxy.stop()
		start := time.Now()
		_, err := b.Allow(context.Background(), "route:client", l, 1)
		if err == nil {
			t.Fatal("Allow succeeded with Redis down")
		}
		if took := time.Since(start); took > timeout+time.Second {
			t.Errorf("Allow took %v to fail, want it within the %v timeout (plus slack)", took, timeout)
		}
	})

	t.Run("RecoversUnaided", func(t *testing.T) {
		proxy.start()
		waitUntilAllowed(t, b, l)
	})

	t.Run("NoReplyHitsTheDeadline", func(t *testing.T) {
		proxy.setBlackhole(true)

		// The first call may fail at once on a connection the proxy just
		// cut. A later call opens a fresh one, which is accepted and then
		// never answered; that call must wait for the deadline and no more.
		waited := false
		for range 3 {
			start := time.Now()
			_, err := b.Allow(context.Background(), "route:client", l, 1)
			took := time.Since(start)
			if err == nil {
				t.Fatal("Allow succeeded although Redis never replied")
			}
			if took > timeout+time.Second {
				t.Errorf("Allow took %v to give up, want it within the %v timeout (plus slack)", took, timeout)
			}
			if took >= timeout/2 {
				waited = true
				t.Logf("gave up after %v with: %v", took, err)
				break
			}
		}
		if !waited {
			t.Error("no call waited on the silent connection, so the deadline was never exercised")
		}
	})

	t.Run("RecoversAfterNoReply", func(t *testing.T) {
		proxy.setBlackhole(false)
		waitUntilAllowed(t, b, l)
	})
}

// waitUntilAllowed retries Allow for a few seconds. The first calls after an
// outage may still fail while dead connections are thrown away.
func waitUntilAllowed(t *testing.T, b *Backend, l limiter.Limit) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := b.Allow(context.Background(), "route:client", l, 1)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Allow still failing 5 s after Redis came back: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// naiveBackend is the wrong way to share a limit, kept only to show why it
// is wrong. It reads the state with GET, decides in Go and writes with SET.
// Between its read and its write another gateway can do the same, and one
// of the two updates is lost.
type naiveBackend struct {
	rdb   *goredis.Client
	clock limiter.Clock
}

func (n *naiveBackend) Allow(ctx context.Context, key string, l limiter.Limit, cost int) (limiter.Decision, error) {
	w := l.Period.Milliseconds()
	now := n.clock.Now().UnixMilli()
	idx, elapsed := now/w, now%w

	// State is "idx cur prev" in one string.
	var sidx, cur, prev int64
	raw, err := n.rdb.Get(ctx, "naive:"+key).Result()
	if err != nil && err != goredis.Nil {
		return limiter.Decision{}, err
	}
	if f := strings.Fields(raw); len(f) == 3 {
		sidx, _ = strconv.ParseInt(f[0], 10, 64)
		cur, _ = strconv.ParseInt(f[1], 10, 64)
		prev, _ = strconv.ParseInt(f[2], 10, 64)
	}
	switch sidx {
	case idx:
	case idx - 1:
		prev, cur = cur, 0
	default:
		prev, cur = 0, 0
	}

	estimate := float64(cur) + float64(prev)*(1-float64(elapsed)/float64(w))
	d := limiter.Decision{Limit: l.Rate}
	if estimate+float64(cost) > float64(l.Rate) {
		d.RetryAfter = time.Duration(w-elapsed) * time.Millisecond
		return d, nil
	}
	cur += int64(cost)
	state := fmt.Sprintf("%d %d %d", idx, cur, prev)
	if err := n.rdb.Set(ctx, "naive:"+key, state, 2*l.Period).Err(); err != nil {
		return limiter.Decision{}, err
	}
	d.Allowed = true
	d.Remaining = max(0, l.Rate-int(estimate)-cost)
	return d, nil
}

func (n *naiveBackend) Close() error { return n.rdb.Close() }

// flakyProxy is a TCP forwarder that sits between a Backend and Redis so a
// test can take Redis away and bring it back on the same address.
type flakyProxy struct {
	t      *testing.T
	addr   string // where the Backend connects
	target string // the real Redis

	mu        sync.Mutex
	ln        net.Listener
	conns     map[net.Conn]struct{}
	blackhole bool
}

func newFlakyProxy(t *testing.T, target string) *flakyProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &flakyProxy{t: t, addr: ln.Addr().String(), target: target, conns: make(map[net.Conn]struct{})}
	p.serve(ln)
	t.Cleanup(p.stop)
	return p
}

// start listens again on the proxy's address after a stop.
func (p *flakyProxy) start() {
	p.t.Helper()
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		p.t.Fatalf("proxy listen on %s: %v", p.addr, err)
	}
	p.serve(ln)
}

func (p *flakyProxy) serve(ln net.Listener) {
	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			go p.handle(client)
		}
	}()
}

func (p *flakyProxy) handle(client net.Conn) {
	p.mu.Lock()
	p.conns[client] = struct{}{}
	blackhole := p.blackhole
	p.mu.Unlock()

	if blackhole {
		io.Copy(io.Discard, client) // accept everything, answer nothing
		return
	}
	server, err := net.Dial("tcp", p.target)
	if err != nil {
		client.Close()
		return
	}
	p.mu.Lock()
	p.conns[server] = struct{}{}
	p.mu.Unlock()
	go func() { io.Copy(server, client); server.Close() }()
	io.Copy(client, server)
	client.Close()
}

// stop refuses new connections and cuts the open ones, like Redis going down.
func (p *flakyProxy) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		p.ln.Close()
		p.ln = nil
	}
	p.dropConnsLocked()
}

// setBlackhole(true) makes the proxy accept connections and never reply,
// like a Redis that has stalled.
func (p *flakyProxy) setBlackhole(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blackhole = on
	p.dropConnsLocked()
}

func (p *flakyProxy) dropConnsLocked() {
	for c := range p.conns {
		c.Close()
		delete(p.conns, c)
	}
}
