// Package metrics defines the gateway's Prometheus collectors, exactly as
// listed in the plan's metrics table, on a registry of their own.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Decision label values for gateway_requests_total.
const (
	Allowed  = "allowed"
	Rejected = "rejected"
	Error    = "error"
)

// Metrics holds every collector. Create it once per process with New.
type Metrics struct {
	reg *prometheus.Registry

	Requests      *prometheus.CounterVec   // route, decision
	Decision      *prometheus.HistogramVec // backend, algorithm
	Upstream      *prometheus.HistogramVec // route
	LimiterErrors *prometheus.CounterVec   // backend
	GossipLatency prometheus.Histogram     // gossip_delivery_seconds
}

// New registers the collectors on a fresh registry. A private registry,
// not the global default, keeps tests independent of each other.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_requests_total",
			Help: "Requests that matched a route, by the rate limiter's decision.",
		}, []string{"route", "decision"}),
		Decision: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gateway_limiter_decision_seconds",
			Help: "Time spent inside one limiter Allow call.",
			// 10 µs, then ×1.5, 24 buckets: up to about 110 ms.
			Buckets: prometheus.ExponentialBuckets(10e-6, 1.5, 24),
		}, []string{"backend", "algorithm"}),
		Upstream: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_upstream_seconds",
			Help:    "Time to proxy a request to the upstream and relay its whole response to the client.",
			Buckets: prometheus.ExponentialBuckets(100e-6, 2, 16), // 100 µs to about 3.3 s
		}, []string{"route"}),
		LimiterErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_limiter_errors_total",
			Help: "Allow calls that failed because the backend could not decide.",
		}, []string{"backend"}),
		GossipLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "gossip_delivery_seconds",
			Help: "Age of a gossip packet on arrival: receiver's clock minus the sender's timestamp.",
			// Packet timestamps are whole milliseconds, so start at 1 ms.
			Buckets: prometheus.ExponentialBuckets(1e-3, 1.5, 20),
		}),
	}
	reg.MustRegister(m.Requests, m.Decision, m.Upstream, m.LimiterErrors,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// Handler serves the registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg})
}

// ObserveDecision records one Allow call's latency.
func (m *Metrics) ObserveDecision(backend, algorithm string, d time.Duration) {
	m.Decision.WithLabelValues(backend, algorithm).Observe(d.Seconds())
}

// TrackKeys exposes limiter_tracked_keys for a backend that can count its
// keys. count is called on every scrape.
func (m *Metrics) TrackKeys(backend string, count func() int) {
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name:        "limiter_tracked_keys",
		Help:        "Keys the limiter currently holds state for.",
		ConstLabels: prometheus.Labels{"backend": backend},
	}, func() float64 { return float64(count()) }))
}

// GossipStats is what the gossip node reports on each scrape.
type GossipStats struct {
	PacketsSent   uint64
	BytesSent     uint64
	Members       int
	EntriesMerged uint64
}

// TrackGossip exposes the gossip counters and the member gauge. stats is
// called on every scrape.
func (m *Metrics) TrackGossip(stats func() GossipStats) {
	m.reg.MustRegister(
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "gossip_packets_sent_total", Help: "Gossip packets sent.",
		}, func() float64 { return float64(stats().PacketsSent) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "gossip_bytes_sent_total", Help: "Bytes of gossip packets sent.",
		}, func() float64 { return float64(stats().BytesSent) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "gossip_entries_merged_total",
			Help: "Counts received from peers that told this node something new. Zero across a cluster means nodes are not learning from each other.",
		}, func() float64 { return float64(stats().EntriesMerged) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "gossip_members", Help: "Cluster members this node can see, itself included.",
		}, func() float64 { return float64(stats().Members) }),
		m.GossipLatency,
	)
}
