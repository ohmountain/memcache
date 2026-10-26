package memcache

import (
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

func TestNonTTLSetDoesNotReadClock(t *testing.T) {
	var calls int64
	now := func() time.Time {
		atomic.AddInt64(&calls, 1)
		return modelEpoch
	}
	m := newMemcache[int](4, false, now, false)
	defer m.Close()
	m.Set("a", 1)
	m.Lookup("a")
	m.Get("a")
	if got := atomic.LoadInt64(&calls); got != 0 {
		t.Fatalf("non-TTL operations read the clock %d times", got)
	}
}

func TestTTLWriteWithoutDeadlineDoesNotReadClock(t *testing.T) {
	var calls int64
	now := func() time.Time {
		atomic.AddInt64(&calls, 1)
		return modelEpoch
	}
	m := newMemcache[int](4, true, now, false)
	defer m.Close()
	// The constructor samples once for the monotonic base.
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("constructor clock calls = %d, want 1", got)
	}
	m.Set("a", 1)
	m.Lookup("a")
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("plain Set on a TTL cache with no deadlines read the clock %d times", got-1)
	}
	m.SetExpire("a", 2, 60)
	if got := atomic.LoadInt64(&calls); got <= 1 {
		t.Fatal("SetExpire did not read the clock")
	}
}

func TestCleanerStartsLazily(t *testing.T) {
	m := WithLRU[int](8, true)
	if m.state.workerIsStarted() {
		t.Fatal("worker started before the first TTL write")
	}
	m.Set("a", 1)
	if m.state.workerIsStarted() {
		t.Fatal("worker started for a non-expiring write")
	}
	m.SetExpire("a", 2, 60)
	if !m.state.workerIsStarted() {
		t.Fatal("worker did not start on the first TTL write")
	}
	m.Close()

	// Closing a TTL cache that never started a worker must not hang, and must
	// not have allocated the walker's channels at all.
	idle := WithLRU[int](8, true)
	idle.Close()
	if idle.state.wake != nil || idle.state.done != nil || idle.state.stopped != nil {
		t.Fatal("a TTL cache that never stored a TTL allocated worker channels")
	}
}

func TestPeekDoesNotRefreshRecency(t *testing.T) {
	m, _ := clockCache(t, 3)
	m.Set("a", 1)
	m.Set("b", 2)
	requireOrder(t, m, "b", "a")
	if v, ok := m.Peek("a"); !ok || v != 1 {
		t.Fatalf("Peek(a) = %d, %v", v, ok)
	}
	requireOrder(t, m, "b", "a")

	stats := m.Stats()
	if stats.Peeks != 1 || stats.PeekHits != 1 {
		t.Fatalf("peek stats = %+v", stats)
	}
}

func TestPeekLeavesExpiredEntryForCleanup(t *testing.T) {
	m, clock := clockCache(t, 4)
	m.SetExpire("a", 1, 1)
	m.Set("b", 2)
	clock.Advance(time.Second)
	if _, ok := m.Peek("a"); ok {
		t.Fatal("Peek returned an expired entry")
	}
	if !m.state.hasKey("a") {
		t.Fatal("Peek removed the expired entry")
	}
	if got := m.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2 physical entries", got)
	}
	if got := m.Size(); got != 1 {
		t.Fatalf("Size = %d, want 1 live entry", got)
	}
	if m.state.hasKey("a") {
		t.Fatal("Size did not purge the expired entry")
	}
}

func TestLenDoesNotPurge(t *testing.T) {
	m, clock := clockCache(t, 4)
	m.SetExpire("a", 1, 1)
	m.Set("b", 2)
	clock.Advance(time.Second)
	if got := m.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
	if got := m.Size(); got != 1 {
		t.Fatalf("Size = %d, want 1", got)
	}
	if got := m.Len(); got != 1 {
		t.Fatalf("Len after purge = %d, want 1", got)
	}
}

func TestSizeChunkedPurge(t *testing.T) {
	const n = 2*sizePurgeBatch + 1 // force at least three lock releases
	m, clock := clockCache(t, n)
	for i := 0; i < n; i++ {
		m.SetExpire(workloadKey(i), i, 1)
	}
	clock.Advance(time.Second)
	if got := m.Size(); got != 0 {
		t.Fatalf("Size = %d, want 0", got)
	}
	if got := m.Len(); got != 0 {
		t.Fatalf("Len = %d after Size, want 0", got)
	}
	assertInvariants(t, m)
}

