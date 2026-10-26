package memcache

import (
	"fmt"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestPerformanceRoundsReleaseOwnedCaches(t *testing.T) {
	before := Lives()
	for round := 0; round < 3; round++ {
		roundEntry := Lives()
		caches := make([]*Memcache[int], 0, 2)
		for i := 0; i < 2; i++ {
			m := WithLRU[int](1024, true)
			caches = append(caches, m)
			for k := 0; k < 1024; k++ {
				m.Set(strconv.Itoa(k), k)
			}
		}
		for _, m := range caches {
			m.Close()
		}
		if err := checkCachesReleased(caches, roundEntry); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	if Lives() != before {
		t.Fatalf("rounds leaked registrations: before=%d after=%d", before, Lives())
	}

	// A deliberately unclosed instance must be caught before it pollutes
	// the next round's measurements.
	roundEntry := Lives()
	caches := make([]*Memcache[int], 0, 2)
	for i := 0; i < 2; i++ {
		m := WithLRU[int](1024, true)
		caches = append(caches, m)
		m.Set("k", i)
	}
	caches[0].Close()
	if err := checkCachesReleased(caches, roundEntry); err == nil {
		t.Fatal("checker missed a leaked cache")
	}
	caches[1].Close()
	if err := checkCachesReleased(caches, roundEntry); err != nil {
		t.Fatalf("checker reported released caches: %v", err)
	}
}

// cleanerRunning reports whether a cache's background cleaner has not stopped
// yet. It is the one place that reads the stopped channel.
//
// A nil stopped means no worker was ever started — the cache never held a TTL
// — which is reported as "not running" because there is nothing to join. That
// is the right answer for "has the cleaner stopped", but it is not evidence
// that a cleaner ever existed; a test that needs to prove the worker ran must
// assert that separately (see TestCleanerStartsLazily).
func cleanerRunning[T CacheAble](m *Memcache[T]) bool {
	stopped := m.state.stopped
	if stopped == nil {
		// No worker was ever started, so there is nothing running to join.
		return false
	}
	select {
	case <-stopped:
		return false
	default:
		return true
	}
}

// checkCachesReleased verifies every cache's cleaner stopped and the global
// registry returned to its pre-round count. It returns an error instead of
// failing so tests can also assert that leaks are caught.
func checkCachesReleased[T CacheAble](caches []*Memcache[T], livesBefore int) error {
	for i, m := range caches {
		if cleanerRunning(m) {
			return fmt.Errorf("cache %d cleaner still running", i)
		}
	}
	if got := Lives(); got != livesBefore {
		return fmt.Errorf("registry count = %d, want %d", got, livesBefore)
	}
	return nil
}

// ownedWorkersRunning counts workers of caches we own that have not stopped.
func ownedWorkersRunning[T CacheAble](caches []*Memcache[T]) uint64 {
	var running uint64
	for _, m := range caches {
		if cleanerRunning(m) {
			running++
		}
	}
	return running
}

// readHeapStats reports heap statistics after a forced GC. Forced GC is used
// only for these retention measurements, never inside latency runs.
func readHeapStats() (heapAlloc, heapObjects uint64) {
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc, stats.HeapObjects
}

// runIdleScenario observes capacity-1 TTL instances with an empty heap
// (idle-empty) or one 3600s entry each (idle-future) for a fixed 3s window
// carrying only a 1 rps no-op load.
//
// Both counters come from an instrumented now() (one atomic increment per
// call), which adds that overhead to every sample. empty_heap_checks is the
// total since construction, kept for compatibility with older records: it
// includes one deadline read per initial write plus the worker's startup
// observation, so it is not "periodic wakes". window_now_checks counts only
// calls made inside the observation window, which is the number that shows
// whether an idle instance wakes periodically. Neither counter proves by
// itself that no timer exists; that proof is cleaner_test.go's fake-timer
// event record (TestCleanerEmptyHeapSleeps), which asserts no timer is created
// for an empty heap and no clock read happens without a change.
func runIdleScenario(t *testing.T, base perfRecord, future bool) perfRecord {
	switch *perfInstances {
	case 1, 100, 1000:
	default:
		t.Fatalf("-cache-perf-instances must be 1, 100, or 1000 for idle scenarios, got %d", *perfInstances)
	}
	before := Lives()
	var nowCalls uint64
	countingNow := func() time.Time {
		atomic.AddUint64(&nowCalls, 1)
		return time.Now()
	}
	caches := make([]*Memcache[int], 0, *perfInstances)
	for i := 0; i < *perfInstances; i++ {
		m := newMemcache[int](1, true, countingNow, true)
		caches = append(caches, m)
		if future {
			m.SetExpire("k", i, 3600)
		}
	}
	t.Cleanup(func() {
		for _, m := range caches {
			m.Close()
		}
	})
	cfg := loadConfig{Rate: 1, Duration: 3 * time.Second, Workers: 1, Queue: 8, BurstWindow: burstWindowOf(3 * time.Second)}
	base.Config = cfg
	base.Instances = *perfInstances
	base.ValueType = "int"
	if future {
		base.InitialExpiring = *perfInstances
	}
	windowStart := time.Now()
	windowCallsStart := atomic.LoadUint64(&nowCalls)
	base.Load = runLoadOnceAt(t, cfg, windowStart, func(uint64) loadOutcome { return loadOutcome{} })
	// The last 1 rps request is planned 2s into the 3s window, so the load
	// alone would stop there. The settled tail below starts once the load has
	// finished: workers have long since made their startup observation, so any
	// clock call in the tail would be a periodic wake, not initialization.
	tailCallsStart := atomic.LoadUint64(&nowCalls)
	if remain := time.Until(windowStart.Add(cfg.Duration)); remain > 0 {
		time.Sleep(remain)
	}
	windowCalls := atomic.LoadUint64(&nowCalls) - windowCallsStart
	tailCalls := atomic.LoadUint64(&nowCalls) - tailCallsStart
	heapAlloc, heapObjects := readHeapStats()
	base.Resources = map[string]uint64{
		"heap_alloc_bytes":  heapAlloc,
		"heap_objects":      heapObjects,
		"goroutines":        uint64(runtime.NumGoroutine()),
		"registered_caches": uint64(Lives() - before),
		"owned_workers":     ownedWorkersRunning(caches),
		"empty_heap_checks": atomic.LoadUint64(&nowCalls),
		"window_now_checks": windowCalls,
		"tail_now_checks":   tailCalls,
	}
	t.Logf("idle run: empty_heap_checks=%d total, window_now_checks=%d inside the 3s window, tail_now_checks=%d after the load finished (instrumented now() adds one atomic increment per call)",
		atomic.LoadUint64(&nowCalls), windowCalls, tailCalls)
	return base
}

// runLifecycleScenario creates, fills, and closes rounds of perRound TTL
// caches of capacity 1024, reporting per-round resource logs and aggregate
// resources. The bytes4k variant stores one pre-allocated 4KiB slice per
// entry; payload references stay held during the rounds, then a final GC
// after releasing them delineates payload versus metadata retention. Both
// phases persist in the record: resources_held (payloads pinned) and
// resources (after release).
func runLifecycleScenario(t *testing.T, base perfRecord, rounds, perRound int) perfRecord {
	switch *perfValue {
	case "int", "bytes4k":
	default:
		t.Fatalf("-cache-perf-value must be int or bytes4k for lifecycle, got %q", *perfValue)
	}
	const capacity = 1024
	before := Lives()
	base.ValueType = *perfValue
	base.Capacity = capacity
	base.Instances = rounds * perRound
	keys := make([]string, capacity)
	for k := range keys {
		keys[k] = strconv.Itoa(k)
	}
	closeTotal := time.Duration(0)
	if *perfValue == "int" {
		caches := make([]*Memcache[int], 0, perRound)
		for round := 0; round < rounds; round++ {
			roundEntry := Lives()
			caches = caches[:0]
			for i := 0; i < perRound; i++ {
				m := WithLRU[int](capacity, true)
				caches = append(caches, m)
				for k := 0; k < capacity; k++ {
					m.Set(keys[k], k)
				}
			}
			start := time.Now()
			for _, m := range caches {
				m.Close()
			}
			closeTotal += time.Since(start)
			if err := checkCachesReleased(caches, roundEntry); err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
			alloc, objects := readHeapStats()
			t.Logf("round %d closed: heap_alloc=%d heap_objects=%d goroutines=%d", round, alloc, objects, runtime.NumGoroutine())
		}
	} else {
		caches := make([]*Memcache[[]byte], 0, perRound)
		var retained [][]byte
		for round := 0; round < rounds; round++ {
			roundEntry := Lives()
			caches = caches[:0]
			for i := 0; i < perRound; i++ {
				m := WithLRU[[]byte](capacity, true)
				caches = append(caches, m)
				for k := 0; k < capacity; k++ {
					payload := make([]byte, 4096)
					payload[0] = byte(k)
					retained = append(retained, payload)
					m.Set(keys[k], payload)
				}
			}
			start := time.Now()
			for _, m := range caches {
				m.Close()
			}
			closeTotal += time.Since(start)
			if err := checkCachesReleased(caches, roundEntry); err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
			alloc, objects := readHeapStats()
			t.Logf("round %d closed with payloads retained: heap_alloc=%d heap_objects=%d retained_payloads=%d", round, alloc, objects, len(retained))
		}
		allocHeld, objectsHeld := readHeapStats()
		// KeepAlive pins the payloads through the held sampling: without it
		// the compiler may already consider retained dead and GC could
		// collect the payloads before the "held" numbers are read.
		runtime.KeepAlive(retained)
		// The last use above ends the payloads' liveness; the next GC can
		// collect them before the after-release sample.
		allocFreed, objectsFreed := readHeapStats()
		t.Logf("payload boundary: held alloc=%d objects=%d; released alloc=%d objects=%d", allocHeld, objectsHeld, allocFreed, objectsFreed)
		base.ResourcesHeld = map[string]uint64{
			"heap_alloc_bytes": allocHeld,
			"heap_objects":     objectsHeld,
		}
		base.Resources = map[string]uint64{
			"heap_alloc_bytes": allocFreed,
			"heap_objects":     objectsFreed,
		}
	}
	if Lives() != before {
		t.Fatalf("lifecycle rounds leaked registrations: before=%d after=%d", before, Lives())
	}
	// int variant: resources were not set above; measure after the rounds.
	if base.Resources == nil {
		alloc, objects := readHeapStats()
		base.Resources = map[string]uint64{
			"heap_alloc_bytes": alloc,
			"heap_objects":     objects,
		}
	}
	base.Resources["goroutines"] = uint64(runtime.NumGoroutine())
	base.Resources["registered_caches"] = uint64(Lives() - before)
	base.Resources["owned_workers"] = 0
	base.Resources["reclaim_duration_ns"] = uint64(closeTotal.Nanoseconds())
	t.Logf("lifecycle total close time: %v across %d rounds", closeTotal, rounds)
	return base
}

// TestLifecycleScenarioRecordsPayloadBoundary pins the evidence contract of
// the bytes4k lifecycle run: both heap phases must land in the record — the
// held sample taken while payload references are pinned and the after-release
// sample — with the payload bytes visibly between them. The int variant
// makes no payload claim, so it carries no held map.
func TestLifecycleScenarioRecordsPayloadBoundary(t *testing.T) {
	for _, tc := range []struct {
		value      string
		wantHeld   bool
		payloadMiB uint64
	}{
		{value: "int", wantHeld: false},
		{value: "bytes4k", wantHeld: true, payloadMiB: 36}, // 3*3*1024*4KiB ≈ 36 MiB
	} {
		old := *perfValue
		*perfValue = tc.value
		record := runLifecycleScenario(t, perfRecord{}, 3, 3)
		*perfValue = old
		if !tc.wantHeld {
			if record.ResourcesHeld != nil {
				t.Fatalf("%s variant recorded resources_held=%v; no payload boundary applies", tc.value, record.ResourcesHeld)
			}
			continue
		}
		if record.ResourcesHeld == nil || record.Resources == nil {
			t.Fatalf("%s variant lost a heap phase: held=%v released=%v", tc.value, record.ResourcesHeld, record.Resources)
		}
		for _, key := range []string{"heap_alloc_bytes", "heap_objects"} {
			if _, ok := record.ResourcesHeld[key]; !ok {
				t.Fatalf("held phase missing %s: %v", key, record.ResourcesHeld)
			}
			if _, ok := record.Resources[key]; !ok {
				t.Fatalf("released phase missing %s: %v", key, record.Resources)
			}
		}
		held := record.ResourcesHeld["heap_alloc_bytes"]
		released := record.Resources["heap_alloc_bytes"]
		if held < released+tc.payloadMiB<<20/2 {
			t.Fatalf("payload boundary vanished: held=%d released=%d, want held to exceed released by at least %d MiB", held, released, tc.payloadMiB/2)
		}
		t.Logf("%s boundary: held=%d released=%d bytes", tc.value, held, released)
	}
}
