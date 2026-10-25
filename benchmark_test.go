package memcache

import (
	"flag"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
)

// cachePerfFull gates large, slow performance scenarios. It is registered
// here once and reused by all later performance entry points.
var cachePerfFull = flag.Bool("cache-perf-full", false, "run large cache performance scenarios (slow)")

// workloadReplayOps is the pre-generated operation count every workload
// benchmark replays cyclically; at 4096 it covers the 2048-key space of the
// 1K tier twice per cycle at every distribution.
const workloadReplayOps = 4096

// numericKeys returns the "0".."n-1" key pool shared by the plain
// BenchmarkCache and the payload benchmarks.
func numericKeys(n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = strconv.Itoa(i)
	}
	return keys
}

func BenchmarkCache(b *testing.B) {
	keys := numericKeys(1024)
	for _, operation := range []string{"Lookup", "Get", "Set", "RenewTTL"} {
		b.Run(operation, func(b *testing.B) {
			m := WithLRU[int](1024, operation == "RenewTTL")
			defer m.Close()
			for i, key := range keys {
				m.Set(key, i)
			}
			b.ReportAllocs()
			b.ResetTimer()
			// One wraparound counter and one hoisted loop per operation keep
			// harness arithmetic (modulo, dispatch) out of the timed region.
			keyIndex := 0
			nextKey := func() string {
				key := keys[keyIndex]
				keyIndex++
				if keyIndex == len(keys) {
					keyIndex = 0
				}
				return key
			}
			switch operation {
			case "Lookup":
				for i := 0; i < b.N; i++ {
					m.Lookup(nextKey())
				}
			case "Get":
				for i := 0; i < b.N; i++ {
					m.Get(nextKey())
				}
			case "Set":
				for i := 0; i < b.N; i++ {
					m.Set(nextKey(), i)
				}
			case "RenewTTL":
				for i := 0; i < b.N; i++ {
					m.SetExpire(nextKey(), i, 60)
				}
			}
			b.StopTimer()
		})
	}
	b.Run("ParallelLookup", func(b *testing.B) {
		m := WithLRU[int](1024, false)
		defer m.Close()
		for i, key := range keys {
			m.Set(key, i)
		}
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			keyIndex := 0
			for pb.Next() {
				m.Lookup(keys[keyIndex])
				keyIndex++
				if keyIndex == len(keys) {
					keyIndex = 0
				}
			}
		})
		b.StopTimer()
	})
}

type workloadScenario struct {
	name         string
	ttl          bool
	renew        bool
	preload      bool
	keyRatio     int // key pool as a multiple of capacity
	readPercent  int
	distribution string
}

// BenchmarkCacheWorkload replays pre-generated deterministic mixes. Keys and
// operations are built before the timer; hit or miss is decided by the
// fixture (preload), never by an extra timed Lookup. Parallel workers index
// the operation slice with their own counters and sum read results locally.
func BenchmarkCacheWorkload(b *testing.B) {
	bases := []workloadScenario{
		{name: "Hit", preload: true, keyRatio: 1, readPercent: 100, distribution: "uniform"},
		{name: "Miss", keyRatio: 2, readPercent: 100, distribution: "uniform"},
		{name: "Overwrite", preload: true, keyRatio: 1, distribution: "uniform"},
		{name: "InsertEvict", preload: true, keyRatio: 2, distribution: "uniform"},
		{name: "Uniform95", preload: true, keyRatio: 2, readPercent: 95, distribution: "uniform"},
		{name: "Zipf95", preload: true, keyRatio: 2, readPercent: 95, distribution: "zipf"},
		{name: "Scan50", preload: true, keyRatio: 2, readPercent: 50, distribution: "scan"},
		{name: "RenewTTL", ttl: true, renew: true, preload: true, keyRatio: 1, distribution: "uniform"},
	}
	capacities := []int{1024}
	if *cachePerfFull {
		capacities = append(capacities, 100000, 1000000)
	}
	for _, capacity := range capacities {
		for _, scenario := range bases {
			scenario, capacity := scenario, capacity
			cfg := workloadConfig{
				Capacity:     capacity,
				KeyCount:     scenario.keyRatio * capacity,
				ReadPercent:  scenario.readPercent,
				Distribution: scenario.distribution,
				Seed:         workloadDefaultSeed,
			}
			name := fmt.Sprintf("%s-cap%d", scenario.name, capacity)
			b.Run(name, func(b *testing.B) {
				runWorkloadBenchmark(b, scenario, cfg, false)
			})
			b.Run(name+"-Parallel", func(b *testing.B) {
				runWorkloadBenchmark(b, scenario, cfg, true)
			})
		}
	}
}

func runWorkloadBenchmark(b *testing.B, scenario workloadScenario, cfg workloadConfig, parallel bool) {
	b.Helper()
	ops, err := makeWorkload(cfg, workloadReplayOps)
	if err != nil {
		b.Fatal(err)
	}
	m := WithLRU[int](uint(cfg.Capacity), scenario.ttl)
	b.Cleanup(m.Close)
	if scenario.preload {
		preload := cfg.KeyCount
		if preload > cfg.Capacity {
			preload = cfg.Capacity
		}
		for i := 0; i < preload; i++ {
			if scenario.renew {
				m.SetExpire(workloadKey(i), i, 3600)
			} else {
				m.Set(workloadKey(i), i)
			}
		}
	}
	write := func(key string, value int) {
		if scenario.renew {
			m.SetExpire(key, value, 3600)
		} else {
			m.Set(key, value)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	if !parallel {
		checksum := 0
		opIndex := 0
		for i := 0; i < b.N; i++ {
			op := ops[opIndex]
			opIndex++
			if opIndex == len(ops) {
				opIndex = 0
			}
			if op.Read {
				if value, ok := m.Lookup(op.Key); ok {
					checksum += value
				}
				continue
			}
			write(op.Key, i)
		}
		b.StopTimer()
		// The checksum is a non-negative sink; this only stops the compiler
		// from eliding the timed Lookup.
		if checksum == -1 {
			b.Fatal("unreachable")
		}
		return
	}
	var reads int64
	b.RunParallel(func(pb *testing.PB) {
		local, localReads, opIndex := 0, 0, 0
		for pb.Next() {
			op := ops[opIndex]
			opIndex++
			if opIndex == len(ops) {
				opIndex = 0
			}
			if op.Read {
				if value, ok := m.Lookup(op.Key); ok {
					localReads += value
				}
			} else {
				write(op.Key, local)
			}
			local++
		}
		atomic.AddInt64(&reads, int64(localReads))
	})
	b.StopTimer()
	// Same as above: keep the timed reads observable.
	if reads == -1 {
		b.Fatal("unreachable")
	}
}
