package memcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"container/heap"
)

func TestPerformanceJSONSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	record := perfRecord{Schema: 1, Case: "noop", GoVersion: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GOMAXPROCS: runtime.GOMAXPROCS(0), Seed: 20261025,
		Config:    loadConfig{Rate: 1000, Duration: time.Second, Workers: 1, Queue: 1, BurstWindow: 250 * time.Millisecond},
		Load:      loadResult{Planned: 1000, Attempted: 1000, Completed: 1000, All: loadQuantiles{Samples: 1000, P50: time.Millisecond}},
		Resources: map[string]uint64{"goroutines": 4}}
	if err := writePerfRecord(path, record); err != nil {
		t.Fatal(err)
	}
	if err := writePerfRecord(path, record); err == nil {
		t.Fatal("overwrote baseline evidence")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["schema"] != float64(1) || decoded["case"] != "noop" || decoded["seed"] != float64(20261025) {
		t.Fatalf("header fields = %v", decoded)
	}
	config, ok := decoded["config"].(map[string]interface{})
	if !ok {
		t.Fatal("missing config object")
	}
	if config["rate"] != float64(1000) || config["workers"] != float64(1) || config["queue"] != float64(1) {
		t.Fatalf("config = %v", config)
	}
	if config["duration"] != float64(time.Second) || config["burst_window"] != float64(250*time.Millisecond) {
		t.Fatalf("durations not encoded in nanoseconds: %v", config)
	}
	load, ok := decoded["load"].(map[string]interface{})
	if !ok {
		t.Fatal("missing load object")
	}
	if load["planned"] != float64(1000) || load["completed"] != float64(1000) {
		t.Fatalf("load counts = %v", load)
	}
	all, ok := load["all"].(map[string]interface{})
	if !ok || all["samples"] != float64(1000) || all["p50"] != float64(time.Millisecond) {
		t.Fatalf("quantiles = %v", all)
	}
	if decoded["resources"].(map[string]interface{})["goroutines"] != float64(4) {
		t.Fatal("resources lost")
	}
}

func TestRealExpiryFixture(t *testing.T) {
	m, base := prepareRealExpiryCache(t, 8, 2, 0)
	snap := snapshotCache(m)
	if len(snap.Items) != 10 || len(snap.Heap) != 8 {
		t.Fatalf("fixture items=%d heap=%d, want 10/8", len(snap.Items), len(snap.Heap))
	}
	for key, item := range snap.Items {
		if strings.HasPrefix(key, "exp") {
			if !item.Deadline.Equal(base) {
				t.Fatalf("burst deadline for %q = %v, want %v", key, item.Deadline, base)
			}
		} else if !item.Deadline.IsZero() {
			t.Fatalf("live key %q has a deadline", key)
		}
	}
	assertInvariants(t, m)

	spread := time.Second
	m2, base2 := prepareRealExpiryCache(t, 8, 2, spread)
	snap2 := snapshotCache(m2)
	earliest, latest := time.Time{}, time.Time{}
	for key, item := range snap2.Items {
		if !strings.HasPrefix(key, "exp") {
			continue
		}
		if item.Deadline.Before(base2) || item.Deadline.After(base2.Add(spread)) {
			t.Fatalf("spread deadline for %q = %v outside (%v, %v]", key, item.Deadline, base2, base2.Add(spread))
		}
		if earliest.IsZero() || item.Deadline.Before(earliest) {
			earliest = item.Deadline
		}
		if item.Deadline.After(latest) {
			latest = item.Deadline
		}
	}
	if earliest.IsZero() || !latest.After(earliest) {
		t.Fatalf("spread deadlines not distributed: earliest=%v latest=%v", earliest, latest)
	}
	select {
	case <-m2.state.stopped:
		t.Fatal("cleaner not running after preparation")
	default:
	}
	assertInvariants(t, m2)
}

// Scenario flags; -cache-perf-full comes from benchmark_test.go. Cases and
// their fixed configurations are documented in docs/performance/README.md.
var (
	perfCase      = flag.String("cache-perf-case", "", "performance scenario: noop|steady|expiry-burst|expiry-spread|size-burst|idle-empty|idle-future|lifecycle")
	perfRate      = flag.Int("cache-perf-rate", 10000, "planned requests per second")
	perfWorkers   = flag.Int("cache-perf-workers", 4, "open-loop workers")
	perfQueue     = flag.Int("cache-perf-queue", 1024, "open-loop queue bound")
	perfDuration  = flag.Duration("cache-perf-duration", 3*time.Second, "measurement window")
	perfInstances = flag.Int("cache-perf-instances", 1, "instance count for idle scenarios (1|100|1000)")
	perfValue     = flag.String("cache-perf-value", "int", "value type for lifecycle (int|bytes4k)")
	perfOut       = flag.String("cache-perf-out", "", "output JSON path; must not exist (evidence is never overwritten)")
)

