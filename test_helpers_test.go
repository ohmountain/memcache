package memcache

import (
	"runtime"
	"testing"
	"time"
)

// Test-only seams. Keeping them here removes them from the shipped package
// surface while preserving the same-package access the model/cleaner tests need.

// checkExpired simulates the foreground expiry pass.
func (s *cacheState[T]) checkExpired() {
	_ = s.Size()
}

func (m *Memcache[T]) checkExpired() {
	m.state.checkExpired()
	runtime.KeepAlive(m)
}

// purgeExpiredBatch drives the cleaner primitive with a model clock.
func (s *cacheState[T]) purgeExpiredBatch(now time.Time, limit int) int {
	removed := s.purgeDueBatch(s.nanosAt(now), limit)
	if removed > 0 {
		s.notifyCleaner()
	}
	return removed
}

// expiryLen reports pending heap records across shards.
func (s *cacheState[T]) expiryLen() int {
	_, total := s.deadlineSnapshot()
	return total
}

// liveExpiryLen counts authoritative expiry records across shards; stale
// records left by the lazy heap are excluded.
func (s *cacheState[T]) liveExpiryLen() int {
	total := 0
	for _, sh := range s.shards {
		sh.mu.Lock()
		total += sh.heapLive
		sh.mu.Unlock()
	}
	return total
}

// hasKey checks physical presence without touching recency or expiry.
func (s *cacheState[T]) hasKey(key string) bool {
	sh := s.shardFor(key)
	sh.mu.Lock()
	_, ok := sh.m[key]
	sh.mu.Unlock()
	return ok
}

func (s *cacheState[T]) workerIsStarted() bool {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	return s.started
}

// rebuildHeapLocked restores the lazy heap after a fixture rewrites deadlines
// out of band: every deadlined node gets a fresh generation and record, which
// also invalidates anything the fixture left in the array.
func (s *shard[T]) rebuildHeapLocked() {
	s.heap = s.heap[:0]
	for _, n := range s.m {
		if n.deadline != 0 {
			n.heapGen++
			s.heap.push(n, n.deadline)
		}
	}
	s.heapLive = len(s.heap)
}

// shardedClockCache builds a deterministic TTL cache with no background worker.
func shardedClockCache(t *testing.T, capacity uint, shards int) (*Memcache[int], *testClock) {
	t.Helper()
	clock := &testClock{now: modelEpoch}
	m := newMemcacheWithClockShards[int](capacity, true, systemClock{now: clock.Now}, false, shards, shardSlotTarget, false, true, false)
	t.Cleanup(m.Close)
	return m, clock
}