func TestShardingOptions(t *testing.T) {
	m := WithOptions[int](Options{Capacity: 4096, Shards: 16})
	defer m.Close()
	if got := len(m.state.shards); got != 16 {
		t.Fatalf("shards = %d, want 16", got)
	}
	keys := numericKeys(1024)
	for i, key := range keys {
		m.Set(key, i)
	}
	if got := m.Len(); got != 1024 {
		t.Fatalf("Len = %d, want 1024", got)
	}
	for i, key := range keys {
		if v, ok := m.Lookup(key); !ok || v != i {
			t.Fatalf("Lookup(%q) = %d, %v", key, v, ok)
		}
	}
	stats := m.Stats()
	if stats.Entries != 1024 || stats.Hits != 1024 {
		t.Fatalf("stats = %+v", stats)
	}
	if got := m.Size(); got != 1024 {
		t.Fatalf("Size = %d, want 1024", got)
	}
	assertInvariants(t, m)
	m.Clear()
	if got := m.Len(); got != 0 {
		t.Fatalf("Len after Clear = %d", got)
	}
}

func TestShardingClampsTinyCapacity(t *testing.T) {
	m := WithOptions[int](Options{Capacity: 100, Shards: 16})
	defer m.Close()
	if got := len(m.state.shards); got != 1 {
		t.Fatalf("shards = %d, want 1 for a 100-slot cache", got)
	}
}

func TestShardedTTLExpiry(t *testing.T) {
	m, clock := shardedClockCache(t, 4096, 8)
	if got := len(m.state.shards); got != 8 {
		t.Fatalf("shards = %d, want 8", got)
	}
	for i := 0; i < 1000; i++ {
		m.SetExpire(workloadKey(i), i, 1)
	}
	clock.Advance(time.Second)
	m.checkExpired()
	if got := m.Size(); got != 0 {
		t.Fatalf("Size = %d, want 0", got)
	}
	if got := m.state.expiryLen(); got != 0 {
		t.Fatalf("expiry records = %d, want 0", got)
	}
	assertInvariants(t, m)
}

