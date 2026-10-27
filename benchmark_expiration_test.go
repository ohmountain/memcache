package memcache

import (
	"flag"
	"fmt"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// prepareExpiryBenchmark fills a TTL cache with count entries sharing one
// deadline (now + 1s) on a frozen private clock with no cleaner. The caller
// advances the clock explicitly.
func prepareExpiryBenchmark(tb testing.TB, count int) (*Memcache[int], *testClock) {
	tb.Helper()
	clock := &testClock{now: modelEpoch}
	m := newMemcache[int](uint(count), true, clock.Now, false)
	tb.Cleanup(m.Close)
	for i := 0; i < count; i++ {
		m.SetExpire(strconv.Itoa(i), i, 1)
	}
	return m, clock
}

// BenchmarkCacheExpiry measures the first Set, first Lookup, and Size after
// every entry expired at once. Each target operation runs exactly once on a
// fresh fixture: fill, advance, verification, and Close stay outside the
// timer. Adaptive b.N would refill 100K/1M caches repeatedly, so only an
// explicit -benchtime=1x may run it.
func BenchmarkCacheExpiry(b *testing.B) {
	benchtime := ""
	if f := flag.Lookup("test.benchtime"); f != nil {
		benchtime = f.Value.String()
	}
	if benchtime != "1x" {
		b.Skipf("expiry benchmarks require -benchtime=1x (one operation per fixture); benchtime=%q", benchtime)
	}
	capacities := []int{1024, 100000}
	if *cachePerfFull {
		capacities = append(capacities, 1000000)
	}
	// Each target pairs its measured operation with the post-op state it
	// must leave behind. The checks are deliberately strategy-neutral: the
	// live view and capacity bound must hold for both a full purge and a
	// bounded foreground reclamation. The exact physical count that B1
	// guarantees is pinned by foreground_expiration_test.go instead.
	targets := []struct {
		name   string
		run    func(m *Memcache[int])
		verify func(b *testing.B, snap cacheSnapshot, capacity int, now time.Time)
	}{
		{
			name: "FirstSet",
			run:  func(m *Memcache[int]) { m.Set("new", 1) },
			verify: func(b *testing.B, snap cacheSnapshot, capacity int, now time.Time) {
				values, order := liveSnapshot(snap, now)
				if !reflect.DeepEqual(values, map[string]int{"new": 1}) || !reflect.DeepEqual(order, []string{"new"}) {
					b.Fatalf("after first Set over expired entries: live values=%v order=%v", values, order)
				}
				if uint(len(snap.Items)) > uint(capacity) {
					b.Fatalf("after first Set over expired entries: %d physical items over capacity %d", len(snap.Items), capacity)
				}
			},
		},
		{
			name: "FirstLookup",
			run:  func(m *Memcache[int]) { m.Lookup("0") },
			verify: func(b *testing.B, snap cacheSnapshot, capacity int, now time.Time) {
				if len(snap.Items) != capacity-1 {
					b.Fatalf("lookup must remove only its target: %d of %d remain", len(snap.Items), capacity)
				}
			},
		},
		{
			name: "Size",
			run:  func(m *Memcache[int]) { m.Size() },
			verify: func(b *testing.B, snap cacheSnapshot, capacity int, now time.Time) {
				if len(snap.Items) != 0 {
					b.Fatalf("Size must purge every expired entry: %d remain", len(snap.Items))
				}
			},
		},
	}
	for _, capacity := range capacities {
		for _, target := range targets {
			capacity, target := capacity, target
			b.Run(fmt.Sprintf("%s-cap%d", target.name, capacity), func(b *testing.B) {
				if b.N != 1 {
					b.Skipf("requires exactly one iteration, got N=%d; use -benchtime=1x", b.N)
				}
				m, clock := prepareExpiryBenchmark(b, capacity)
				clock.Advance(time.Second)
				b.ResetTimer()
				target.run(m)
				b.StopTimer()
				target.verify(b, snapshotCache(m), capacity, clock.Now())
				assertInvariants(b, m)
			})
		}
	}
}

func TestExpiryFixtureHasOneDeadline(t *testing.T) {
	m, clock := prepareExpiryBenchmark(t, 16)
	snap := snapshotCache(m)
	if len(snap.Items) != 16 {
		t.Fatalf("physical entries = %d, want 16", len(snap.Items))
	}
	want := clock.Now().Add(time.Second)
	for key, item := range snap.Items {
		if !item.Deadline.Equal(want) {
			t.Fatalf("key %q deadline %v, want %v", key, item.Deadline, want)
		}
	}
	if len(snap.Heap) != 16 {
		t.Fatalf("heap records = %d, want 16", len(snap.Heap))
	}
	assertInvariants(t, m)
	clock.Advance(time.Second)
	snap = snapshotCache(m)
	if len(snap.Items) != 16 {
		t.Fatal("advancing the clock must not remove physical entries")
	}
	values, order := liveSnapshot(snap, clock.Now())
	if len(values) != 0 || len(order) != 0 {
		t.Fatalf("live view after deadline: values=%v order=%v", values, order)
	}
	assertInvariants(t, m)
}
