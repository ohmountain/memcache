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

// cleanerRunning reports whether a cache's background cleaner has not
// stopped yet. It is the one place that reads the stopped channel.
func cleanerRunning[T CacheAble](m *Memcache[T]) bool {
	select {
	case <-m.state.stopped:
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
// carrying only a 1 rps no-op load. empty_heap_checks comes from an
// instrumented now() (atomic increment per call): this run honestly reports
// the current ticker behavior with its instrumentation overhead; Stage A
// makes no "no periodic wake-up" claim.
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
	base.Load = runLoadOnceAt(t, cfg, windowStart, func(uint64) loadOutcome { return loadOutcome{} })
	// The last 1 rps request is planned 2s into the 3s window, so the load
	// alone would stop there; hold the full observation window open before
	// sampling resources and ticker checks.
	if remain := time.Until(windowStart.Add(cfg.Duration)); remain > 0 {
		time.Sleep(remain)
	}
	heapAlloc, heapObjects := readHeapStats()
	base.Resources = map[string]uint64{
		"heap_alloc_bytes":  heapAlloc,
		"heap_objects":      heapObjects,
		"goroutines":        uint64(runtime.NumGoroutine()),
		"registered_caches": uint64(Lives() - before),
		"owned_workers":     ownedWorkersRunning(caches),
		"empty_heap_checks": atomic.LoadUint64(&nowCalls),
	}
	t.Logf("idle run: empty_heap_checks counted through an instrumented now() (adds one atomic increment per call)")
	return base
}

// runLifecycleScenario creates, fills, and closes 10 rounds of 10 TTL
// caches of capacity 1024, reporting per-round resource logs and aggregate
// resources. The bytes4k variant stores one pre-allocated 4KiB slice per
// entry; payload references stay held during the rounds, then a final GC
// after releasing them delineates payload versus metadata retention.
func runLifecycleScenario(t *testing.T, base perfRecord) perfRecord {
	switch *perfValue {
	case "int", "bytes4k":
	default:
		t.Fatalf("-cache-perf-value must be int or bytes4k for lifecycle, got %q", *perfValue)
	}
	const rounds, perRound, capacity = 10, 10, 1024
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
		retained = nil
		allocFreed, objectsFreed := readHeapStats()
		t.Logf("payload boundary: held alloc=%d objects=%d; released alloc=%d objects=%d", allocHeld, objectsHeld, allocFreed, objectsFreed)
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
