package memcache

import (
	"reflect"
	"sync/atomic"
	"testing"
)

func assertInvariants(t testing.TB, m *Memcache[int]) {
	t.Helper()
	s := m.state
	total := 0
	for _, sh := range s.shards {
		sh.mu.Lock()
		size := int(atomic.LoadUint32(&sh.size))
		if size != len(sh.m) || size > sh.cap {
			sh.mu.Unlock()
			t.Fatalf("inconsistent shard size: map=%d size=%d cap=%d", len(sh.m), size, sh.cap)
		}
		total += size
		// Heap pass first: order by the record's own deadline snapshot, plus
		// a live-record census used by the list walk below. A record is live
		// iff its generation still matches its node; stale records may linger
		// up to the compaction bound.
		live := make(map[*node[int]]int)
		for i, rec := range sh.heap {
			if i > 0 && rec.deadline < sh.heap[(i-1)/2].deadline {
				sh.mu.Unlock()
				t.Fatal("expiry heap is out of order")
			}
			if rec.gen == rec.n.heapGen {
				live[rec.n]++
			}
		}
		if len(sh.heap) >= minHeapCompactLen && len(sh.heap) > maxHeapRecordsPerEntry*len(sh.m) {
			sh.mu.Unlock()
			t.Fatalf("expiry heap holds %d records for %d entries past the compaction bound", len(sh.heap), len(sh.m))
		}
		liveTotal := 0
		for _, count := range live {
			liveTotal += count
		}
		if liveTotal != sh.heapLive {
			sh.mu.Unlock()
			t.Fatalf("heapLive = %d but %d live records present", sh.heapLive, liveTotal)
		}
		seen := make(map[string]bool, len(sh.m))
		var prev *node[int]
		for n := sh.head; n != nil; n = n.next {
			if n.prev != prev {
				sh.mu.Unlock()
				t.Fatal("LRU links disagree")
			}
			if seen[n.key] || sh.m[n.key] != n {
				sh.mu.Unlock()
				t.Fatal("list and index disagree")
			}
			seen[n.key] = true
			if count := live[n]; (n.deadline == 0) != (count == 0) || count > 1 {
				sh.mu.Unlock()
				t.Fatalf("key %q has %d live records with deadline %d", n.key, count, n.deadline)
			}
			prev = n
		}
		if sh.tail != prev {
			sh.mu.Unlock()
			t.Fatal("LRU tail disagrees")
		}
		if len(seen) != len(sh.m) {
			sh.mu.Unlock()
			t.Fatalf("list holds %d entries, map holds %d", len(seen), len(sh.m))
		}
		for n := range live {
			if !seen[n.key] {
				sh.mu.Unlock()
				t.Fatal("live expiry record for a node outside the list")
			}
		}
		sh.mu.Unlock()
	}
	if uint(total) > s.capacity {
		t.Fatalf("cache holds %d entries over capacity %d", total, s.capacity)
	}
}

func requireOrder(t *testing.T, m *Memcache[int], want ...string) {
	t.Helper()
	if len(m.state.shards) != 1 {
		t.Fatalf("requireOrder is only meaningful for one shard, got %d", len(m.state.shards))
	}
	assertInvariants(t, m)
	var got []string
	for _, sh := range m.state.shards {
		sh.mu.Lock()
		for n := sh.head; n != nil; n = n.next {
			got = append(got, n.key)
		}
		sh.mu.Unlock()
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LRU order = %v, want %v", got, want)
	}
}
