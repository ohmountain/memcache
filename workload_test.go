package memcache

import (
	"fmt"
	"math/rand"
	"reflect"
	"strconv"
	"testing"
)

// workloadConfig describes a deterministic operation mix. Distribution must
// be uniform, zipf (s=1.2, v=1), or scan (sequential sweep). Seed 0 selects
// the fixed default seed so the zero value is deterministic.
type workloadConfig struct {
	Capacity     int    `json:"capacity"`
	KeyCount     int    `json:"key_count"`
	ReadPercent  int    `json:"read_percent"`
	Distribution string `json:"distribution"`
	Seed         int64  `json:"seed"`
}

type workloadOp struct {
	Key  string
	Read bool
}

const workloadDefaultSeed = int64(20261025)

// makeWorkload pre-generates count operations over a fixed key space. Reads
// are assigned positionally — every window of 100 operations holds exactly
// ReadPercent reads — so the mix never depends on random luck. Whether a read
// hits is decided by the caller's fixture, not here.
func makeWorkload(cfg workloadConfig, count int) ([]workloadOp, error) {
	if cfg.Capacity < 0 {
		return nil, fmt.Errorf("workload: negative capacity %d", cfg.Capacity)
	}
	if cfg.KeyCount <= 0 {
		return nil, fmt.Errorf("workload: key count %d must be positive", cfg.KeyCount)
	}
	if cfg.ReadPercent < 0 || cfg.ReadPercent > 100 {
		return nil, fmt.Errorf("workload: read percent %d out of range", cfg.ReadPercent)
	}
	switch cfg.Distribution {
	case "uniform", "zipf", "scan":
	default:
		return nil, fmt.Errorf("workload: unknown distribution %q", cfg.Distribution)
	}
	if count < 0 {
		return nil, fmt.Errorf("workload: negative count %d", count)
	}
	seed := cfg.Seed
	if seed == 0 {
		seed = workloadDefaultSeed
	}
	rng := rand.New(rand.NewSource(seed))
	var zipf *rand.Zipf
	if cfg.Distribution == "zipf" {
		zipf = rand.NewZipf(rng, 1.2, 1, uint64(cfg.KeyCount-1))
	}
	ops := make([]workloadOp, count)
	for i := 0; i < count; i++ {
		index := 0
		switch cfg.Distribution {
		case "uniform":
			index = rng.Intn(cfg.KeyCount)
		case "zipf":
			index = int(zipf.Uint64())
		case "scan":
			index = i % cfg.KeyCount
		}
		if index < 0 || index >= cfg.KeyCount {
			index = cfg.KeyCount - 1
		}
		ops[i] = workloadOp{Key: workloadKey(index), Read: i%100 < cfg.ReadPercent}
	}
	return ops, nil
}

// workloadKey is the single source of the workload key format. Fixture
// preloads elsewhere must use it too, or hit scenarios silently degrade
// into miss scenarios.
func workloadKey(i int) string { return "k" + strconv.Itoa(i) }

// workloadReplayCount returns the replay cycle length for a capacity: at
// large tiers the cycle must hold more distinct keys than the cache can
// store, otherwise eviction scenarios silently degrade into overwrites of a
// resident working set. The 1K tier keeps the plain 4096-op cycle.
func workloadReplayCount(capacity int) int {
	if count := 2 * capacity; count > workloadReplayOps {
		return count
	}
	return workloadReplayOps
}

// TestWorkloadFullTierEvicts pins the eviction premise behind the scaled
// replay counts: replaying one cycle of a write-only workload over a
// 2x-capacity key pool must evict most of the preloaded entries.
func TestWorkloadFullTierEvicts(t *testing.T) {
	for _, capacity := range []int{1024, 100000} {
		for _, distribution := range []string{"uniform", "scan"} {
			t.Run(fmt.Sprintf("%s-cap%d", distribution, capacity), func(t *testing.T) {
				cfg := workloadConfig{
					Capacity:     capacity,
					KeyCount:     2 * capacity,
					Distribution: distribution,
					Seed:         workloadDefaultSeed,
				}
				ops, err := makeWorkload(cfg, workloadReplayCount(capacity))
				if err != nil {
					t.Fatal(err)
				}
				m := WithLRU[int](uint(capacity), false)
				t.Cleanup(m.Close)
				preloaded := make(map[string]bool, capacity)
				for i := 0; i < capacity; i++ {
					key := workloadKey(i)
					m.Set(key, i)
					preloaded[key] = true
				}
				// A write to a key that is not resident forces an insert and
				// an eviction once the cache is full; counting them proves
				// the cycle keeps crossing the capacity boundary. (The extra
				// Lookup runs in this test only, never in the benchmark.)
				misses := 0
				for _, op := range ops {
					if _, ok := m.Lookup(op.Key); !ok {
						misses++
					}
					m.Set(op.Key, 0)
				}
				snap := snapshotCache(m)
				if len(snap.Items) != capacity {
					t.Fatalf("cache holds %d of %d slots", len(snap.Items), capacity)
				}
				if misses < capacity/4 {
					t.Fatalf("%s: only %d of %d writes missed the full cache", distribution, misses, len(ops))
				}
				if distribution == "scan" {
					// A sequential sweep leaves none of the preloaded head
					// keys resident.
					survivors := 0
					for key := range snap.Items {
						if preloaded[key] {
							survivors++
						}
					}
					if survivors != 0 {
						t.Fatalf("scan left %d preloaded keys resident", survivors)
					}
				}
				t.Logf("%s cap %d: %d misses across %d writes", distribution, capacity, misses, len(ops))
			})
		}
	}
}