// perfRecord is the JSON evidence of one scenario run. Durations encode as
// nanoseconds (encoding/json default). Schema=1.
type perfRecord struct {
	Schema          int               `json:"schema"`
	Case            string            `json:"case"`
	GoVersion       string            `json:"go_version"`
	GOOS            string            `json:"goos"`
	GOARCH          string            `json:"goarch"`
	GOMAXPROCS      int               `json:"gomaxprocs"`
	Seed            int64             `json:"seed"`
	Config          loadConfig        `json:"config"`
	Workload        workloadConfig    `json:"workload"`
	Capacity        int               `json:"capacity"`
	InitialExpiring int               `json:"initial_expiring"`
	Instances       int               `json:"instances"`
	ValueType       string            `json:"value_type"`
	WorkloadSHA256  string            `json:"workload_sha256"`
	Load            loadResult        `json:"load"`
	Resources       map[string]uint64 `json:"resources,omitempty"`
}

// writePerfRecord creates path with O_EXCL: baseline evidence is written
// once and never overwritten.
func writePerfRecord(path string, record perfRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// burstWindowOf is min(250ms, duration) written with plain comparisons for
// Go 1.18 compatibility.
func burstWindowOf(duration time.Duration) time.Duration {
	window := 250 * time.Millisecond
	if duration < window {
		window = duration
	}
	return window
}

const (
	expiryExpiredPrefix = "exp"
	expiryLivePrefix    = "live"
	expiryNewPrefix     = "new"
)

func expiryExpiredKey(i int) string { return fmt.Sprintf("%s%06d", expiryExpiredPrefix, i) }
func expiryLiveKey(i int) string    { return fmt.Sprintf("%s%06d", expiryLivePrefix, i) }
func expiryNewKey(i int) string     { return fmt.Sprintf("%s%06d", expiryNewPrefix, i) }

// prepareRealExpiryCache builds a TTL cache holding expired expiring entries
// plus live permanent ones on the real clock. Deadlines are rewritten under
// the cache lock after filling — uniform (spread=0) or evenly spread over
// (base, base+spread] — so filling time never distorts the deadline
// distribution; heap.Init rebuilds the index. The cleaner is started exactly
// once by re-arming the stopped channel the constructor closed. The returned
// time is the earliest deadline, the moment measurement starts from. No
// concurrent operation or Close may happen during preparation.
func prepareRealExpiryCache(tb testing.TB, expired, live int, spread time.Duration) (*Memcache[int], time.Time) {
	tb.Helper()
	if expired < 0 || live < 0 || (expired > 0 && spread < 0) {
		tb.Fatalf("invalid expiry fixture: expired=%d live=%d spread=%v", expired, live, spread)
	}
	m := newMemcache[int](uint(expired+live), true, time.Now, false)
	tb.Cleanup(m.Close)
	for i := 0; i < expired; i++ {
		m.SetExpire(expiryExpiredKey(i), i, 3600)
	}
	for i := 0; i < live; i++ {
		m.Set(expiryLiveKey(i), i)
	}
	s := m.state
	s.mu.Lock()
	base := time.Now().Add(time.Second)
	for i := 0; i < expired; i++ {
		deadline := base
		if spread > 0 {
			deadline = base.Add(time.Duration(int64(spread) * int64(i+1) / int64(expired)))
		}
		s.holder[expiryExpiredKey(i)].expiresAt = deadline
	}
	heap.Init(&s.expiry)
	s.stopped = make(chan struct{})
	go s.runCleaner()
	s.mu.Unlock()
	return m, base
}

// cacheAction is one adapted request: read a fixture key, write a
// pre-generated key, or an explicit Size.
type cacheAction struct {
	Key  string
	Kind uint8 // 0 read, 1 write, 2 size
}

// buildCacheActions adapts a generated read/write pattern to concrete keys:
// reads consume readKeys in order, writes consume writeKeys in order, so no
// key is produced inside the timed loop. With sizeEvery > 0, one read in
// every window of sizeEvery operations is replaced by a Size.
func buildCacheActions(cfg workloadConfig, count int, readKeys, writeKeys []string, sizeEvery int) ([]cacheAction, error) {
	pattern, err := makeWorkload(cfg, count)
	if err != nil {
		return nil, err
	}
	if len(readKeys) == 0 || len(writeKeys) == 0 {
		return nil, fmt.Errorf("performance: empty key pool")
	}
	actions := make([]cacheAction, 0, len(pattern))
	reads, writes := 0, 0
	for i, op := range pattern {
		if sizeEvery > 0 && i%sizeEvery == 10 && op.Read {
			actions = append(actions, cacheAction{Kind: 2})
			continue
		}
		if op.Read {
			actions = append(actions, cacheAction{Key: readKeys[reads%len(readKeys)], Kind: 0})
			reads++
		} else {
			actions = append(actions, cacheAction{Key: writeKeys[writes%len(writeKeys)], Kind: 1})
			writes++
		}
	}
	return actions, nil
}

func actionsSHA256(actions []cacheAction) string {
	digest := sha256.New()
	for _, action := range actions {
		switch action.Kind {
		case 0:
			digest.Write([]byte("R "))
		case 1:
			digest.Write([]byte("W "))
		default:
			digest.Write([]byte("S "))
		}
		digest.Write([]byte(action.Key))
		digest.Write([]byte("\n"))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func cacheActionOp(m *Memcache[int], actions []cacheAction) func(uint64) loadOutcome {
	return func(id uint64) loadOutcome {
		action := actions[int(id)%len(actions)]
		switch action.Kind {
		case 0:
			_, ok := m.Lookup(action.Key)
			return loadOutcome{Read: true, Hit: ok}
		case 1:
			m.Set(action.Key, int(id))
			return loadOutcome{Write: true}
		default:
			m.Size()
			return loadOutcome{}
		}
	}
}

// verifyLoadAccounting enforces the load accounting equations on every
// recorded run.
func verifyLoadAccounting(r loadResult, canceled bool) error {
	if canceled {
		if r.Planned != r.Attempted+r.Unissued || r.Attempted != r.Completed+r.Dropped {
			return fmt.Errorf("canceled run accounting broken: %+v", r)
		}
	} else if r.Attempted != r.Planned || r.Completed+r.Dropped != r.Planned || r.Unissued != 0 {
		return fmt.Errorf("run accounting broken: %+v", r)
	}
	if r.BurstAttempted != r.BurstCompleted+r.BurstDropped+r.BurstUnissued ||
		r.BurstPlanned != r.BurstCompleted+r.BurstDropped+r.BurstUnissued {
		return fmt.Errorf("burst accounting broken: %+v", r)
	}
	if r.All.Samples != r.Completed || r.Burst.Samples != r.BurstCompleted || r.Set.Samples > r.Completed {
		return fmt.Errorf("sample accounting broken: %+v", r)
	}
	return nil
}

func runLoadOnceAt(t *testing.T, cfg loadConfig, start time.Time, op func(uint64) loadOutcome) loadResult {
	t.Helper()
	clock := newRealLoadClock()
	defer clock.Close()
	result, err := runOpenLoop(context.Background(), cfg, start, clock, op)
	if err != nil {
		t.Fatalf("open-loop run %v: %v", cfg, err)
	}
	if err := verifyLoadAccounting(result, false); err != nil {
		t.Fatal(err)
	}
	return result
}

func perfLoadConfig() loadConfig {
	return loadConfig{Rate: *perfRate, Duration: *perfDuration, Workers: *perfWorkers, Queue: *perfQueue, BurstWindow: burstWindowOf(*perfDuration)}
}

func runNoopScenario(t *testing.T, base perfRecord) perfRecord {
	cfg := perfLoadConfig()
	base.Config = cfg
	base.ValueType = "none"
	base.Load = runLoadOnceAt(t, cfg, time.Now(), func(uint64) loadOutcome { return loadOutcome{} })
	base.Resources = map[string]uint64{"goroutines": uint64(runtime.NumGoroutine())}
	return base
}

func runSteadyScenario(t *testing.T, base perfRecord) perfRecord {
	cfg := perfLoadConfig()
	wcfg := workloadConfig{Capacity: 1024, KeyCount: 2048, ReadPercent: 95, Distribution: "uniform", Seed: workloadDefaultSeed}
	keys := make([]string, 2048)
	for i := range keys {
		keys[i] = "k" + strconv.Itoa(i)
	}
	actions, err := buildCacheActions(wcfg, 4096, keys, keys, 0)
	if err != nil {
		t.Fatal(err)
	}
	before := Lives()
	m := WithLRU[int](1024, false)
	t.Cleanup(m.Close)
	for i := 0; i < 1024; i++ {
		m.Set(keys[i], i)
	}
	base.Config = cfg
	base.Workload = wcfg
	base.Capacity = 1024
	base.ValueType = "int"
	base.WorkloadSHA256 = actionsSHA256(actions)
	base.Load = runLoadOnceAt(t, cfg, time.Now(), cacheActionOp(m, actions))
	base.Resources = map[string]uint64{
		"goroutines":        uint64(runtime.NumGoroutine()),
		"registered_caches": uint64(Lives() - before),
		"owned_workers":     0,
	}
	return base
}

// waitExpiryReclaimed polls pure snapshots (never Size) until no expiring
// fixture key remains physically, bounded by bound. The returned duration
// runs from the measurement start.
func waitExpiryReclaimed(m *Memcache[int], startedAt time.Time, bound time.Duration) (time.Duration, bool) {
	limit := time.Now().Add(bound)
	for {
		remaining := 0
		for key := range snapshotCache(m).Items {
			if strings.HasPrefix(key, expiryExpiredPrefix) {
				remaining++
			}
		}
		if remaining == 0 {
			return time.Since(startedAt), true
		}
		if time.Now().After(limit) {
			return 0, false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func runExpiryScenario(t *testing.T, base perfRecord, spread time.Duration, withSize bool) perfRecord {
	const expiredCount, liveCount, newKeys = 100000, 1024, 4096
	cfg := perfLoadConfig()
	wcfg := workloadConfig{Capacity: expiredCount + liveCount, KeyCount: 2 * (expiredCount + liveCount), ReadPercent: 95, Distribution: "uniform", Seed: workloadDefaultSeed}
	readKeys := make([]string, liveCount)
	for i := range readKeys {
		readKeys[i] = expiryLiveKey(i)
	}
	writeKeys := make([]string, newKeys)
	for i := range writeKeys {
		writeKeys[i] = expiryNewKey(i)
	}
	sizeEvery := 0
	if withSize {
		sizeEvery = 100
	}
	actions, err := buildCacheActions(wcfg, 4096, readKeys, writeKeys, sizeEvery)
	if err != nil {
		t.Fatal(err)
	}
	before := Lives()

	// Latency run: no snapshots inside the measurement.
	m, startAt := prepareRealExpiryCache(t, expiredCount, liveCount, spread)
	latency := runLoadOnceAt(t, cfg, startAt, cacheActionOp(m, actions))

	// Monitor run: separate fixture and load; pure snapshots only.
	monitor, monitorStart := prepareRealExpiryCache(t, expiredCount, liveCount, spread)
	monitorDone := make(chan loadResult, 1)
	go func() {
		clock := newRealLoadClock()
		defer clock.Close()
		result, err := runOpenLoop(context.Background(), cfg, monitorStart, clock, cacheActionOp(monitor, actions))
		if err != nil {
			t.Errorf("monitor run: %v", err)
		}
		monitorDone <- result
	}()
	reclaim, reclaimed := waitExpiryReclaimed(monitor, monitorStart, 10*time.Second)
	if err := verifyLoadAccounting(<-monitorDone, false); err != nil {
		t.Errorf("monitor run %v", err)
	}

	base.Config = cfg
	base.Workload = wcfg
	base.Capacity = expiredCount + liveCount
	base.InitialExpiring = expiredCount
	base.ValueType = "int"
	base.WorkloadSHA256 = actionsSHA256(actions)
	base.Load = latency
	resources := map[string]uint64{
		"goroutines":        uint64(runtime.NumGoroutine()),
		"registered_caches": uint64(Lives() - before),
	}
	if reclaimed {
		resources["reclaim_duration_ns"] = uint64(reclaim.Nanoseconds())
	} else {
		t.Logf("reclaim did not finish within the bounded wait; reclaim_duration_ns omitted")
	}
	base.Resources = resources
	return base
}

// TestCachePerformance is the explicit entry point for the staged
// performance scenarios. It never runs under a plain `go test ./...`.
func TestCachePerformance(t *testing.T) {
	if *perfCase == "" {
		t.Skip("performance scenarios run explicitly with -cache-perf-case; see docs/performance/README.md")
	}
	if *perfOut == "" {
		t.Fatal("-cache-perf-out is required together with -cache-perf-case")
	}
	base := perfRecord{
		Schema:     1,
		Case:       *perfCase,
		GoVersion:  runtime.Version(),
		GOOS:       runtime.GOOS,
		GOARCH:     runtime.GOARCH,
		GOMAXPROCS: runtime.GOMAXPROCS(0),
		Seed:       workloadDefaultSeed,
	}
	var record perfRecord
	switch *perfCase {
	case "noop":
		record = runNoopScenario(t, base)
	case "steady":
		record = runSteadyScenario(t, base)
	case "expiry-burst":
		record = runExpiryScenario(t, base, 0, false)
	case "expiry-spread":
		record = runExpiryScenario(t, base, time.Second, false)
	case "size-burst":
		record = runExpiryScenario(t, base, 0, true)
	case "idle-empty":
		record = runIdleScenario(t, base, false)
	case "idle-future":
		record = runIdleScenario(t, base, true)
	case "lifecycle":
		record = runLifecycleScenario(t, base)
	default:
		t.Fatalf("unknown -cache-perf-case %q", *perfCase)
	}
	if err := writePerfRecord(*perfOut, record); err != nil {
		t.Fatalf("writing evidence %s: %v", *perfOut, err)
	}
	t.Logf("performance record written to %s", *perfOut)
}
