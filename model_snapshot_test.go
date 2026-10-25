package memcache

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

// snapshotItem is a frozen copy of one physical node: value, deadline, and the
// node's index in the expiry heap at snapshot time (-1 when not expiring).
type snapshotItem struct {
	Value       int
	Deadline    time.Time
	ExpiryIndex int
}

// cacheSnapshot is a lock-held copy of the complete physical state. Taking it
// must not purge, refresh recency, or read the clock.
type cacheSnapshot struct {
	Capacity uint
	Closed   bool
	Items    map[string]snapshotItem
	Order    []string // most-recently-used first
	Heap     []string // keys in expiry-heap array order
}

func snapshotCache(m *Memcache[int]) cacheSnapshot {
	s := m.state
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := cacheSnapshot{
		Capacity: s.capacity,
		Closed:   s.closed,
		Items:    make(map[string]snapshotItem, len(s.holder)),
		Order:    make([]string, 0, s.order.Len()),
		Heap:     make([]string, 0, len(s.expiry)),
	}
	for e := s.order.Front(); e != nil; e = e.Next() {
		n := e.Value.(*node[int])
		snap.Items[n.key] = snapshotItem{Value: n.value, Deadline: n.expiresAt, ExpiryIndex: n.expiryIndex}
		snap.Order = append(snap.Order, n.key)
	}
	for _, n := range s.expiry {
		snap.Heap = append(snap.Heap, n.key)
	}
	return snap
}

// liveSnapshot filters a snapshot to entries valid at now, preserving LRU
// order. A zero deadline means no TTL and never expires. The snapshot is not
// modified.
func liveSnapshot(s cacheSnapshot, now time.Time) (map[string]int, []string) {
	values := make(map[string]int, len(s.Items))
	order := make([]string, 0, len(s.Order))
	for _, key := range s.Order {
		item := s.Items[key]
		if item.Deadline.IsZero() || item.Deadline.After(now) {
			values[key] = item.Value
			order = append(order, key)
		}
	}
	return values, order
}

func snapshotEpochClock() *testClock {
	return &testClock{now: modelEpoch}
}

func TestSnapshotPreservesExpiredPhysicalEntries(t *testing.T) {
	clock := snapshotEpochClock()
	m := newMemcache[int](2, true, clock.Now, false)
	defer m.Close()
	m.SetExpire("a", 1, 1)
	m.Set("b", 2)
	clock.Advance(time.Second)
	before := snapshotCache(m)
	after := snapshotCache(m)
	if len(before.Items) != 2 || !reflect.DeepEqual(before, after) {
		t.Fatal("observer changed state")
	}
	values, order := liveSnapshot(after, clock.Now())
	if !reflect.DeepEqual(values, map[string]int{"b": 2}) || !reflect.DeepEqual(order, []string{"b"}) {
		t.Fatal("expired logical view")
	}
	if before.Items["a"].Deadline.IsZero() || !before.Items["b"].Deadline.IsZero() {
		t.Fatal("deadline not captured per entry")
	}
	if before.Items["a"].ExpiryIndex == -1 || before.Items["b"].ExpiryIndex != -1 {
		t.Fatal("heap membership not captured")
	}
	assertInvariants(t, m)
}

func TestSnapshotDoesNotAliasState(t *testing.T) {
	clock := snapshotEpochClock()
	m := newMemcache[int](3, true, clock.Now, false)
	defer m.Close()
	m.Set("a", 1)
	m.SetExpire("b", 2, 60)
	first := snapshotCache(m)
	if len(first.Heap) == 0 {
		t.Fatal("expiring entry missing from heap view")
	}
	first.Items["a"] = snapshotItem{Value: -1}
	first.Items["ghost"] = snapshotItem{Value: -1}
	first.Order[0] = "mutated"
	first.Heap[0] = "mutated"
	second := snapshotCache(m)
	if second.Items["a"].Value != 1 || second.Items["b"].Value != 2 || len(second.Items) != 2 {
		t.Fatalf("snapshot aliased cache entries: %v", second.Items)
	}
	if !reflect.DeepEqual(second.Order, []string{"b", "a"}) {
		t.Fatalf("snapshot aliased LRU order: %v", second.Order)
	}
	if !reflect.DeepEqual(second.Heap, []string{"b"}) {
		t.Fatalf("snapshot aliased heap view: %v", second.Heap)
	}
	if second.Items["b"].Deadline.IsZero() || !second.Items["a"].Deadline.IsZero() {
		t.Fatal("snapshot lost deadline information")
	}
	assertInvariants(t, m)
}

// validateSnapshot checks a frozen copy is internally consistent. It may run
// on a snapshotter goroutine, so it reports with t.Errorf instead of t.Fatal.
func validateSnapshot(t *testing.T, s cacheSnapshot) {
	t.Helper()
	if uint(len(s.Items)) > s.Capacity {
		t.Errorf("snapshot holds %d items over capacity %d", len(s.Items), s.Capacity)
		return
	}
	fail := func(format string, args ...interface{}) {
		t.Errorf(format, args...)
	}
	seen := make(map[string]bool, len(s.Order))
	for i, key := range s.Order {
		if seen[key] {
			fail("duplicate key %q at order %d", key, i)
		}
		seen[key] = true
		item, present := s.Items[key]
		if !present {
			fail("order key %q missing from items", key)
			continue
		}
		if item.Deadline.IsZero() != (item.ExpiryIndex == -1) {
			fail("key %q deadline and heap membership disagree", key)
		}
	}
	for key := range s.Items {
		if !seen[key] {
			fail("item key %q missing from order", key)
		}
	}
	heapMembers := 0
	for key, item := range s.Items {
		if item.ExpiryIndex == -1 {
			continue
		}
		heapMembers++
		if item.ExpiryIndex >= len(s.Heap) || s.Heap[item.ExpiryIndex] != key {
			fail("key %q expiry index %d does not match heap view", key, item.ExpiryIndex)
		}
	}
	if heapMembers != len(s.Heap) {
		fail("heap view has %d entries but %d items claim membership", len(s.Heap), heapMembers)
	}
}

func TestSnapshotConcurrentMutation(t *testing.T) {
	const capacity = 8
	m := testCache(t, capacity, true)
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 2000; i++ {
			key := fmt.Sprintf("k%d", i%12)
			if i%3 == 0 {
				m.Delete(key)
			} else {
				m.Set(key, i)
			}
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 2000; i++ {
			validateSnapshot(t, snapshotCache(m))
		}
	}()
	close(start)
	wg.Wait()
	validateSnapshot(t, snapshotCache(m))
	assertInvariants(t, m)
}
