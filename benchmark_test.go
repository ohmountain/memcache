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

func BenchmarkCache(b *testing.B) {
	keys := make([]string, 1024)
	for i := range keys {
		keys[i] = strconv.Itoa(i)
	}
	for _, operation := range []string{"Lookup", "Get", "Set", "RenewTTL"} {
		b.Run(operation, func(b *testing.B) {
			m := WithLRU[int](1024, operation == "RenewTTL")
			defer m.Close()
			for i, key := range keys {
				m.Set(key, i)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				key := keys[i%len(keys)]
				switch operation {
				case "Lookup":
					m.Lookup(key)
				case "Get":
					m.Get(key)
				case "Set":
					m.Set(key, i)
				case "RenewTTL":
					m.SetExpire(key, i, 60)
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
			i := 0
			for pb.Next() {
				m.Lookup(keys[i%len(keys)])
				i++
			}
		})
		b.StopTimer()
	})
}

type workloadScenario struct {
	name        string
	ttl         bool
	renew       bool
	preload     bool
	keyRatio    int // key pool as a multiple of capacity
	readPercent int
}

// BenchmarkCacheWorkload replays pre-generated deterministic mixes. Keys and
// operations are built before the timer; hit or miss is decided by the
// fixture (preload), never by an extra timed Lookup. Parallel workers index
// the operation slice with their own counters and sum read results locally.
func BenchmarkCacheWorkload(b *testing.B) {
	bases := []workloadScenario{
		{"Hit", false, false, true, 1, 100},
		{"Miss", false, false, false, 2, 100},
		{"Overwrite", false, false, true, 1, 0},
		{"InsertEvict", false, false, true, 2, 0},
		{"Uniform95", false, false, true, 2, 95},
		{"Zipf95", false, false, true, 2, 95},
		{"Scan50", false, false, true, 2, 50},
		{"RenewTTL", true, true, true, 1, 0},
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
				Distribution: "uniform",
				Seed:         workloadDefaultSeed,
			}
			if scenario.name == "Zipf95" {
				cfg.Distribution = "zipf"
			}
			if scenario.name == "Scan50" {
				cfg.Distribution = "scan"
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
	ops, err := makeWorkload(cfg, 4096)
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
			key := "k" + strconv.Itoa(i)
			if scenario.renew {
				m.SetExpire(key, i, 3600)
			} else {
				m.Set(key, i)
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
		for i := 0; i < b.N; i++ {
			op := ops[i%len(ops)]
			if op.Read {
				if value, ok := m.Lookup(op.Key); ok {
					checksum += value
				}
				continue
			}
			write(op.Key, i)
		}
		b.StopTimer()
		if checksum == -1 {
			b.Fatal("unreachable")
		}
		return
	}
	var reads int64
	b.RunParallel(func(pb *testing.PB) {
		local, localReads := 0, 0
		for pb.Next() {
			op := ops[local%len(ops)]
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
	if reads == -1 {
		b.Fatal("unreachable")
	}
}
