package memcache

import "time"

// cacheClock is the private seam between the cleaner and real time. Tests
// inject a synchronized fake; production wraps the cache's now function and
// time.NewTimer. It is not a public option: callers cannot replace the clock
// and no user callback runs on it.
type cacheClock interface {
	Now() time.Time
	NewTimer(time.Duration) cacheTimer
}

// cacheTimer is the channel-timer surface the cleaner owns. AfterFunc is
// deliberately not used: the worker must be the only receiver, and Go 1.18's
// Stop/Reset semantics require explicit state tracking around each call.
type cacheTimer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(time.Duration) bool
}

// systemClock combines the cache's injected now function with real timers, so
// deadline decisions and scheduling read the same clock source.
type systemClock struct {
	now func() time.Time
}

func (c systemClock) Now() time.Time { return c.now() }

func (c systemClock) NewTimer(d time.Duration) cacheTimer {
	return systemTimer{timer: time.NewTimer(d)}
}

type systemTimer struct {
	timer *time.Timer
}

func (t systemTimer) C() <-chan time.Time { return t.timer.C }

func (t systemTimer) Stop() bool { return t.timer.Stop() }

func (t systemTimer) Reset(d time.Duration) bool { return t.timer.Reset(d) }

// cleanerTimerState tracks the ownership states a single worker must
// distinguish. Only the cleaner worker may touch a cleanerTimer.
type cleanerTimerState uint8

const (
	cleanerTimerUncreated cleanerTimerState = iota
	cleanerTimerArmed
	cleanerTimerConsumed
	cleanerTimerStopped
)

// cleanerTimer is the worker's single timer plus its state. It creates at most
// one timer and reconfigures it with Reset. No method takes the cache lock; the
// worker calls all of them outside it.
//
// Go 1.18 semantics: Stop returns false when the timer already fired, and the
// buffered event may still be waiting. Because the worker is the only receiver,
// an armed-but-unconsumed timer can be drained with a single receive before
// Reset. A consumed or already-stopped timer has no pending event, so it is
// never drained and cannot block.
type cleanerTimer struct {
	state cleanerTimerState
	timer cacheTimer
}

// channel exposes the timer channel only while armed. A nil channel makes the
// worker's select simply not wait on the timer.
func (t *cleanerTimer) channel() <-chan time.Time {
	if t.state != cleanerTimerArmed || t.timer == nil {
		return nil
	}
	return t.timer.C()
}

// consumed marks that the worker received the current timer event. The channel
// disappears until the next arm so a stale value cannot be observed twice.
func (t *cleanerTimer) consumed() {
	if t.state == cleanerTimerArmed {
		t.state = cleanerTimerConsumed
	}
}

// arm creates the timer once and reschedules it. An armed timer that has not
// been consumed is stopped first; if Stop reports the event already fired, the
// worker (the only receiver) drains it before Reset.
func (t *cleanerTimer) arm(clock cacheClock, delay time.Duration) {
	if t.timer == nil {
		t.timer = clock.NewTimer(delay)
		t.state = cleanerTimerArmed
		return
	}
	if t.state == cleanerTimerArmed && !t.timer.Stop() {
		<-t.timer.C()
	}
	t.timer.Reset(delay)
	t.state = cleanerTimerArmed
}

// stop releases the timer. Only an armed timer can have a pending event; a
// consumed or stopped timer is never drained, so repeated stops cannot block.
func (t *cleanerTimer) stop() {
	if t.state == cleanerTimerArmed && t.timer != nil && !t.timer.Stop() {
		<-t.timer.C()
	}
	t.state = cleanerTimerStopped
}
