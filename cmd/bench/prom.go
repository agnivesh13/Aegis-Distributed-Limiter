package main

import (
	"bufio"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Snapshot is one scrape of every gateway, reduced to the few numbers the
// bench runner reports. Counters are summed over gateways.
type Snapshot struct {
	DecisionBuckets map[float64]float64 // le -> cumulative count, gateway_limiter_decision_seconds
	GossipBytes     float64
	GossipPackets   float64
	LimiterErrors   float64
}

func newSnapshot() Snapshot {
	return Snapshot{DecisionBuckets: map[float64]float64{}}
}

// Add parses one gateway's /metrics text into the snapshot. It reads only
// the lines it needs, so it does not depend on a parsing library's API.
func (s *Snapshot) Add(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		name, labels, value, ok := parseSample(line)
		if !ok {
			continue
		}
		switch name {
		case "gateway_limiter_decision_seconds_bucket":
			// ParseFloat reads "+Inf" as positive infinity.
			if le, err := strconv.ParseFloat(labelValue(labels, "le"), 64); err == nil {
				s.DecisionBuckets[le] += value
			}
		case "gossip_bytes_sent_total":
			s.GossipBytes += value
		case "gossip_packets_sent_total":
			s.GossipPackets += value
		case "gateway_limiter_errors_total":
			s.LimiterErrors += value
		}
	}
	return sc.Err()
}

// parseSample splits `name{labels} value` (labels optional).
func parseSample(line string) (name, labels string, value float64, ok bool) {
	sp := strings.LastIndexByte(line, ' ')
	if sp < 0 {
		return "", "", 0, false
	}
	v, err := strconv.ParseFloat(line[sp+1:], 64)
	if err != nil {
		return "", "", 0, false
	}
	head := line[:sp]
	if i := strings.IndexByte(head, '{'); i >= 0 {
		return head[:i], strings.TrimSuffix(head[i+1:], "}"), v, true
	}
	return head, "", v, true
}

// labelValue finds name="value" in a label string. Label values here never
// contain commas or quotes.
func labelValue(labels, name string) string {
	for _, part := range strings.Split(labels, ",") {
		k, v, ok := strings.Cut(part, "=")
		if ok && k == name {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

// Sub returns what happened between an earlier snapshot and this one.
func (s Snapshot) Sub(earlier Snapshot) Snapshot {
	out := newSnapshot()
	for le, n := range s.DecisionBuckets {
		out.DecisionBuckets[le] = n - earlier.DecisionBuckets[le]
	}
	out.GossipBytes = s.GossipBytes - earlier.GossipBytes
	out.GossipPackets = s.GossipPackets - earlier.GossipPackets
	out.LimiterErrors = s.LimiterErrors - earlier.LimiterErrors
	return out
}

// DecisionQuantile estimates the q-quantile of decision latency in seconds
// the way Prometheus's histogram_quantile does: find the bucket the rank
// falls in and interpolate linearly inside it. The answer is only as fine
// as the buckets, which grow by 1.5x each.
func (s Snapshot) DecisionQuantile(q float64) float64 {
	bounds := make([]float64, 0, len(s.DecisionBuckets))
	for le := range s.DecisionBuckets {
		bounds = append(bounds, le)
	}
	sort.Float64s(bounds)
	if len(bounds) == 0 {
		return 0
	}
	total := s.DecisionBuckets[bounds[len(bounds)-1]] // +Inf holds the total
	if total <= 0 {
		return 0
	}
	rank := q * total
	prevBound, prevCount := 0.0, 0.0
	for _, le := range bounds {
		count := s.DecisionBuckets[le]
		if count >= rank {
			if math.IsInf(le, 1) {
				return prevBound // beyond the last finite bucket: report its edge
			}
			if count == prevCount {
				return le
			}
			return prevBound + (le-prevBound)*(rank-prevCount)/(count-prevCount)
		}
		prevBound, prevCount = le, count
	}
	return prevBound
}
