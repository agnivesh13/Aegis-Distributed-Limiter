package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// A point is one scenario setting on one backend: the repetitions of it
// are summarised together.
type point struct {
	scenario, backend, label string
	gateways                 string
	reps                     int
	over, falseRej, peak     []float64
	p99, decisionP99         []float64
	latePct, errPct          []float64
}

// printTable reads a summary file and writes one Markdown row per point:
// the median of its repetitions, with the lowest and highest in brackets
// where they differ. This is the "median and range of 3 runs" the plan's
// measurement rules ask for, and the README and report quote it.
func printTable(summaryPath string, w io.Writer) error {
	f, err := os.Open(summaryPath)
	if err != nil {
		return err
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return err
	}
	if len(rows) < 2 {
		return fmt.Errorf("%s has no runs", summaryPath)
	}
	col := map[string]int{}
	for i, name := range rows[0] {
		col[name] = i
	}
	for _, need := range []string{"scenario", "backend", "gateways", "requests", "errors", "late_sends",
		"over_admission_pct", "false_reject_pct", "peak_window_ratio", "p99_ms", "note"} {
		if _, ok := col[need]; !ok {
			return fmt.Errorf("%s has no %q column", summaryPath, need)
		}
	}

	var order []string
	points := map[string]*point{}
	for _, r := range rows[1:] {
		get := func(name string) string { return r[col[name]] }
		num := func(name string) float64 { v, _ := strconv.ParseFloat(get(name), 64); return v }
		meta := noteFields(get("note"))

		key := get("scenario") + "|" + get("backend") + "|" + meta["label"]
		p := points[key]
		if p == nil {
			p = &point{scenario: get("scenario"), backend: get("backend"), label: meta["label"], gateways: get("gateways")}
			points[key] = p
			order = append(order, key)
		}
		p.reps++
		p.over = append(p.over, num("over_admission_pct"))
		p.falseRej = append(p.falseRej, num("false_reject_pct"))
		p.peak = append(p.peak, num("peak_window_ratio"))
		p.p99 = append(p.p99, num("p99_ms"))
		if v, err := strconv.ParseFloat(meta["decision_p99_us"], 64); err == nil {
			p.decisionP99 = append(p.decisionP99, v)
		}
		if n := num("requests"); n > 0 {
			p.latePct = append(p.latePct, 100*num("late_sends")/n)
			p.errPct = append(p.errPct, 100*num("errors")/n)
		}
	}

	fmt.Fprintln(w, "| Scenario | Point | Backend | Gateways | Runs | Over-admission % | False rejects % | Peak in one period | Decision p99 (µs) | Client p99 (ms) | Errors % | Late sends % |")
	fmt.Fprintln(w, "| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
	for _, key := range order {
		p := points[key]
		fmt.Fprintf(w, "| %s | %s | %s | %s | %d | %s | %s | %s | %s | %s | %s | %s |\n",
			p.scenario, p.label, p.backend, p.gateways, p.reps,
			medianRange(p.over, 2), medianRange(p.falseRej, 2), medianRange(p.peak, 2),
			medianRange(p.decisionP99, 0), medianRange(p.p99, 2), medianRange(p.errPct, 2), medianRange(p.latePct, 2))
	}
	return nil
}

// noteFields splits the note column's key=value pairs.
func noteFields(note string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Fields(note) {
		if k, v, ok := strings.Cut(part, "="); ok {
			out[k] = v
		}
	}
	return out
}

// median returns the middle value, or the mean of the two middle values.
func median(values []float64) float64 {
	s := append([]float64(nil), values...)
	sort.Float64s(s)
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// medianRange formats "median (low to high)", leaving the range out when
// every repetition gave the same value at this precision.
func medianRange(values []float64, decimals int) string {
	if len(values) == 0 {
		return "n/a"
	}
	s := append([]float64(nil), values...)
	sort.Float64s(s)
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', decimals, 64) }
	med, lo, hi := f(median(s)), f(s[0]), f(s[len(s)-1])
	if lo == hi {
		return med
	}
	return fmt.Sprintf("%s (%s to %s)", med, lo, hi)
}