func TestStatsCounters(t *testing.T) {
	m := WithLRU[int](2, false)
	defer m.Close()

	m.Set("a", 1)
	m.Set("b", 2)
	m.Lookup("a") // hit
	m.Lookup("x") // miss
	m.Peek("a")   // peek hit
	m.Peek("x")   // peek miss
	m.Set("c", 3) // full write: evicts the LRU tail "b"
	m.Delete("a")

	stats := m.Stats()
	if stats.Sets != 3 || stats.Hits != 1 || stats.Misses != 1 || stats.Peeks != 2 || stats.PeekHits != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.Deletes != 1 || stats.Evictions != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.Entries != 1 || stats.Capacity != 2 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestExpirationStats(t *testing.T) {
	m, clock := clockCache(t, 2)
	m.SetExpire("a", 1, 1)
	clock.Advance(time.Second)
	m.Lookup("a")
	if got := m.Stats().Expirations; got != 1 {
		t.Fatalf("expirations = %d, want 1", got)
	}
}

func TestSetExpireDuration(t *testing.T) {
	m, clock := clockCache(t, 2)
	m.SetExpireDuration("a", 1, 500*time.Millisecond)
	clock.Advance(499 * time.Millisecond)
	requireValue(t, m, "a", 1)
	clock.Advance(time.Millisecond)
	requireMissing(t, m, "a")
	assertInvariants(t, m)
}

func TestSetExpireDurationOnNonTTLCache(t *testing.T) {
	clock := &testClock{now: modelEpoch}
	m := newMemcache[int](2, false, clock.Now, false)
	defer m.Close()
	m.SetExpireDuration("a", 1, time.Nanosecond)
	clock.Advance(time.Hour)
	requireValue(t, m, "a", 1)
}

func TestShardedCloseAndClear(t *testing.T) {
	m := WithOptions[int](Options{Capacity: 4096, EnableExpired: true, Shards: 8})
	for i := 0; i < 1000; i++ {
		m.SetExpire(workloadKey(i), i, 60)
	}
	if got := m.Len(); got != 1000 {
		t.Fatalf("Len = %d, want 1000", got)
	}
	assertInvariants(t, m)
	m.Clear()
	if got := m.Len(); got != 0 {
		t.Fatalf("Len after Clear = %d", got)
	}
	for i := 0; i < 1000; i++ {
		m.Set(workloadKey(i), i)
	}
	assertInvariants(t, m)
	m.Close()
	if got := m.Len(); got != 0 {
		t.Fatalf("Len after Close = %d", got)
	}
	if _, ok := m.Lookup(workloadKey(0)); ok {
		t.Fatal("Lookup after Close hit")
	}
	if _, ok := m.Peek(workloadKey(0)); ok {
		t.Fatal("Peek after Close hit")
	}
}

func TestZeroCapacityWithOptions(t *testing.T) {
	m := WithOptions[int](Options{Capacity: 0, EnableExpired: true, Shards: 8})
	defer m.Close()
	m.Set("a", 1)
	m.SetExpire("b", 2, 1)
	if got := m.Len(); got != 0 {
		t.Fatalf("Len = %d, want 0", got)
	}
	if got := m.Size(); got != 0 {
		t.Fatalf("Size = %d, want 0", got)
	}
}

func TestShardedConcurrent(t *testing.T) {
	const (
		workers    = 8
		operations = 3000
		keySpace   = 2048 // below capacity, so this exercises routing, not eviction
	)
	m := WithOptions[int](Options{Capacity: 4096, EnableExpired: true, Shards: workers})
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < operations; i++ {
				key := workloadKey((w*7919 + i*31) % keySpace)
				switch i % 8 {
				case 0:
					m.Set(key, i)
				case 1:
					m.SetExpire(key, i, 1)
				case 2:
					m.Lookup(key)
				case 3:
					m.Peek(key)
				case 4:
					m.Delete(key)
				case 5:
					if got := m.Len(); got > 4096 {
						t.Errorf("Len = %d over capacity", got)
						return
					}
				case 6:
					_ = m.Stats()
				case 7:
					if got := m.Size(); got > 4096 {
						t.Errorf("Size = %d over capacity", got)
						return
					}
				}
			}
		}(w)
	}
	wg.Wait()
	assertInvariants(t, m)
	m.Close()
	assertInvariants(t, m)
}

func TestShardedDifferential(t *testing.T) {
	m, clock := shardedClockCache(t, 4096, 16)
	if got := len(m.state.shards); got != 16 {
		t.Fatalf("shards = %d, want 16", got)
	}

	// diffEntry is local because the sharded cache models per-shard expiry,
	// not the global-capacity model used by model_state_test.go.
	type diffEntry struct {
		value    int
		deadline int64 // nanoseconds since modelEpoch; 0 means no TTL
	}
	live := make(map[string]diffEntry)
	now := int64(0)
	rng := uint64(1)
	const (
		lcgMultiplier = 6364136223846793005
		lcgIncrement  = 1442695040888963407
	)
	next := func(n int) int {
		rng = rng*lcgMultiplier + lcgIncrement
		return int((rng >> 33) % uint64(n))
	}
	check := func(step int) {
		t.Helper()
		if got, want := m.Len(), uint(len(live)); got != want {
			t.Fatalf("step %d: Len = %d, want %d", step, got, want)
		}
		if got := m.Stats().Entries; got != uint(len(live)) {
			t.Fatalf("step %d: Stats.Entries = %d, want %d", step, got, len(live))
		}
	}

	for i := 0; i < 3000; i++ {
		key := workloadKey(next(512))
		switch next(6) {
		case 0, 1:
			value := next(1000)
			ttl := next(3)
			if ttl == 0 {
				m.Set(key, value)
				live[key] = diffEntry{value: value}
				break
			}
			m.SetExpire(key, value, int64(ttl))
			live[key] = diffEntry{value: value, deadline: now + int64(ttl)*int64(time.Second)}
		case 2:
			entry, exists := live[key]
			if exists && entry.deadline != 0 && entry.deadline <= now {
				delete(live, key)
				exists = false
			}
			got, ok := m.Lookup(key)
			if ok != exists || (ok && got != entry.value) {
				t.Fatalf("step %d: Lookup(%q) = %d, %v; model = %+v, exists=%v",
					i, key, got, ok, entry, exists)
			}
		case 3:
			m.Delete(key)
			delete(live, key)
		case 4:
			other := workloadKey(next(512))
			entry, exists := live[other]
			expired := exists && entry.deadline != 0 && entry.deadline <= now
			got, ok := m.Peek(other)
			if ok != (exists && !expired) || (ok && got != entry.value) {
				t.Fatalf("step %d: Peek(%q) = %d, %v; model = %+v, exists=%v, expired=%v",
					i, other, got, ok, entry, exists, expired)
			}
		case 5:
			clock.Advance(time.Second)
			now += int64(time.Second)
			for k, entry := range live {
				if entry.deadline != 0 && entry.deadline <= now {
					delete(live, k)
				}
			}
			if got, want := m.Size(), uint(len(live)); got != want {
				t.Fatalf("step %d: Size = %d, want %d", i, got, want)
			}
		}
		check(i)
	}

	for key, entry := range live {
		got, ok := m.Lookup(key)
		if !ok || got != entry.value {
			t.Fatalf("final Lookup(%q) = %d, %v; want %d", key, got, ok, entry.value)
		}
	}
	assertInvariants(t, m)
}

