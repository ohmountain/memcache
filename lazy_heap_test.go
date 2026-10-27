package memcache

import (
	"fmt"
	"testing"
	"time"
)

// liveRecordCount counts heap records whose generation still matches their
// node (the authoritative record) under the shard lock.
func liveRecordCount(t *testing.T, m *Memcache[int], key string) int {
	t.Helper()
	sh := m.state.shards[0]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	n, ok := sh.m[key]
	if !ok {
		t.Fatalf("key %q missing from the map", key)
	}
	live := 0
	for _, rec := range sh.heap {
		if rec.n == n && rec.live() {
			live++
		}
	}
	return live
}

// TestSetAfterExpireSurvivesStaleRoot: overwriting a TTL entry with Set
// cancels its deadline but keeps the entry; the cancelled record left in the
// heap must be discarded when it surfaces, without evicting the live entry.
// The cancelled record sits behind a later-expiring root, so it can only
// surface through a purge pass — exactly the path a stale-record bug breaks.
func TestSetAfterExpireSurvivesStaleRoot(t *testing.T) {
	m, clock := clockCache(t, 4)
	m.SetExpire("a", 1, 5)
	m.SetExpire("b", 2, 1)
	m.Set("a", 3) // cancel a; its stale 5s record is now below b's live root

	clock.Advance(2 * time.Second)
	if removed := m.state.purgeDueBatch(int64(2*time.Second), 1<<30); removed != 1 {
		t.Fatalf("purgeDueBatch removed %d entries, want 1 (only b)", removed)
	}
	requireValue(t, m, "a", 3)
	requireMissing(t, m, "b")

	clock.Advance(4 * time.Second) // past a's stale record deadline
	if removed := m.state.purgeDueBatch(int64(6*time.Second), 1<<30); removed != 0 {
		t.Fatalf("purgeDueBatch removed %d live entries, want 0 (only a stale record)", removed)
	}
	requireValue(t, m, "a", 3)
	if got := m.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1", got)
	}
	if live := liveRecordCount(t, m, "a"); live != 0 {
		t.Fatalf("cancelled entry still has %d live heap records, want 0", live)
	}
	assertInvariants(t, m)
}

// TestRenewalLeavesOneLiveRecord: renewing one key many times must leave
// exactly one authoritative record and a heap bounded by the compaction rule.
func TestRenewalLeavesOneLiveRecord(t *testing.T) {
	m, clock := clockCache(t, 16)
	m.SetExpire("r", 1, 60)
	for i := 0; i < 9; i++ {
		m.SetExpire(fmt.Sprintf("o%d", i), i, 3600)
	}
	for i := 0; i < 200; i++ {
		clock.Advance(time.Millisecond)
		m.SetExpire("r", i, 60)
	}
	if live := liveRecordCount(t, m, "r"); live != 1 {
		t.Fatalf("renewed key has %d live heap records, want 1", live)
	}
	sh := m.state.shards[0]
	sh.mu.Lock()
	heapLen, mapLen := len(sh.heap), len(sh.m)
	sh.mu.Unlock()
	if heapLen >= minHeapCompactLen && heapLen > maxHeapRecordsPerEntry*mapLen {
		t.Fatalf("heap holds %d records for %d entries past the compaction trigger", heapLen, mapLen)
	}
	assertInvariants(t, m)
}

// TestFreelistReuseDoesNotResurrectRecords: nodes cycled through expiry,
// purge, and reinsertion must never be reachable through an old record.
func TestFreelistReuseDoesNotResurrectRecords(t *testing.T) {
	m, clock := clockCache(t, 8)
	for cycle := 0; cycle < 5; cycle++ {
		for i := 0; i < 8; i++ {
			m.SetExpire(fmt.Sprintf("c%d-%d", cycle, i), i, 1)
		}
		clock.Advance(2 * time.Second)
		m.state.purgeDueBatch(int64(2*time.Second), 1<<30)
		assertInvariants(t, m)
	}
	sh := m.state.shards[0]
	sh.mu.Lock()
	free := make(map[*node[int]]bool, len(sh.free))
	for _, n := range sh.free {
		free[n] = true
	}
	for _, rec := range sh.heap {
		if free[rec.n] && rec.live() {
			sh.mu.Unlock()
			t.Fatal("a freelist node has a live heap record")
		}
	}
	sh.mu.Unlock()
}

// TestHeapCompactionBound: sustained renewal of a single entry must keep the
// heap at one live record and trigger the bounded-compaction invariant.
func TestHeapCompactionBound(t *testing.T) {
	m, clock := clockCache(t, 16)
	m.SetExpire("a", 1, 3600)
	for i := 0; i < 300; i++ {
		clock.Advance(time.Millisecond)
		m.SetExpire("a", i, 3600)
	}
	if live := liveRecordCount(t, m, "a"); live != 1 {
		t.Fatalf("renewed entry has %d live records, want 1", live)
	}
	sh := m.state.shards[0]
	sh.mu.Lock()
	heapLen := len(sh.heap)
	sh.mu.Unlock()
	if heapLen >= minHeapCompactLen && heapLen > maxHeapRecordsPerEntry*len(sh.m) {
		t.Fatalf("heap holds %d records, compaction bound violated", heapLen)
	}
	assertInvariants(t, m)
}
