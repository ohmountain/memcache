package memcache

import (
	"math"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// These tests pin the B1 contract for a foreground write: it never clears every
// expired entry, it removes at most one extra entry to make room, and it only
// ever removes the target key's own expired record. Intermediate state is read
// through snapshotCache, never through Lookup or Size, so observing the cache
// cannot itself perform the purge under test. Every case uses a frozen clock
// and no cleaner: the physical snapshot is the only source of truth.

func TestForegroundExpiryFullInsertReclaimsOne(t *testing.T) {
	const capacity = 100
	m, clock := clockCache(t, capacity)
	for i := 0; i < capacity; i++ {
		m.SetExpire(strconv.Itoa(i), i, 1)
	}
	clock.Advance(time.Second)
	m.Set("new", 1)

	snap := snapshotCache(m)
	if len(snap.Items) != capacity {
		t.Fatalf("physical entries after full-insert = %d, want %d", len(snap.Items), capacity)
	}
	old := 0
	for key := range snap.Items {
		if key != "new" {
			old++
		}
	}
	if old != capacity-1 {
		t.Fatalf("old physical entries = %d, want %d", old, capacity-1)
	}
	if item, ok := snap.Items["new"]; !ok || item.Value != 1 {
		t.Fatalf("new entry = %+v, present=%v", item, ok)
	}
	values, order := liveSnapshot(snap, clock.Now())
	if !reflect.DeepEqual(values, map[string]int{"new": 1}) {
		t.Fatalf("live values = %v, want only the new entry", values)
	}
	if !reflect.DeepEqual(order, []string{"new"}) {
		t.Fatalf("live LRU order = %v, want [new]", order)
	}
	assertInvariants(t, m)
}

func TestForegroundExpirySpareCapacityPreservesOthers(t *testing.T) {
	m, clock := clockCache(t, 4)
	m.SetExpire("a", 1, 1)
	m.SetExpire("b", 2, 1)
	clock.Advance(time.Second)
	m.Set("new", 3)

	snap := snapshotCache(m)
	if len(snap.Items) != 3 {
		t.Fatalf("physical entries = %d, want 3", len(snap.Items))
	}
	if len(snap.Heap) != 2 {
		t.Fatalf("heap records = %d, want the 2 expired records", len(snap.Heap))
	}
	for _, key := range []string{"a", "b"} {
		if _, ok := snap.Items[key]; !ok {
			t.Fatalf("write with spare capacity removed unrelated expired key %q", key)
		}
	}
	values, _ := liveSnapshot(snap, clock.Now())
	if !reflect.DeepEqual(values, map[string]int{"new": 3}) {
		t.Fatalf("live values = %v, want only the new entry", values)
	}
	assertInvariants(t, m)
}

func TestForegroundExpiryUpdatePreservesOthers(t *testing.T) {
	m, clock := clockCache(t, 4)
	m.Set("live", 1)
	m.SetExpire("e1", 2, 1)
	m.SetExpire("e2", 3, 1)
	clock.Advance(time.Second)
	m.SetExpire("live", 9, 3600)

	snap := snapshotCache(m)
	if len(snap.Items) != 3 {
		t.Fatalf("physical entries after updating a live key = %d, want 3", len(snap.Items))
	}
	live, ok := snap.Items["live"]
	if !ok || live.Value != 9 {
		t.Fatalf("updated entry = %+v, present=%v", live, ok)
	}
	if !live.Deadline.After(clock.Now()) {
		t.Fatalf("updated entry deadline %v is not in the future", live.Deadline)
	}
	if len(snap.Order) == 0 || snap.Order[0] != "live" {
		t.Fatalf("LRU order = %v, want updated key at the front", snap.Order)
	}
	for _, key := range []string{"e1", "e2"} {
		item, ok := snap.Items[key]
		if !ok {
			t.Fatalf("update removed unrelated expired key %q", key)
		}
		if item.Deadline.After(clock.Now()) {
			t.Fatalf("unrelated key %q deadline %v moved into the future", key, item.Deadline)
		}
	}
	if len(snap.Heap) != 3 {
		t.Fatalf("heap records = %d, want 3 (one per expiring entry)", len(snap.Heap))
	}
	assertInvariants(t, m)
}

func TestForegroundExpiryTargetReinsert(t *testing.T) {
	for _, tc := range []struct {
		name     string
		run      func(*Memcache[int])
		wantHeap int
		expiring bool
	}{
		{"set", func(m *Memcache[int]) { m.Set("target", 10) }, 1, false},
		{"setexpire", func(m *Memcache[int]) { m.SetExpire("target", 10, 3600) }, 2, true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			m, clock := clockCache(t, 4)
			m.SetExpire("target", 1, 1)
			m.SetExpire("other", 2, 1)
			clock.Advance(time.Second)
			tc.run(m)

			snap := snapshotCache(m)
			if len(snap.Items) != 2 {
				t.Fatalf("physical entries = %d, want target rebuilt beside other", len(snap.Items))
			}
			other, ok := snap.Items["other"]
			if !ok {
				t.Fatal("re-inserting the expired target removed the unrelated expired key")
			}
			if other.Deadline.After(clock.Now()) {
				t.Fatalf("unrelated key deadline = %v, want it untouched and expired", other.Deadline)
			}
			target, ok := snap.Items["target"]
			if !ok || target.Value != 10 {
				t.Fatalf("rebuilt target = %+v, present=%v", target, ok)
			}
			if target.Deadline.IsZero() == tc.expiring {
				t.Fatalf("rebuilt target deadline = %v, want expiring=%v", target.Deadline, tc.expiring)
			}
			if len(snap.Order) == 0 || snap.Order[0] != "target" {
				t.Fatalf("LRU order = %v, want rebuilt target at the front", snap.Order)
			}
			if len(snap.Heap) != tc.wantHeap {
				t.Fatalf("heap records = %d, want %d", len(snap.Heap), tc.wantHeap)
			}
			assertInvariants(t, m)
		})
	}
}

