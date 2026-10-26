package memcache

import (
	"testing"
	"time"
)

// newApproxCache builds a workerless cache whose reads follow
// Options.ApproximateTTLReads. The injected clock carries no monotonic
// reading, so systemClock keeps its portable Since path.
func newApproxCache(t *testing.T, approximate bool) (*Memcache[int], *testClock) {
	t.Helper()
	clock := &testClock{now: modelEpoch}
	m := newMemcacheConfigured[int](
		Options{Capacity: 16, EnableExpired: true, ApproximateTTLReads: approximate},
		systemClock{now: clock.Now}, cleanerNone)
	t.Cleanup(m.Close)
	return m, clock
}

// TestApproximateReadsOffIsExact pins the default: without the option a read
// consults the precise clock, so an expired entry misses exactly as before.
func TestApproximateReadsOffIsExact(t *testing.T) {
	m, clock := newApproxCache(t, false)
	m.SetExpire("a", 1, 1)
	clock.Advance(2 * time.Second)
	requireMissing(t, m, "a")
	requireMissing(t, m, "a")
	if got := m.Len(); got != 0 {
		t.Fatalf("Len = %d, want 0 after expired read", got)
	}
	assertInvariants(t, m)
}

// TestApproximateReadsServeUnobservedExpiry pins the opt-in slack: an entry
// whose deadline passed but was not yet observed (no newer sample published)
// is served as a hit.
func TestApproximateReadsServeUnobservedExpiry(t *testing.T) {
	m, clock := newApproxCache(t, true)
	m.SetExpire("a", 1, 1)
	clock.Advance(2 * time.Second)
	requireValue(t, m, "a", 1)
	if v, ok := m.Peek("a"); !ok || v != 1 {
		t.Fatalf("Peek = (%v, %v), want (1, true)", v, ok)
	}
	if got := m.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1 while expiry is unobserved", got)
	}
	assertInvariants(t, m)
}

// TestApproximateReadsMissAfterObservation: once a newer sample is published
// (any later TTL write), the expired entry compares below it, misses, and is
// removed.
func TestApproximateReadsMissAfterObservation(t *testing.T) {
	m, clock := newApproxCache(t, true)
	m.SetExpire("a", 1, 1)
	clock.Advance(2 * time.Second)
	m.SetExpire("b", 2, 60)
	requireMissing(t, m, "a")
	requireValue(t, m, "b", 2)
	if got := m.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1 (only b remains)", got)
	}
	assertInvariants(t, m)
}

// TestApproximateReadsBoundary: a deadline exactly equal to the published
// sample counts as expired (the sample is a lower bound of the real clock).
func TestApproximateReadsBoundary(t *testing.T) {
	m, clock := newApproxCache(t, true)
	m.SetExpire("a", 1, 1)
	clock.Advance(time.Second)
	m.SetExpire("b", 2, 60) // publishes a sample at the exact deadline of a
	requireMissing(t, m, "a")
	requireValue(t, m, "b", 2)
}

// TestApproximateReadsCleanerPublishes drives a real worker with the fake
// clock: after a purge round the shard holds the worker's clock sample, so a
// read inside the observation slack hits while the entry is unobserved, and
// misses once the worker has reclaimed it.
func TestApproximateReadsCleanerPublishes(t *testing.T) {
	fake := newFakeClock(modelEpoch)
	m := newMemcacheWithClockShards[int](16, true, fake, true, 1, shardSlotTarget, false, true, true)
	t.Cleanup(m.Close)
	ch := seamSignals(fake)
	m.SetExpire("a", 1, 1)
	m.SetExpire("b", 2, 2)
	waitArmedDelay(t, ch, fake, time.Second)

	fake.Advance(1500 * time.Millisecond)
	fake.timer(0).fire()
	waitForSeam(t, ch, "purge of the first entry", func() bool { return m.Len() == 1 })
	requireMissing(t, m, "a")

	m.state.shards[0].mu.Lock()
	sample := m.state.shards[0].nowSample
	m.state.shards[0].mu.Unlock()
	if want := int64(1500 * time.Millisecond); sample != want {
		t.Fatalf("nowSample = %d, want %d after the purge round", sample, want)
	}

	// The real clock is past b's deadline, but the last observed sample is
	// not: the read must still hit.
	fake.Advance(time.Second)
	requireValue(t, m, "b", 2)

	waitArmedDelay(t, ch, fake, 500*time.Millisecond)
	fake.Advance(time.Second)
	fake.timer(0).fire()
	// The purge after this fire leaves the worker's timer consumed, so its
	// next stop skips fakeTimer.Stop and fires no seam hook — waitForSeam
	// would sleep forever. Poll for the state change instead.
	waitEventually(t, "purge of the second entry", func() bool { return m.Len() == 0 })
	requireMissing(t, m, "b")
}

// waitEventually polls a state condition with a hang guard. Prefer the
// seam-driven waitForSeam whenever the worker passes a timer seam; use this
// only when the awaited transition produces no seam signal.
func waitEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	guard := time.NewTimer(2 * time.Second)
	defer guard.Stop()
	for !cond() {
		select {
		case <-guard.C:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(time.Millisecond):
		}
	}
}

// TestPublishNowMonotonic pins the publication rules: never regress a newer
// sample, and never store at all when the option is off.
func TestPublishNowMonotonic(t *testing.T) {
	on, _ := newApproxCache(t, true)
	sh := on.state.shards[0]
	sh.mu.Lock()
	sh.nowSample = 100
	sh.mu.Unlock()
	on.state.publishNow(sh, 50)
	sh.mu.Lock()
	if got := sh.nowSample; got != 100 {
		sh.mu.Unlock()
		t.Fatalf("nowSample = %d after publishing 50, want 100 (no regression)", got)
	}
	sh.mu.Unlock()
	on.state.publishNow(sh, 150)
	sh.mu.Lock()
	if got := sh.nowSample; got != 150 {
		sh.mu.Unlock()
		t.Fatalf("nowSample = %d after publishing 150, want 150", got)
	}
	sh.mu.Unlock()

	off, _ := newApproxCache(t, false)
	shOff := off.state.shards[0]
	shOff.mu.Lock()
	shOff.nowSample = 100
	shOff.mu.Unlock()
	off.state.publishNow(shOff, 200)
	shOff.mu.Lock()
	if got := shOff.nowSample; got != 100 {
		shOff.mu.Unlock()
		t.Fatalf("nowSample = %d with the option off, want 100 (no store)", got)
	}
	shOff.mu.Unlock()
}
