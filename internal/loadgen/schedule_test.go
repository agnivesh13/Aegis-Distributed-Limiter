package loadgen

import (
	"context"
	"sync"
	"testing"
	"time"
)

var start = time.Unix(1_700_000_000, 0)

// fakeTimer is a clock that moves only when slept on. stall, if set, makes
// one chosen Sleep overrun, like a machine that froze for a moment.
type fakeTimer struct {
	mu       sync.Mutex
	now      time.Time
	sleeps   int
	stallAt  int           // which Sleep call overruns (1-based); 0 for never
	stallFor time.Duration // by how much
}

func (f *fakeTimer) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeTimer) Sleep(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sleeps++
	f.now = f.now.Add(d)
	if f.sleeps == f.stallAt {
		f.now = f.now.Add(f.stallFor)
	}
}

// recorder collects the sends a Run makes.
type recorder struct {
	mu   sync.Mutex
	seqs map[int64]time.Time
}

func (r *recorder) send(seq int64, scheduled time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seqs == nil {
		r.seqs = make(map[int64]time.Time)
	}
	r.seqs[seq] = scheduled
}

func TestScheduleDoesNotDrift(t *testing.T) {
	s := Schedule{Start: start, Rate: 3} // 1/3 s cannot be stored exactly
	if got, want := s.At(3_000_000), start.Add(1_000_000*time.Second); got != want {
		t.Errorf("request 3,000,000 at 3 per second is scheduled at %v, want %v", got, want)
	}
	if got := s.Count(30 * time.Second); got != 90 {
		t.Errorf("Count(30s) = %d, want 90", got)
	}
}

func TestRunSendsEveryRequestOnSchedule(t *testing.T) {
	sched := Schedule{Start: start, Rate: 200}
	timer := &fakeTimer{now: start}
	var rec recorder

	stats := Run(context.Background(), sched, 2*time.Second, timer, rec.send)

	if stats.Sent != 400 || stats.Late != 0 {
		t.Errorf("Stats = %+v, want 400 sent and none late", stats)
	}
	if len(rec.seqs) != 400 {
		t.Fatalf("send was called for %d requests, want 400", len(rec.seqs))
	}
	for seq, scheduled := range rec.seqs {
		if want := sched.At(seq); scheduled != want {
			t.Errorf("request %d was given scheduled time %v, want %v", seq, scheduled, want)
		}
	}
}

// After a stall the overdue requests go out at once, the ones more than
// 5 ms behind are counted late, and the schedule is back on time afterwards
// with nothing skipped.
func TestRunCatchesUpAfterAStall(t *testing.T) {
	sched := Schedule{Start: start, Rate: 1000} // one request per millisecond
	timer := &fakeTimer{now: start, stallAt: 100, stallFor: 20 * time.Millisecond}
	var rec recorder

	stats := Run(context.Background(), sched, time.Second, timer, rec.send)

	if stats.Sent != 1000 || len(rec.seqs) != 1000 {
		t.Fatalf("sent %d (send called %d times), want all 1000 despite the stall", stats.Sent, len(rec.seqs))
	}
	// The stall ends 20 ms past request 100's time. Requests 100 to 114
	// are then 20 ms down to 6 ms behind: late. Request 115 is exactly
	// 5 ms behind, which is within the allowance.
	if stats.Late != 15 {
		t.Errorf("Late = %d, want 15", stats.Late)
	}
	if got, want := timer.Now(), sched.At(999); got != want {
		t.Errorf("the run ended at %v, want %v: the schedule did not recover", got, want)
	}
}

// Open loop: a send that never returns must not hold up the sends after it.
func TestRunDoesNotWaitForSlowSends(t *testing.T) {
	sched := Schedule{Start: start, Rate: 100}
	timer := &fakeTimer{now: start}

	release := make(chan struct{})
	started := make(chan int64, 100)
	done := make(chan Stats)
	go func() {
		done <- Run(context.Background(), sched, time.Second, timer, func(seq int64, _ time.Time) {
			started <- seq
			<-release // every send hangs until the test lets go
		})
	}()

	for range 100 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("not all 100 sends started while earlier ones were still hanging")
		}
	}
	close(release)
	if stats := <-done; stats.Sent != 100 {
		t.Errorf("Sent = %d, want 100", stats.Sent)
	}
}

func TestRunStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sched := Schedule{Start: start, Rate: 100}
	var rec recorder

	stats := Run(ctx, sched, time.Second, &fakeTimer{now: start}, rec.send)
	if stats.Sent != 0 || len(rec.seqs) != 0 {
		t.Errorf("sent %d requests on a cancelled context, want none", stats.Sent)
	}
}