func TestSteadyStateAllocations(t *testing.T) {
	m := WithLRU[int](64, false)
	defer m.Close()
	for i := 0; i < 64; i++ {
		m.Set(workloadKey(i), i)
	}
	hit := workloadKey(0)
	if got := testing.AllocsPerRun(1000, func() { m.Lookup(hit) }); got != 0 {
		t.Fatalf("Lookup allocates %.1f per op, want 0", got)
	}
	if got := testing.AllocsPerRun(1000, func() { m.Set(hit, 1) }); got != 0 {
		t.Fatalf("overwriting Set allocates %.1f per op, want 0", got)
	}
	if got := testing.AllocsPerRun(1000, func() { m.Get(hit) }); got > 1 {
		t.Fatalf("Get allocates %.1f per op, want <=1", got)
	}

	// Alternating two keys in a one-slot cache exercises the evict path.
	evict := WithLRU[int](1, false)
	defer evict.Close()
	evict.Set("a", 1)
	flip := 0
	if got := testing.AllocsPerRun(1000, func() {
		if flip == 0 {
			evict.Set("a", 1)
		} else {
			evict.Set("b", 2)
		}
		flip ^= 1
	}); got != 0 {
		t.Fatalf("evicting Set allocates %.1f per op, want 0", got)
	}

	ttl := newMemcache[int](64, true, time.Now, false)
	defer ttl.Close()
	ttl.SetExpireDuration("k", 1, time.Minute)
	if got := testing.AllocsPerRun(1000, func() {
		ttl.SetExpireDuration("k", 1, time.Minute)
	}); got != 0 {
		t.Fatalf("renewing SetExpireDuration allocates %.1f per op, want 0", got)
	}
}

// TestFootprintBudget is a gross memory-regression guard using the audit's
// cache-only methodology: the key set is allocated before the baseline sample,
// so key strings are excluded. The audit gate is 110 B/entry at 1K; the 1K
// budget is looser because map growth at that size varies by several bytes
// between runs, while the 100K sample is stable enough to catch a layout
// regression. TestIntrusiveLayout pins the exact node size.
func TestFootprintBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		n      int
		budget float64
	}{
		{"1K", 1024, 120},
		{"100K", 100000, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys := numericKeys(tc.n)
			runtime.GC()
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)

			m := WithLRU[uint64](uint(tc.n), false)
			for i, key := range keys {
				m.Set(key, uint64(i))
			}
			runtime.GC()
			runtime.GC()
			runtime.ReadMemStats(&after)
			runtime.KeepAlive(m)
			m.Close()

			perEntry := float64(after.HeapAlloc-before.HeapAlloc) / float64(tc.n)
			t.Logf("cache-only live heap = %.1f B/entry at %d entries", perEntry, tc.n)
			if perEntry > tc.budget {
				t.Fatalf("cache-only live heap = %.1f B/entry at %d entries, want <=%.0f",
					perEntry, tc.n, tc.budget)
			}
		})
	}
}

