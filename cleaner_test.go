package memcache

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The cleaner tests drive a real worker goroutine through a synchronized fake
// clock. Scheduling uses channels and hooks only; the two-second guards exist
// to fail a hung test, never to let time make a test pass.

// countingClock counts Now calls (the idle proof) and can report them to a
// channel so a test can wait for the worker's first observation.
type countingClock struct {
	*fakeClock
	nowCalls  int32
	nowSignal chan struct{}
}

func (c *countingClock) Now() time.Time {
	atomic.AddInt32(&c.nowCalls, 1)
	if c.nowSignal != nil {
		select {
		case c.nowSignal <- struct{}{}:
		default:
		}
	}
	return c.fakeClock.Now()
}

func (c *countingClock) nowCallCount() int { return int(atomic.LoadInt32(&c.nowCalls)) }

// stepClock gates worker Now calls: with the gate armed, each call reports
// itself and then waits for one token. Foreground operations must not run
// while the gate is armed, which the batch-close test guarantees.
type stepClock struct {
	*fakeClock
	armed   int32
	reached chan struct{}
	gate    chan struct{}
	stop    chan struct{}
}

func (c *stepClock) Now() time.Time {
	if atomic.LoadInt32(&c.armed) != 0 {
		select {
		case c.reached <- struct{}{}:
		default:
		}
		select {
		case <-c.gate:
		case <-c.stop:
		}
	}
	return c.fakeClock.Now()
}

func (c *stepClock) armGate() { atomic.StoreInt32(&c.armed, 1) }

func (c *stepClock) disarmGate() { atomic.StoreInt32(&c.armed, 0) }

func newCleanerFixture(t *testing.T, capacity uint) (*Memcache[int], *fakeClock, *countingClock) {
	t.Helper()
	fake := newFakeClock(modelEpoch)
	clock := &countingClock{fakeClock: fake}
	m := newMemcacheWithClock[int](capacity, true, clock, true)
	t.Cleanup(m.Close)
	return m, fake, clock
}

