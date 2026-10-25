package memcache

import (
	"flag"
	"fmt"
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
	for _, capacity := range capacities {
		for _, target := range []string{"FirstSet", "FirstLookup", "Size"} {
			capacity, target := capacity, target
			b.Run(fmt.Sprintf("%s-cap%d", target, capacity), func(b *testing.B) {
				if b.N != 1 {
					b.Skipf("requires exactly one iteration, got N=%d; use -benchtime=1x", b.N)
				}
				m, clock := prepareExpiryBenchmark(b, capacity)
				b.ResetTimer()
				clock.Advance(time.Second)
				switch target {
				case "FirstSet":
					m.Set("new", 1)
				case "FirstLookup":
					m.Lookup("0")
				case "Size":
					m.Size()
				}
				b.StopTimer()
				snap := snapshotCache(m)
				switch target {
				case "FirstSet":
					if len(snap.Items) != 1 || snap.Items["new"].Value != 1 {
						b.Fatalf("after first Set over expired entries: %d items", len(snap.Items))
					}
				case "FirstLookup":
					if len(snap.Items) != capacity-1 {
						b.Fatalf("lookup must remove only its target: %d of %d remain", len(snap.Items), capacity)
					}
				case "Size":
					if len(snap.Items) != 0 {
						b.Fatalf("Size must purge every expired entry: %d remain", len(snap.Items))
					}
				}
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
