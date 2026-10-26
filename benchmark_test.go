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

// workloadReplayOps is the pre-generated operation count the uniform and
// zipf workload benchmarks replay cyclically; at 4096 it covers the 2048-key
// space of the 1K tier twice per cycle. Scan benchmarks replay one full key
// space sweep instead (see workloadReplayCount).
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
	for _, operation := range []string{"Lookup", "Peek", "Get", "Set", "RenewTTL"} {
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
			case "Peek":
				for i := 0; i < b.N; i++ {
					m.Peek(nextKey())
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

// workloadBenchmarkScenarios is the fixed benchmark mix table. Tests reuse
// it so a scenario config change (for example the Scan50 key ratio) and its
// regression assertions can never drift apart.
var workloadBenchmarkScenarios = []workloadScenario{
	{name: "Hit", preload: true, keyRatio: 1, readPercent: 100, distribution: "uniform"},
	{name: "Miss", keyRatio: 2, readPercent: 100, distribution: "uniform"},
	{name: "Overwrite", preload: true, keyRatio: 1, distribution: "uniform"},
	{name: "InsertEvict", preload: true, keyRatio: 2, distribution: "uniform"},
	{name: "Uniform95", preload: true, keyRatio: 2, readPercent: 95, distribution: "uniform"},
	{name: "Zipf95", preload: true, keyRatio: 2, readPercent: 95, distribution: "zipf"},
	// The 4x key pool keeps the write half at 2x capacity, so the one-sweep
	// replay crosses the capacity boundary every cycle; at 2x the write half
	// would exactly equal capacity and settle into pure overwrites.
	{name: "Scan50", preload: true, keyRatio: 4, readPercent: 50, distribution: "scan"},
	{name: "RenewTTL", ttl: true, renew: true, preload: true, keyRatio: 1, distribution: "uniform"},
}

// BenchmarkCacheWorkload replays pre-generated deterministic mixes. Keys and
// operations are built before the timer; hit or miss is decided by the
// fixture (preload), never by an extra timed Lookup. Parallel workers index
// the operation slice with their own counters and sum read results locally.
func BenchmarkCacheWorkload(b *testing.B) {
	capacities := []int{1024}
	if *cachePerfFull {
		capacities = append(capacities, 100000, 1000000)
	}
	for _, capacity := range capacities {
		for _, scenario := range workloadBenchmarkScenarios {
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
	ops, err := makeWorkload(cfg, workloadReplayCount(cfg))
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

// TestWorkloadScanSustainsEviction pins the Scan50 benchmark premise across
// warmup: every replayed cycle must keep missing (and therefore evicting)
// after the preload and the first cycle have warmed the cache. The failure
// mode it guards against is the write half settling into overwrites of a
// fully resident key set, which silently turns the scan benchmark into an
// overwrite benchmark. (The extra pre-write Lookups run in this test only,
// never inside the benchmark.)
func TestWorkloadScanSustainsEviction(t *testing.T) {
	var scenario workloadScenario
	for _, s := range workloadBenchmarkScenarios {
		if s.name == "Scan50" {
			scenario = s
		}
	}
	if scenario.name == "" {
		t.Fatal("Scan50 scenario missing from the benchmark table")
	}
	for _, capacity := range []int{1024, 100000} {
		cfg := workloadConfig{
			Capacity:     capacity,
			KeyCount:     scenario.keyRatio * capacity,
			ReadPercent:  scenario.readPercent,
			Distribution: scenario.distribution,
			Seed:         workloadDefaultSeed,
		}
		ops, err := makeWorkload(cfg, workloadReplayCount(cfg))
		if err != nil {
			t.Fatal(err)
		}
		m := WithLRU[int](uint(capacity), false)
		t.Cleanup(m.Close)
		preload := cfg.KeyCount
		if preload > capacity {
			preload = capacity
		}
		for i := 0; i < preload; i++ {
			m.Set(workloadKey(i), i)
		}
		for cycle := 0; cycle < 3; cycle++ {
			misses := 0
			for _, op := range ops {
				if op.Read {
					continue
				}
				if _, ok := m.Lookup(op.Key); !ok {
					misses++
				}
				m.Set(op.Key, cycle)
			}
			if cycle > 0 && misses < capacity/2 {
				t.Fatalf("cap %d cycle %d: only %d of %d writes missed after warmup; the scan replay degraded into overwrites", capacity, cycle, misses, len(ops)/2)
			}
			t.Logf("cap %d cycle %d: %d write misses across %d ops", capacity, cycle, misses, len(ops))
		}
	}
}

// BenchmarkCacheSharded measures the opt-in sharded read path. Workers start
// at different keys so they are not all queueing on one shard; a lockstep
// workload is deliberately left to BenchmarkCache/ParallelLookup.
func BenchmarkCacheSharded(b *testing.B) {
	keys := numericKeys(1024)
	for _, shards := range []int{1, 8, 16, 64} {
		shards := shards
		b.Run(fmt.Sprintf("Shards%d", shards), func(b *testing.B) {
			m := WithOptions[int](Options{Capacity: 4096, Shards: shards})
			defer m.Close()
			for i, key := range keys {
				m.Set(key, i)
			}
			b.ReportAllocs()
			b.ResetTimer()
			var worker int64
			b.RunParallel(func(pb *testing.PB) {
				w := int(atomic.AddInt64(&worker, 1) - 1)
				keyIndex := (w * 997) % len(keys)
				for pb.Next() {
					m.Lookup(keys[keyIndex])
					keyIndex++
					if keyIndex == len(keys) {
						keyIndex = 0
					}
				}
			})
		})
	}
}

// BenchmarkCacheShardedSetExpire covers the TTL write path, which must not
// serialize on the cache-wide lifecycle mutex once the cleaner is running.
func BenchmarkCacheShardedSetExpire(b *testing.B) {
	keys := numericKeys(1024)
	for _, shards := range []int{1, 8, 64} {
		shards := shards
		b.Run(fmt.Sprintf("Shards%d", shards), func(b *testing.B) {
			m := WithOptions[int](Options{Capacity: 4096, EnableExpired: true, Shards: shards})
			defer m.Close()
			for i, key := range keys {
				m.Set(key, i)
			}
			b.ReportAllocs()
			b.ResetTimer()
			var worker int64
			b.RunParallel(func(pb *testing.PB) {
				w := int(atomic.AddInt64(&worker, 1) - 1)
				keyIndex := (w * 997) % len(keys)
				i := 0
				for pb.Next() {
					m.SetExpire(keys[keyIndex], i, 3600)
					i++
					keyIndex++
					if keyIndex == len(keys) {
						keyIndex = 0
					}
				}
			})
		})
	}
}