func TestWorkloadDeterministic(t *testing.T) {
	cfg := workloadConfig{Capacity: 1024, KeyCount: 2048, ReadPercent: 95, Distribution: "uniform", Seed: 20261025}
	first, err := makeWorkload(cfg, 100)
	if err != nil {
		t.Fatal(err)
	}
	second, err := makeWorkload(cfg, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("unstable workload")
	}
	for _, tc := range []struct {
		readPercent int
		wantReads   int
	}{
		{100, 100},
		{95, 95},
		{50, 50},
		{0, 0},
	} {
		cfg := workloadConfig{Capacity: 1024, KeyCount: 2048, ReadPercent: tc.readPercent, Distribution: "uniform", Seed: 20261025}
		ops, err := makeWorkload(cfg, 100)
		if err != nil {
			t.Fatal(err)
		}
		reads := 0
		for _, op := range ops {
			if op.Read {
				reads++
			}
		}
		if reads != tc.wantReads {
			t.Fatalf("read percent %d: reads=%d, want %d", tc.readPercent, reads, tc.wantReads)
		}
	}
	// Every distribution stays inside the generated key space and a scan
	// sweeps sequentially.
	for _, distribution := range []string{"uniform", "zipf", "scan"} {
		cfg := workloadConfig{Capacity: 64, KeyCount: 128, ReadPercent: 50, Distribution: distribution, Seed: 20261025}
		ops, err := makeWorkload(cfg, 200)
		if err != nil {
			t.Fatal(err)
		}
		if len(ops) != 200 {
			t.Fatalf("%s: len=%d", distribution, len(ops))
		}
		for _, op := range ops {
			if op.Key == "" {
				t.Fatalf("%s: empty key generated", distribution)
			}
		}
	}
	scan, err := makeWorkload(workloadConfig{Capacity: 8, KeyCount: 3, ReadPercent: 0, Distribution: "scan", Seed: 20261025}, 6)
	if err != nil {
		t.Fatal(err)
	}
	wantScan := []string{"k0", "k1", "k2", "k0", "k1", "k2"}
	gotScan := make([]string, 0, len(scan))
	for _, op := range scan {
		gotScan = append(gotScan, op.Key)
	}
	if !reflect.DeepEqual(gotScan, wantScan) {
		t.Fatalf("scan sequence = %v, want %v", gotScan, wantScan)
	}
}

func TestWorkloadRejectsInvalidConfig(t *testing.T) {
	valid := workloadConfig{Capacity: 16, KeyCount: 32, ReadPercent: 50, Distribution: "uniform", Seed: 1}
	cases := []struct {
		name   string
		mutate func(*workloadConfig)
	}{
		{"negative capacity", func(c *workloadConfig) { c.Capacity = -1 }},
		{"zero key count", func(c *workloadConfig) { c.KeyCount = 0 }},
		{"negative key count", func(c *workloadConfig) { c.KeyCount = -5 }},
		{"unknown distribution", func(c *workloadConfig) { c.Distribution = "hot" }},
		{"read percent too low", func(c *workloadConfig) { c.ReadPercent = -1 }},
		{"read percent too high", func(c *workloadConfig) { c.ReadPercent = 101 }},
	}
	for _, invalid := range cases {
		cfg := valid
		invalid.mutate(&cfg)
		if _, err := makeWorkload(cfg, 10); err == nil {
			t.Fatalf("%s: expected error", invalid.name)
		}
	}
	if _, err := makeWorkload(valid, -1); err == nil {
		t.Fatal("negative count: expected error")
	}
	empty, err := makeWorkload(valid, 0)
	if err != nil || len(empty) != 0 {
		t.Fatalf("zero count: ops=%v err=%v, want empty", empty, err)
	}
	// The zero seed selects the fixed default so the zero-value config is
	// still deterministic.
	defaulted := valid
	defaulted.Seed = 0
	withDefault, err := makeWorkload(defaulted, 10)
	if err != nil {
		t.Fatal(err)
	}
	valid.Seed = 20261025
	explicit, err := makeWorkload(valid, 10)
	if err != nil || !reflect.DeepEqual(withDefault, explicit) {
		t.Fatal("default seed not applied")
	}
}
