// Package loadgen holds the load generator's send scheduling: the part that
// decides when each request leaves. cmd/loadgen builds the HTTP side on top.
package loadgen

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// LateAfter is how far behind its scheduled time a send may leave before it
// is counted as late. Many late sends mean the machine cannot keep up with
// the requested rate, and the run's numbers should not be trusted.
const LateAfter = 5 * time.Millisecond

// Schedule fixes when every request of a run should leave: request number
// seq leaves at Start + seq/Rate.
type Schedule struct {
	Start time.Time
	Rate  float64 // requests per second
}

// At returns the scheduled send time of request seq. It is worked out from
// seq each time, never by adding an interval to the previous send, so
// rounding errors cannot build up over a long run.
func (s Schedule) At(seq int64) time.Time {
	return s.Start.Add(time.Duration(float64(seq) / s.Rate * float64(time.Second)))
}

// Count returns how many requests fall inside a run of length d.
func (s Schedule) Count(d time.Duration) int64 {
	return int64(s.Rate * d.Seconds())
}

// Timer is the clock Run waits on. Tests replace it to run a schedule
// without waiting.
type Timer interface {
	Now() time.Time
	Sleep(d time.Duration)
}

// SystemTimer is the real clock.
type SystemTimer struct{}

func (SystemTimer) Now() time.Time        { return time.Now() }
func (SystemTimer) Sleep(d time.Duration) { time.Sleep(d) }

// Stats summarises one Run.
type Stats struct {
	Sent int64 // requests started
	Late int64 // requests that left more than LateAfter behind schedule
}

// SendFunc performs one request. scheduled is when it was meant to leave,
// which is the time to record as its send time.
type SendFunc func(seq int64, scheduled time.Time)

// Run fires send for every request on the schedule, for a run of length d,
// and returns when all of them have finished or ctx is cancelled.
//
// The load is open loop: each send runs in its own goroutine and Run never
// waits for one before starting the next. A slow server therefore cannot
// slow the load down, which would hide exactly the overload being measured.
//
// When Run falls behind (the machine stalled, or a sleep overran) it sends
// the overdue requests at once, without sleeping, until it is back on
// schedule, and counts those more than LateAfter behind as late.
func Run(ctx context.Context, sched Schedule, d time.Duration, timer Timer, send SendFunc) Stats {
	var (
		wg   sync.WaitGroup
		late atomic.Int64
		sent int64
	)
	total := sched.Count(d)
	for seq := int64(0); seq < total; seq++ {
		if ctx.Err() != nil {
			break
		}
		at := sched.At(seq)
		if wait := at.Sub(timer.Now()); wait > 0 {
			timer.Sleep(wait)
		}
		if timer.Now().Sub(at) > LateAfter {
			late.Add(1)
		}
		sent++
		wg.Go(func() { send(seq, at) })
	}
	wg.Wait()
	return Stats{Sent: sent, Late: late.Load()}
}