func TestForegroundExpiryDeleteOnlyTarget(t *testing.T) {
	for _, ttl := range []int64{0, -1, math.MinInt64} {
		ttl := ttl
		t.Run(strconv.FormatInt(ttl, 10), func(t *testing.T) {
			m, clock := clockCache(t, 4)
			m.SetExpire("keep", 1, 60)
			m.SetExpire("target", 2, 60)
			m.SetExpire("other", 3, 1)
			clock.Advance(time.Second)
			before := snapshotCache(m)

			m.SetExpire("target", 9, ttl)
			snap := snapshotCache(m)
			if _, ok := snap.Items["target"]; ok {
				t.Fatal("nonpositive TTL did not delete its target")
			}
			if len(snap.Items) != 2 {
				t.Fatalf("physical entries = %d, want keep and other untouched", len(snap.Items))
			}
			beforeOther := before.Items["other"]
			other, ok := snap.Items["other"]
			if !ok {
				t.Fatal("nonpositive TTL removed an unrelated expired entry")
			}
			if !other.Deadline.Equal(beforeOther.Deadline) {
				t.Fatalf("unrelated deadline changed: %v -> %v", beforeOther.Deadline, other.Deadline)
			}
			if _, ok := snap.Items["keep"]; !ok {
				t.Fatal("nonpositive TTL removed a live entry")
			}

			// A missing target must not change the physical snapshot at all.
			m.SetExpire("ghost", 9, ttl)
			after := snapshotCache(m)
			if !reflect.DeepEqual(after, snap) {
				t.Fatalf("deleting a missing target changed state:\nbefore=%+v\nafter=%+v", snap, after)
			}
			assertInvariants(t, m)
		})
	}
}

