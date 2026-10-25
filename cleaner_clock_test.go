package memcache

import (
	"sync"
	"testing"
	"time"
)

// fakeClock is a synchronized clock and timer factory for cleaner tests. The
// cleaner worker runs on its own goroutine in cleaner_test.go, so every field
// is mutex-protected; tests schedule through channels and hooks, never sleeps.
type fakeClock struct {
	mu       sync.Mutex
	now      time.Time
	newCalls int
	timers   []*fakeTimer
	// stopReturnsFalse makes every fakeTimer.Stop report an already-fired
	// timer, reproducing the Go 1.18 case where an event is still buffered.
	stopReturnsFalse bool
	onNewTimer       func()
	onReset          func()
	onStop           func()
}

func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{now: now}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func (c *fakeClock) NewTimer(d time.Duration) cacheTimer {
	c.mu.Lock()
	t := &fakeTimer{clock: c, ch: make(chan time.Time, 1), delay: d, armed: true}
	c.newCalls++
	c.timers = append(c.timers, t)
	hook := c.onNewTimer
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	return t
}

// setStopReturnsFalse forces Stop to report false, so a helper that must drain
// a pending event is exercised without inventing a real goroutine race.
func (c *fakeClock) setStopReturnsFalse(value bool) {
	c.mu.Lock()
	c.stopReturnsFalse = value
	c.mu.Unlock()
}

// setHooks installs callbacks that run after the corresponding operation, with
// no fakeClock lock held. They are the deterministic "worker has reached this
// point" synchronization points for multi-goroutine tests.
func (c *fakeClock) setHooks(newTimer, reset, stop func()) {
	c.mu.Lock()
	c.onNewTimer, c.onReset, c.onStop = newTimer, reset, stop
	c.mu.Unlock()
}

func (c *fakeClock) newTimerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.newCalls
}

func (c *fakeClock) timer(index int) *fakeTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	if index >= len(c.timers) {
		return nil
	}
	return c.timers[index]
}

func (c *fakeClock) forcedStop() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopReturnsFalse
}

func (c *fakeClock) resetHook() func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.onReset
}

func (c *fakeClock) stopHook() func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.onStop
}

type fakeTimer struct {
	clock  *fakeClock
	ch     chan time.Time
	mu     sync.Mutex
	delay  time.Duration
	resets []time.Duration
	stops  int
	armed  bool
	fired  bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	c := t.clock
	forced := c.forcedStop()
	stopHook := c.stopHook()
	t.mu.Lock()
	t.stops++
	fired := t.fired
	if !fired && !forced {
		t.armed = false
	}
	t.mu.Unlock()
	if stopHook != nil {
		stopHook()
	}
	return !fired && !forced
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	resetHook := t.clock.resetHook()
	t.mu.Lock()
	wasActive := t.armed && !t.fired
	t.delay = d
	t.armed = true
	t.fired = false
	t.resets = append(t.resets, d)
	t.mu.Unlock()
	if resetHook != nil {
		resetHook()
	}
	return wasActive
}

// fire delivers one deadline into the buffered channel. With a one-slot buffer
// this is the Go 1.18 "event is pending and not yet consumed" state.
func (t *fakeTimer) fire() {
	t.mu.Lock()
	if !t.armed {
		t.mu.Unlock()
		return
	}
	t.fired = true
	t.mu.Unlock()
	select {
	case t.ch <- t.clock.Now():
	default:
	}
}

func (t *fakeTimer) resetCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.resets)
}

func (t *fakeTimer) stopCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stops
}

func (t *fakeTimer) pending() int { return len(t.ch) }