// TestIdleInstanceFootprint guards the fixed cost of a cache that stores
// nothing, which is the "many small caches" budget: a TTL-enabled cache that
// never writes a TTL must not allocate the cleaner's channels, and a shard must
// stay in the 64-byte-aligned 192-byte class. Measured about 497 B (non-TTL)
// and 494 B (TTL-idle) here on linux/amd64 with Go 1.27, against ~805/910 B
// before the round-3 layout. The 560 B budget leaves about 12% headroom for
// allocator and size-class variation while still failing the old layout, and it
// is tighter than the "1000 idle TTL caches in 0.5 MiB" gate (500 B/instance)
// rather than looser than it.
func TestIdleInstanceFootprint(t *testing.T) {
	const instances = 5000
	for _, tc := range []struct {
		name string
		ctor func() *Memcache[int]
	}{
		{"nonttl", func() *Memcache[int] { return WithLRU[int](1024, false) }},
		{"ttl-idle", func() *Memcache[int] { return WithLRU[int](1024, true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The handle slice is allocated before the baseline sample, so it
			// does not count toward the per-instance figure.
			caches := make([]*Memcache[int], instances)
			runtime.GC()
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)

			for i := range caches {
				caches[i] = tc.ctor()
			}

			runtime.GC()
			runtime.GC()
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			perInstance := float64(after.HeapAlloc-before.HeapAlloc) / float64(instances)

			for _, m := range caches {
				m.Close()
			}
			runtime.KeepAlive(caches)

			t.Logf("idle instance = %.1f B", perInstance)
			if perInstance > 560 {
				t.Fatalf("idle instance = %.1f B, want <=560", perInstance)
			}
		})
	}
}

func TestShardCountRoundsDown(t *testing.T) {
	for _, tc := range []struct {
		capacity  uint
		requested int
		want      int
	}{
		{4096, 0, 1},
		{4096, 1, 1},
		{4096, 2, 2},
		{4096, 3, 2},
		{4096, 5, 4},
		{4096, 6, 4},
		{4096, 7, 4},
		{4096, 8, 8},
		{4096, 64, 64},
		{4096, 65, 64},
		{100, 16, 1},
	} {
		if got := shardCount(tc.capacity, tc.requested); got != tc.want {
			t.Errorf("shardCount(%d, %d) = %d, want %d", tc.capacity, tc.requested, got, tc.want)
		}
	}
}

// TestIntrusiveLayout pins the 64-bit allocations the performance docs depend
// on: a 56-byte node lands in the 64-byte size class and a 192-byte shard lands
// in the 64-byte-aligned class, so one shard can never share a cache line with
// a neighbour.
func TestIntrusiveLayout(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("layout is documented for 64-bit targets")
	}
	if got := unsafe.Sizeof(node[uint64]{}); got != 56 {
		t.Fatalf("node[uint64] = %d bytes, want 56 (64-byte size class)", got)
	}
	// 192 is a size class with 64-byte minimum alignment, so every shard
	// starts on a cache-line boundary and owns exactly three lines.
	if got := unsafe.Sizeof(shard[uint64]{}); got != 192 {
		t.Fatalf("shard[uint64] = %d bytes, want 192 (160-byte body + 32-byte pad)", got)
	}
	// The alignment, not the size, is what the padding buys. Pin it directly:
	// a size-only check would still pass if the body/pad split changed in a way
	// that lost the 64-byte alignment. Each shard must be allocated on its own
	// so it comes from the 192-byte size class; a slice of several shards would
	// be one larger allocation and would not test the class at all. If this
	// fails on a newer toolchain, the 192-byte choice needs revisiting, not the
	// test.
	shards := make([]*shard[uint64], 8)
	for i := range shards {
		shards[i] = new(shard[uint64])
	}
	for i, sh := range shards {
		if off := uintptr(unsafe.Pointer(sh)) % 64; off != 0 {
			t.Fatalf("shard[%d] allocated at offset %d within a cache line, want 0: the "+
				"192-byte layout no longer guarantees 64-byte alignment", i, off)
		}
	}
	runtime.KeepAlive(shards)
}