// seamSignals reports every NewTimer/Reset/Stop call made by the worker.
func seamSignals(fake *fakeClock) <-chan struct{} {
	ch := make(chan struct{}, 64)
	signal := func() {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	fake.setHooks(signal, signal, signal)
	return ch
}

// waitForSeam evaluates cond whenever the worker reaches a timer seam and
// fails after a two-second hang guard. The signal is the schedule; the guard
// only prevents a deadlocked test suite.
func waitForSeam(t *testing.T, ch <-chan struct{}, what string, cond func() bool) {
	t.Helper()
	guard := time.NewTimer(2 * time.Second)
	defer guard.Stop()
	for {
		if cond() {
			return
		}
		select {
		case <-ch:
		case <-guard.C:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func (t *fakeTimer) armedNow() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.armed && !t.fired
}

func (t *fakeTimer) delayNow() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.delay
}

func waitArmedDelay(t *testing.T, ch <-chan struct{}, fake *fakeClock, want time.Duration) {
	t.Helper()
	waitForSeam(t, ch, fmt.Sprintf("timer armed for %v", want), func() bool {
		timer := fake.timer(0)
		return timer != nil && timer.armedNow() && timer.delayNow() == want
	})
}

// waitCondition waits for a state that may change without a later timer seam.
// The tick is the hang guard; the state, not elapsed time, is the assertion.
func waitCondition(t *testing.T, what string, cond func() bool) {
	t.Helper()
	guard := time.NewTimer(2 * time.Second)
	defer guard.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if cond() {
			return
		}
		select {
		case <-tick.C:
		case <-guard.C:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// waitStateClosed waits for Close's state switch, which happens before Close
// blocks on the worker. Polling is the hang guard here: Close exposes no seam
// between its two phases.
func waitStateClosed(t *testing.T, m *Memcache[int]) {
	t.Helper()
	guard := time.NewTimer(2 * time.Second)
	defer guard.Stop()
	for {
		if snapshotCache(m).Closed {
			return
		}
		select {
		case <-guard.C:
			t.Fatal("Close did not switch the state")
		case <-time.After(time.Millisecond):
		}
	}
}

func waitDisarmed(t *testing.T, ch <-chan struct{}, fake *fakeClock) {
	t.Helper()
	waitForSeam(t, ch, "timer stopped", func() bool {
		timer := fake.timer(0)
		return timer == nil || !timer.armedNow()
	})
}

func TestCleanerBatchLimit(t *testing.T) {
	const (
		expired = 130
		live    = 5
	)
	m, clock := clockCache(t, expired+live)
	for i := 0; i < expired; i++ {
		m.SetExpire(fmt.Sprintf("exp%03d", i), i, 1)
	}
	for i := 0; i < live; i++ {
		m.Set(fmt.Sprintf("live%d", i), 1000+i)
	}
	clock.Advance(time.Second)
	now := clock.Now()

	for batch, want := range []int{defaultCleanupBatchSize, defaultCleanupBatchSize, expired - 2*defaultCleanupBatchSize, 0} {
		got := m.state.purgeExpiredBatchLocked(now, defaultCleanupBatchSize)
		if got != want {
			t.Fatalf("batch %d removed %d entries, want %d", batch, got, want)
		}
		assertInvariants(t, m)
	}
	snap := snapshotCache(m)
	if len(snap.Items) != live {
		t.Fatalf("physical entries after all batches = %d, want the %d live ones", len(snap.Items), live)
	}
	for i := 0; i < live; i++ {
		if _, ok := snap.Items[fmt.Sprintf("live%d", i)]; !ok {
			t.Fatalf("batching removed live key live%d", i)
		}
	}
	if len(snap.Heap) != 0 {
		t.Fatalf("heap records = %d after draining expired entries", len(snap.Heap))
	}
}

func TestCleanerEmptyHeapSleeps(t *testing.T) {
	fake := newFakeClock(modelEpoch)
	clock := &countingClock{fakeClock: fake, nowSignal: make(chan struct{}, 64)}
	m := newMemcacheWithClock[int](4, true, clock, true)
	defer m.Close()

	// The worker observes the world once when it starts, then sleeps. The
	// signal is the schedule; the guard only catches a hung start.
	select {
	case <-clock.nowSignal:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never sampled the clock")
	}
	calls := clock.nowCallCount()
	for i := 0; i < 10; i++ {
		fake.Advance(100 * time.Millisecond) // the removed ticker's period
	}
	if got := clock.nowCallCount(); got != calls {
		t.Fatalf("empty-heap worker made %d clock calls after the first, want 0", got-calls)
	}
	if got := fake.newTimerCount(); got != 0 {
		t.Fatalf("empty-heap worker created %d timers, want 0", got)
	}

	ch := seamSignals(fake)
	m.SetExpire("k", 1, 3600)
	waitArmedDelay(t, ch, fake, 3600*time.Second)
	if clock.nowCallCount() <= calls {
		t.Fatal("first TTL write did not make the worker read the clock again")
	}
}

func TestCleanerDeadlineChanges(t *testing.T) {
	m, fake, _ := newCleanerFixture(t, 8)
	ch := seamSignals(fake)

	m.SetExpire("a", 1, 30)
	waitArmedDelay(t, ch, fake, 30*time.Second)

	// A new earlier deadline must replace the timer.
	m.SetExpire("b", 2, 10)
	waitArmedDelay(t, ch, fake, 10*time.Second)

	// Extending the earliest entry exposes the next real root.
	m.SetExpire("b", 3, 120)
	waitArmedDelay(t, ch, fake, 30*time.Second)

	// Deleting the heap root exposes the next root.
	m.Delete("a")
	waitArmedDelay(t, ch, fake, 120*time.Second)

	// Cancelling the last TTL empties the heap and stops the timer.
	m.Set("b", 4)
	waitDisarmed(t, ch, fake)

	// The empty-to-nonempty transition must arm again.
	m.SetExpire("c", 5, 3)
	waitArmedDelay(t, ch, fake, 3*time.Second)

	// The due entry is reclaimed in one batch.
	fake.Advance(3 * time.Second)
	fake.timer(0).fire()
	// A batch that empties the heap ends with a stop on a consumed timer, which
	// emits no seam call; wait on the resulting state instead.
	waitCondition(t, "due entry reclaimed", func() bool {
		m.state.mu.Lock()
		defer m.state.mu.Unlock()
		// "b" stayed live without a TTL; only the due "c" may disappear.
		_, hasC := m.state.holder["c"]
		return !hasC && m.state.holder["b"] != nil
	})
}

func TestCleanerWakeCoalesces(t *testing.T) {
	m, fake, _ := newCleanerFixture(t, 8)
	reached := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseFn()
	signals := make(chan struct{}, 64)
	signal := func() {
		select {
		case signals <- struct{}{}:
		default:
		}
	}
	var once sync.Once
	fake.setHooks(func() {
		once.Do(func() {
			close(reached)
			<-release
		})
	}, signal, signal)

	m.SetExpire("a", 1, 30)
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never armed the first timer")
	}

	// While the worker is parked inside its first NewTimer, several root
	// changes coalesce into at most one buffered notification and never block
	// the foreground.
	done := make(chan struct{})
	go func() {
		m.SetExpire("b", 2, 10)
		m.SetExpire("c", 3, 5)
		m.Delete("c")
		m.SetExpire("a", 4, 20)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("foreground write blocked while the worker was parked")
	}
	if got := len(m.state.wake); got > 1 {
		t.Fatalf("wake buffer holds %d notifications, want at most 1", got)
	}
	releaseFn()
	waitArmedDelay(t, signals, fake, 10*time.Second)
}

func TestCleanerStaleEventAfterClear(t *testing.T) {
	m, fake, _ := newCleanerFixture(t, 8)
	reached := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseFn()
	signals := make(chan struct{}, 64)
	signal := func() {
		select {
		case signals <- struct{}{}:
		default:
		}
	}
	var once sync.Once
	fake.setHooks(func() {
		once.Do(func() {
			close(reached)
			<-release
		})
	}, signal, signal)

	m.SetExpire("k", 1, 10)
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never armed the first timer")
	}
	// The old event is already in the timer channel when Clear and a same-key
	// rebuild happen. Releasing the worker lets that stale event arrive after
	// the rebuild and it must not delete the new value.
	fake.Advance(time.Second)
	fake.timer(0).fire()
	m.Clear()
	m.SetExpire("k", 2, 3600)
	releaseFn()

	waitArmedDelay(t, signals, fake, 3600*time.Second)
	requireValue(t, m, "k", 2)
	assertInvariants(t, m)
}

func TestCleanerCloseBetweenBatches(t *testing.T) {
	fake := newFakeClock(modelEpoch)
	clock := &stepClock{
		fakeClock: fake,
		reached:   make(chan struct{}, 1),
		gate:      make(chan struct{}),
		stop:      make(chan struct{}),
	}
	m := newMemcacheWithClock[int](200, true, clock, true)
	var releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(func() { close(clock.stop) }) }
	defer func() {
		releaseGate()
		m.Close()
	}()

	reached := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fake.setHooks(func() {
		once.Do(func() {
			close(reached)
			<-release
		})
	}, nil, nil)

	const expired = 130
	for i := 0; i < expired; i++ {
		m.SetExpire(fmt.Sprintf("e%03d", i), i, 1)
	}
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never armed the future timer")
	}
	fake.Advance(time.Second)
	clock.armGate()
	// Deliver the deadline: the fake clock does not fire timers by itself.
	fake.timer(0).fire()
	close(release)

	// The first gated Now call happens before any batch.
	select {
	case <-clock.reached:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never reached the first batch")
	}
	clock.gate <- struct{}{}
	// After exactly one batch the worker parks at the next gated Now call.
	select {
	case <-clock.reached:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never parked after the first batch")
	}
	snap := snapshotCache(m)
	if len(snap.Items) != expired-defaultCleanupBatchSize {
		t.Fatalf("physical entries after one batch = %d, want %d", len(snap.Items), expired-defaultCleanupBatchSize)
	}
	assertInvariants(t, m)

	// Close must switch the state and close done while the worker is parked
	// outside the cache lock; it then waits for the worker to exit.
	closed := make(chan struct{})
	go func() {
		m.Close()
		m.Close()
		close(closed)
	}()
	waitStateClosed(t, m)
	clock.disarmGate()
	releaseGate()
	select {
	case <-m.state.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not exit after Close")
	}
	if snap := snapshotCache(m); len(snap.Items) != 0 {
		t.Fatalf("closed cache retained %d entries", len(snap.Items))
	}
}

func TestCleanerDisabledAndZeroCapacity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		enable bool
		cap    uint
	}{
		{"TTL disabled", false, 4},
		{"zero capacity", true, 0},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeClock(modelEpoch)
			m := newMemcacheWithClock[int](tc.cap, tc.enable, fake, true)
			closed := make(chan struct{})
			go func() {
				m.Close()
				m.Close()
				close(closed)
			}()
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				t.Fatal("Close hung without a worker")
			}
			if got := fake.newTimerCount(); got != 0 {
				t.Fatalf("created %d timer(s) without a worker", got)
			}
		})
	}

	t.Run("Clear then first TTL wakes again", func(t *testing.T) {
		m, fake, _ := newCleanerFixture(t, 2)
		ch := seamSignals(fake)
		m.SetExpire("k", 1, 3600)
		waitArmedDelay(t, ch, fake, 3600*time.Second)
		m.Clear()
		waitDisarmed(t, ch, fake)
		m.SetExpire("k", 2, 3600)
		waitArmedDelay(t, ch, fake, 3600*time.Second)
		requireValue(t, m, "k", 2)
	})
}
