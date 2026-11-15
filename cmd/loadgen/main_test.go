package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agnivesh13/Aegis-Distributed-Limiter/internal/analyze"
)

func TestExpandTargets(t *testing.T) {
	lookup := func(host string) ([]string, error) {
		if host == "gateway" {
			return []string{"172.18.0.5", "172.18.0.3", "172.18.0.4"}, nil
		}
		return nil, errors.New("no such host")
	}
	got, err := expandTargets([]string{"http://gateway:8080", "http://10.0.0.1:9000"}, lookup)
	if err != nil {
		t.Fatal(err)
	}
	var have []string
	for _, tg := range got {
		have = append(have, tg.base.Host+"|"+tg.host)
	}
	want := "172.18.0.3:8080|gateway:8080 172.18.0.4:8080|gateway:8080 172.18.0.5:8080|gateway:8080 10.0.0.1:9000|10.0.0.1:9000"
	if strings.Join(have, " ") != want {
		t.Errorf("got  %s\nwant %s", strings.Join(have, " "), want)
	}

	if _, err := expandTargets([]string{"http://nowhere:1"}, lookup); err == nil {
		t.Error("an unresolvable host was accepted")
	}
	if _, err := expandTargets([]string{"gateway:8080"}, lookup); err == nil {
		t.Error("a target without a scheme was accepted")
	}
}

func TestSpread(t *testing.T) {
	got, _ := spread(6, 3, nil)
	if fmt.Sprint(got) != "[0 1 2 0 1 2]" {
		t.Errorf("equal spread = %v", got)
	}
	got, _ = spread(8, 2, []int{3, 1})
	if fmt.Sprint(got) != "[0 0 0 1 0 0 0 1]" {
		t.Errorf("weighted spread = %v", got)
	}
	if _, err := spread(4, 3, []int{1, 1}); err == nil {
		t.Error("weights for the wrong number of targets were accepted")
	}
	if _, err := spread(4, 2, []int{0, 0}); err == nil {
		t.Error("all-zero weights were accepted")
	}
}

func TestChooseKeys(t *testing.T) {
	count := func(keys []string) map[string]int {
		m := map[string]int{}
		for _, k := range keys {
			m[k]++
		}
		return m
	}

	if got := count(chooseKeys(100, options{key: "fixed", keys: 50})); got["fixed"] != 100 {
		t.Errorf("-key: %v", got)
	}

	uniform := count(chooseKeys(100_000, options{keys: 10, dist: "uniform", seed: 1}))
	for k, n := range uniform {
		if n < 9_000 || n > 11_000 {
			t.Errorf("uniform: %s got %d of 100000, want about 10000", k, n)
		}
	}

	hot := count(chooseKeys(100_000, options{keys: 10, dist: "hot", seed: 1}))
	if hot["key-0"] < 89_000 || hot["key-0"] > 91_000 {
		t.Errorf("hot: key-0 got %d of 100000, want about 90000", hot["key-0"])
	}

	zipf := count(chooseKeys(100_000, options{keys: 1000, dist: "zipf", seed: 1}))
	if zipf["key-0"] <= zipf["key-1"] || zipf["key-1"] <= zipf["key-10"] {
		t.Errorf("zipf: key-0 %d, key-1 %d, key-10 %d; want falling counts", zipf["key-0"], zipf["key-1"], zipf["key-10"])
	}
	if len(zipf) < 200 {
		t.Errorf("zipf: only %d distinct keys used of 1000", len(zipf))
	}

	a, b := chooseKeys(1000, options{keys: 10, dist: "uniform", seed: 7}), chooseKeys(1000, options{keys: 10, dist: "uniform", seed: 7})
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Error("the same seed gave different keys")
	}
}

func TestParseFlags(t *testing.T) {
	o, err := parseFlags([]string{"-shape", "burst", "-rate", "100"})
	if err != nil || o.burst != 1000 {
		t.Errorf("burst defaults: %+v, %v", o, err)
	}
	for _, bad := range [][]string{
		{"-rate", "0"}, {"-keys", "0"}, {"-dist", "pareto"}, {"-shape", "wave"}, {"-weights", "1,x"}, {"-targets", ""},
	} {
		if _, err := parseFlags(bad); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
}

// End to end against a fake gateway: every request is sent, counted and
// written to a CSV that the analyzer can read.
func TestRunWritesACSVTheAnalyzerReads(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Gateway-ID", "gw-fake")
		if r.Header.Get("X-API-Key") == "" || r.URL.Path != "/api/hello" {
			w.WriteHeader(400)
			return
		}
		if n.Add(1)%2 == 0 {
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "run.csv")
	o, err := parseFlags([]string{"-targets", srv.URL, "-rate", "200", "-duration", "500ms", "-keys", "3", "-out", out})
	if err != nil {
		t.Fatal(err)
	}
	var summary bytes.Buffer
	if err := run(context.Background(), o, &summary); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sent       100", "allowed    50", "rejected   50", "errors     0", "gateway    gw-fake: 100"} {
		if !strings.Contains(summary.String(), want) {
			t.Errorf("summary is missing %q:\n%s", want, summary.String())
		}
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	requests, err := analyze.ReadCSV(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 100 || requests[0].GatewayID != "gw-fake" || requests[99].Seq != 99 {
		t.Errorf("CSV has %d rows; first %+v", len(requests), requests[0])
	}
	if gap := requests[1].Send.Sub(requests[0].Send); gap != 5*time.Millisecond {
		t.Errorf("send times %v apart, want the 5ms schedule", gap)
	}
}

func TestBurstShapeSendsOnlyTheBurst(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n.Add(1) }))
	defer srv.Close()

	o, err := parseFlags([]string{"-targets", srv.URL, "-shape", "burst", "-idle", "200ms", "-burst-rate", "500", "-burst-for", "200ms"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var summary bytes.Buffer
	if err := run(context.Background(), o, &summary); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 100 {
		t.Errorf("server saw %d requests, want 100 (500/s for 200ms)", n.Load())
	}
	// The last of 100 requests at 500/s leaves 198 ms into the burst.
	if took := time.Since(start); took < 398*time.Millisecond {
		t.Errorf("run took %v, want at least the 200ms idle plus 198ms of burst", took)
	}
}
