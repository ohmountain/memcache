package memcache

import (
	"reflect"
	"testing"
)

func assertInvariants(t testing.TB, m *Memcache[int]) {
	t.Helper()
	s := m.state
	total := 0
	for _, sh := range s.shards {
		sh.mu.Lock()
		if sh.size != len(sh.m) || sh.size > sh.cap {
			sh.mu.Unlock()
			t.Fatalf("inconsistent shard size: map=%d size=%d cap=%d", len(sh.m), sh.size, sh.cap)
		}
		total += sh.size
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
			if (n.deadline == 0) != (n.expiryIndex == -1) {
				sh.mu.Unlock()
				t.Fatal("deadline and heap membership disagree")
			}
			if n.expiryIndex >= 0 && (n.expiryIndex >= len(sh.heap) || sh.heap[n.expiryIndex] != n) {
				sh.mu.Unlock()
				t.Fatal("incorrect expiry index")
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
		for i, n := range sh.heap {
			if n.expiryIndex != i || sh.m[n.key] != n || n.deadline == 0 {
				sh.mu.Unlock()
				t.Fatal("stale expiry entry")
			}
			if i > 0 && n.deadline < sh.heap[(i-1)/2].deadline {
				sh.mu.Unlock()
				t.Fatal("expiry heap is out of order")
			}
		}
		if len(sh.heap) > len(sh.m) {
			sh.mu.Unlock()
			t.Fatal("expiry metadata exceeds shard size")
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