// TestCleanerTimerStopReset pins the helper's state machine, including the Go
// 1.18 path where Stop reports false because an old event is still buffered.
func TestCleanerTimerStopReset(t *testing.T) {
	const (
		first  = time.Second
		second = 2 * time.Second
	)
	t.Run("first arm creates exactly one timer", func(t *testing.T) {
		clock := newFakeClock(modelEpoch)
		var timer cleanerTimer
		if ch := timer.channel(); ch != nil {
			t.Fatal("uncreated timer exposed a channel")
		}
		timer.arm(clock, first)
		if got := clock.newTimerCount(); got != 1 {
			t.Fatalf("NewTimer calls = %d, want 1", got)
		}
		fake := clock.timer(0)
		fake.mu.Lock()
		delay := fake.delay
		fake.mu.Unlock()
		if delay != first {
			t.Fatalf("first delay = %v, want %v", delay, first)
		}
		if timer.channel() != fake.C() {
			t.Fatal("armed timer does not expose its channel")
		}
	})

	t.Run("armed with Stop true reuses the timer", func(t *testing.T) {
		clock := newFakeClock(modelEpoch)
		var timer cleanerTimer
		timer.arm(clock, first)
		timer.arm(clock, second)
		if got := clock.newTimerCount(); got != 1 {
			t.Fatalf("NewTimer calls = %d, want 1 (rearm must Reset)", got)
		}
		fake := clock.timer(0)
		if got := fake.resetCount(); got != 1 {
			t.Fatalf("Reset calls = %d, want 1", got)
		}
		if fake.stopCount() != 1 {
			t.Fatalf("Stop calls = %d, want 1", fake.stopCount())
		}
		if timer.channel() == nil {
			t.Fatal("rearmed timer lost its channel")
		}
	})

	t.Run("Stop false drains the pending event before Reset", func(t *testing.T) {
		clock := newFakeClock(modelEpoch)
		var timer cleanerTimer
		timer.arm(clock, first)
		fake := clock.timer(0)
		clock.setStopReturnsFalse(true)
		fake.fire() // stale deadline buffered, not yet consumed
		if fake.pending() != 1 {
			t.Fatal("fixture did not buffer a stale event")
		}
		clock.Advance(5 * time.Second)
		timer.arm(clock, second)
		if fake.pending() != 0 {
			t.Fatalf("rearm left %d stale event(s) buffered", fake.pending())
		}
		if got := clock.newTimerCount(); got != 1 {
			t.Fatalf("NewTimer calls = %d, want 1", got)
		}
		// The next delivered event must be the new one, not the drained stale
		// value: give the fake a distinct timestamp.
		clock.Advance(7 * time.Second)
		fake.fire()
		want := clock.Now()
		select {
		case got := <-timer.channel():
			if !got.Equal(want) {
				t.Fatalf("received stale event %v, want %v", got, want)
			}
		default:
			t.Fatal("no event after Reset")
		}
	})

	t.Run("rearm after the event was consumed uses Reset", func(t *testing.T) {
		clock := newFakeClock(modelEpoch)
		var timer cleanerTimer
		timer.arm(clock, first)
		fake := clock.timer(0)
		fake.fire()
		<-timer.channel()
		timer.consumed()
		if ch := timer.channel(); ch != nil {
			t.Fatal("consumed timer still exposes a channel")
		}
		timer.arm(clock, second)
		if got := clock.newTimerCount(); got != 1 {
			t.Fatalf("NewTimer calls = %d, want 1", got)
		}
		if fake.stopCount() != 0 {
			t.Fatalf("Stop called %d times after a consumed event, want 0", fake.stopCount())
		}
		clock.Advance(time.Second)
		fake.fire()
		select {
		case <-timer.channel():
		default:
			t.Fatal("no event after rearming a consumed timer")
		}
	})

	t.Run("stop drains once, then channel is nil and repeated stop does not block", func(t *testing.T) {
		clock := newFakeClock(modelEpoch)
		var timer cleanerTimer
		timer.arm(clock, first)
		fake := clock.timer(0)
		clock.setStopReturnsFalse(true)
		fake.fire()
		done := make(chan struct{})
		go func() {
			timer.stop()
			timer.stop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("stop blocked with a pending event")
		}
		if fake.pending() != 0 {
			t.Fatalf("stop left %d event(s) buffered", fake.pending())
		}
		if ch := timer.channel(); ch != nil {
			t.Fatal("stopped timer still exposes a channel")
		}
		if got := fake.stopCount(); got != 1 {
			t.Fatalf("Stop calls = %d, want 1 (a second stop must be a no-op)", got)
		}
	})

	t.Run("stop before arm never creates a timer and does not block", func(t *testing.T) {
		clock := newFakeClock(modelEpoch)
		var timer cleanerTimer
		done := make(chan struct{})
		go func() {
			timer.stop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("stop before arm blocked")
		}
		if got := clock.newTimerCount(); got != 0 {
			t.Fatalf("stop before arm created %d timer(s)", got)
		}
		if ch := timer.channel(); ch != nil {
			t.Fatal("stopped-before-arm timer exposes a channel")
		}
	})
}