func TestMassRemovalBoundsFreelist(t *testing.T) {
	m := WithLRU[int](4096, false)
	defer m.Close()
	for i := 0; i < 4096; i++ {
		m.Set(workloadKey(i), i)
	}
	for i := 0; i < 4096; i++ {
		m.Delete(workloadKey(i))
	}
	if got := m.Len(); got != 0 {
		t.Fatalf("Len = %d after deleting all entries", got)
	}
	for _, sh := range m.state.shards {
		sh.mu.Lock()
		got := len(sh.free)
		sh.mu.Unlock()
		if got > maxFreeNodesPerShard {
			t.Fatalf("freelist holds %d nodes, cap is %d", got, maxFreeNodesPerShard)
		}
	}
}

func TestMassExpiryReleasesHeapBacking(t *testing.T) {
	clock := &testClock{now: modelEpoch}
	m := newMemcacheWithClockShards[int](4096, true, systemClock{now: clock.Now}, false, 1, false, true)
	defer m.Close()
	for i := 0; i < 4096; i++ {
		m.SetExpire(workloadKey(i), i, 1)
	}
	clock.Advance(time.Second)
	if got := m.Size(); got != 0 {
		t.Fatalf("Size = %d after expiry", got)
	}
	sh := m.state.shards[0]
	sh.mu.Lock()
	got := cap(sh.heap)
	sh.mu.Unlock()
	if got > maxIdleHeapCap {
		t.Fatalf("empty heap retains capacity %d, cap is %d", got, maxIdleHeapCap)
	}
}

// TestDisableStatsReportsOnlyEntries pins the Options.DisableStats contract:
// operations still work, but only the structural fields are reported. The
// counters exist to be read; when a caller opts out it must not pay a cache-line
// store per operation, so every counter site is gated.
func TestDisableStatsReportsOnlyEntries(t *testing.T) {
	m := WithOptions[int](Options{Capacity: 4, DisableStats: true})
	defer m.Close()
	m.Set("a", 1)
	m.SetExpire("b", 2, 60)
	m.Lookup("a")
	m.Lookup("missing")
	m.Peek("a")
	m.Peek("missing")
	m.Delete("a")
	if got := m.Size(); got != 1 {
		t.Fatalf("Size = %d, want 1 live entry", got)
	}
	stats := m.Stats()
	if stats.Capacity != 4 || stats.Entries != 1 {
		t.Fatalf("structural stats = cap %d entries %d, want 4/1", stats.Capacity, stats.Entries)
	}
	if stats.Hits != 0 || stats.Misses != 0 || stats.Sets != 0 || stats.Deletes != 0 ||
		stats.Evictions != 0 || stats.Expirations != 0 || stats.Peeks != 0 || stats.PeekHits != 0 {
		t.Fatalf("counters recorded with DisableStats: %+v", stats)
	}
}

// TestHashKeyDistribution guards the shard router. A poor avalanche would
// concentrate unrelated keys in one shard, which costs both capacity and
// throughput long before any benchmark notices; the old byte-at-a-time FNV
// loop had to be replaced for speed, so its distribution property needs a test.
func TestHashKeyDistribution(t *testing.T) {
	const (
		keys    = 1 << 16
		buckets = 64
	)
	patterns := map[string]func(int) string{
		"numeric":      func(i int) string { return strconv.Itoa(i) },
		"sharedPrefix": func(i int) string { return "session:0123456789:" + strconv.Itoa(i) },
		"uuidLike": func(i int) string {
			return strconv.FormatInt(int64(i)*2654435761, 16) + "-0123-4567-89ab"
		},
	}
	for name, keyOf := range patterns {
		counts := make([]int, buckets)
		for i := 0; i < keys; i++ {
			counts[hashKey(keyOf(i), 0xfeedfacecafebeef)&(buckets-1)]++
		}
		min, max := counts[0], counts[0]
		for _, c := range counts {
			if c < min {
				min = c
			}
			if c > max {
				max = c
			}
		}
		mean := float64(keys) / buckets
		if float64(max) > mean*1.35 {
			t.Fatalf("%s: busiest shard has %d keys, mean %.1f (min %d)", name, max, mean, min)
		}
		if min == 0 {
			t.Fatalf("%s: an empty shard for %d keys", name, keys)
		}
	}
}