func TestForegroundExpiryPrefersExpiredOverLiveLRU(t *testing.T) {
	m, clock := clockCache(t, 3)
	m.Set("valid", 1)
	m.SetExpire("exp1", 2, 1)
	m.SetExpire("exp2", 3, 1)
	clock.Advance(time.Second)
	before := snapshotCache(m)
	if !reflect.DeepEqual(before.Order, []string{"exp2", "exp1", "valid"}) {
		t.Fatalf("fixture order = %v, want live key at the LRU tail and expired at the front", before.Order)
	}

	m.Set("new", 4)
	snap := snapshotCache(m)
	if len(snap.Items) != 3 {
		t.Fatalf("physical entries = %d, want exactly one slot reclaimed", len(snap.Items))
	}
	if _, ok := snap.Items["valid"]; !ok {
		t.Fatal("a full write evicted the live LRU tail while an expired entry was available")
	}
	expiredPhysical := 0
	for key, item := range snap.Items {
		if key != "new" && !item.Deadline.IsZero() {
			expiredPhysical++
		}
	}
	if expiredPhysical != 1 {
		t.Fatalf("expired physical entries = %d, want exactly one reclaimed", expiredPhysical)
	}
	values, order := liveSnapshot(snap, clock.Now())
	if !reflect.DeepEqual(values, map[string]int{"valid": 1, "new": 4}) {
		t.Fatalf("live values = %v, want valid and new", values)
	}
	if !reflect.DeepEqual(order, []string{"new", "valid"}) {
		t.Fatalf("live LRU order = %v, want [new valid]", order)
	}
	assertInvariants(t, m)
}

func TestForegroundExpiryLiveVictimAndRenewal(t *testing.T) {
	t.Run("live LRU victim", func(t *testing.T) {
		m, _ := clockCache(t, 3)
		m.Set("a", 1)
		m.Set("b", 2)
		m.SetExpire("c", 3, 3600)
		requireOrder(t, m, "c", "b", "a")
		m.Set("d", 4)
		snap := snapshotCache(m)
		if len(snap.Items) != 3 {
			t.Fatalf("physical entries = %d, want 3", len(snap.Items))
		}
		if _, ok := snap.Items["a"]; ok {
			t.Fatal("full write with a future heap root did not evict the LRU tail")
		}
		requireOrder(t, m, "d", "c", "b")
	})

	t.Run("renewal and cancellation", func(t *testing.T) {
		m, clock := clockCache(t, 2)
		m.SetExpire("k", 1, 1)
		m.SetExpire("k", 2, 3600)
		if m.state.expiryLen() != 1 {
			t.Fatalf("renewal left %d expiry records", m.state.expiryLen())
		}
		clock.Advance(time.Second) // past the original deadline
		requireValue(t, m, "k", 2)
		m.Set("k", 3)
		if m.state.expiryLen() != 0 {
			t.Fatal("plain Set did not cancel the TTL record")
		}
		clock.Advance(time.Hour)
		requireValue(t, m, "k", 3)
	})

	t.Run("huge TTL", func(t *testing.T) {
		m, clock := clockCache(t, 1)
		m.SetExpire("k", 3, math.MaxInt64)
		clock.Advance(time.Hour)
		requireValue(t, m, "k", 3)
	})

	t.Run("TTL disabled", func(t *testing.T) {
		clock := &testClock{now: modelEpoch}
		m := newMemcache[int](2, false, clock.Now, false)
		defer m.Close()
		m.SetExpire("k", 1, 0) // behaves like Set when TTL is disabled
		requireValue(t, m, "k", 1)
		clock.Advance(2 * time.Hour)
		requireValue(t, m, "k", 1)
		assertInvariants(t, m)
	})

	t.Run("zero capacity", func(t *testing.T) {
		clock := &testClock{now: modelEpoch}
		m := newMemcache[int](0, true, clock.Now, false)
		defer m.Close()
		m.Set("a", 1)
		m.SetExpire("b", 2, 1)
		if snap := snapshotCache(m); len(snap.Items) != 0 {
			t.Fatalf("zero-capacity cache stored %d entries", len(snap.Items))
		}
		assertInvariants(t, m)
	})
}
