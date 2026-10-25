package memcache

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// loadConfig describes an open-loop run: requests are planned at
// start + floor(id*1s/Rate) regardless of completion speed, queued into a
// bounded queue, and dropped (never hidden) when the queue is full.
type loadConfig struct {
	Rate        int           `json:"rate"`
	Duration    time.Duration `json:"duration"`
	Workers     int           `json:"workers"`
	Queue       int           `json:"queue"`
	BurstWindow time.Duration `json:"burst_window"`
}

// loadOutcome classifies one completed operation. Read and Write are never
// both true: Lookup reports Read, Set reports Write, Size/noop reports
// neither.
type loadOutcome struct {
	Read  bool
	Hit   bool
	Write bool
}

type loadQuantiles struct {
	Samples int           `json:"samples"`
	P50     time.Duration `json:"p50"`
	P95     time.Duration `json:"p95"`
	P99     time.Duration `json:"p99"`
	Max     time.Duration `json:"max"`
}

type loadResult struct {
	Planned        int           `json:"planned"`
	Attempted      int           `json:"attempted"`
	Completed      int           `json:"completed"`
	Dropped        int           `json:"dropped"`
	Unissued       int           `json:"unissued"`
	BurstPlanned   int           `json:"burst_planned"`
	BurstAttempted int           `json:"burst_attempted"`
	BurstCompleted int           `json:"burst_completed"`
	BurstDropped   int           `json:"burst_dropped"`
	BurstUnissued  int           `json:"burst_unissued"`
	Reads          int           `json:"reads"`
	Hits           int           `json:"hits"`
	Elapsed        time.Duration `json:"elapsed"`
	All            loadQuantiles `json:"all"`
	Burst          loadQuantiles `json:"burst"`
	Set            loadQuantiles `json:"set"`
	BurstSet       loadQuantiles `json:"burst_set"`
}

// loadClock is the scheduler's time source: the real clock shares one reused
// timer, the fake clock advances explicitly for deterministic self-tests.
type loadClock interface {
	Now() time.Time
	WaitUntil(context.Context, time.Time) error
}

const (
	loadMaxRate     = 1000000
	loadMaxPlanned  = 2000000
	loadMaxDuration = 60 * time.Second
	loadMaxWorkers  = 64
	loadMaxQueue    = 65536
)

// openLoopIssueDone, when non-nil, runs on the scheduler goroutine after the
// issue loop and the queue close complete, so every enqueue attempt has
// finished. Tests use it to make drop accounting deterministic against
// real-time goroutine scheduling; it is set and restored around a single
// runOpenLoop call and is test-only state.
var openLoopIssueDone func()

func (c loadConfig) validate() error {
	if c.Rate < 1 || c.Rate > loadMaxRate {
		return fmt.Errorf("load: rate %d out of range [1,%d]", c.Rate, loadMaxRate)
	}
	if c.Duration <= 0 || c.Duration > loadMaxDuration {
		return fmt.Errorf("load: duration %v out of range (0,%v]", c.Duration, loadMaxDuration)
	}
	if c.Workers < 1 || c.Workers > loadMaxWorkers {
		return fmt.Errorf("load: workers %d out of range [1,%d]", c.Workers, loadMaxWorkers)
	}
	if c.Queue < 0 || c.Queue > loadMaxQueue {
		return fmt.Errorf("load: queue %d out of range [0,%d]", c.Queue, loadMaxQueue)
	}
	if c.BurstWindow <= 0 || c.BurstWindow > c.Duration {
		return fmt.Errorf("load: burst window %v out of range (0,%v]", c.BurstWindow, c.Duration)
	}
	if planned := int64(c.Duration) * int64(c.Rate) / int64(time.Second); planned < 1 || planned > loadMaxPlanned {
		return fmt.Errorf("load: planned %d out of range [1,%d]", planned, loadMaxPlanned)
	}
	return nil
}

// percentileIndex is the nearest-rank index: ceil(p*n/100) - 1, at least 0.
func percentileIndex(n, p int) int {
	i := (p*n + 99) / 100
	if i < 1 {
		i = 1
	}
	if i > n {
		i = n
	}
	return i - 1
}

// summarizeLoad returns nearest-rank quantiles over a copy of samples. An
// empty input yields Samples=0, which callers must not read as a valid zero
// latency; a negative sample is an error.
func summarizeLoad(samples []time.Duration) (loadQuantiles, error) {
	for _, s := range samples {
		if s < 0 {
			return loadQuantiles{}, fmt.Errorf("load: negative latency sample %v", s)
		}
	}
	summary := loadQuantiles{Samples: len(samples)}
	if len(samples) == 0 {
		return summary, nil
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	summary.P50 = sorted[percentileIndex(len(sorted), 50)]
	summary.P95 = sorted[percentileIndex(len(sorted), 95)]
	summary.P99 = sorted[percentileIndex(len(sorted), 99)]
	summary.Max = sorted[len(sorted)-1]
	return summary, nil
}

// runOpenLoop plans cfg requests at fixed intervals from start, hands each
// to a bounded queue as its time arrives (non-blocking; a full queue counts
// a drop), and joins cfg.Workers workers that call op. Latency runs from the
// planned time to completion, so queueing and scheduler lag are included.
// The scheduler never skips a planned request because it is late. On context
// cancellation no new requests are issued, already-queued requests still
// run, every worker is joined, and the context error is returned. Burst*
// fields use only requests planned inside [start,start+BurstWindow). All
// sample capacity is allocated up front; each sample is written once by its
// completing worker and aggregated after the join.
func runOpenLoop(ctx context.Context, cfg loadConfig, start time.Time, clock loadClock, op func(uint64) loadOutcome) (loadResult, error) {
	if err := cfg.validate(); err != nil {
		return loadResult{}, err
	}
	planned := int(int64(cfg.Duration) * int64(cfg.Rate) / int64(time.Second))
	plannedTime := func(id uint64) time.Time {
		offset := time.Duration(uint64(id) * uint64(time.Second) / uint64(cfg.Rate))
		return start.Add(offset)
	}
	inBurst := func(id uint64) bool { return plannedTime(id).Sub(start) < cfg.BurstWindow }

	begin := clock.Now()
	queue := make(chan uint64, cfg.Queue)
	latencies := make([]time.Duration, planned)
	completedFlags := make([]bool, planned)
	writeFlags := make([]bool, planned)
	droppedFlags := make([]bool, planned)
	var reads, hits int64

	var wg sync.WaitGroup
	for w := 0; w < cfg.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			localReads, localHits := int64(0), int64(0)
			for id := range queue {
				outcome := op(id)
				latencies[id] = clock.Now().Sub(plannedTime(id))
				completedFlags[id] = true
				writeFlags[id] = outcome.Write
				if outcome.Read {
					localReads++
					if outcome.Hit {
						localHits++
					}
				}
			}
			atomic.AddInt64(&reads, localReads)
			atomic.AddInt64(&hits, localHits)
		}()
	}

	schedulerDone := make(chan struct{})
	var runErr error
	go func() {
		// Defer LIFO: the queue closes first, then schedulerDone, then the
		// issue-done hook — so the hook really runs after every enqueue
		// attempt and the queue close complete.
		if issueDone := openLoopIssueDone; issueDone != nil {
			defer issueDone()
		}
		defer close(schedulerDone)
		defer close(queue)
		select {
		case <-ctx.Done():
			runErr = ctx.Err()
			return
		default:
		}
		for id := 0; id < planned; id++ {
			if err := clock.WaitUntil(ctx, plannedTime(uint64(id))); err != nil {
				runErr = err
				return
			}
			select {
			case queue <- uint64(id):
			default:
				droppedFlags[id] = true
			}
		}
	}()
	<-schedulerDone
	wg.Wait()

	result := loadResult{Planned: planned, Elapsed: clock.Now().Sub(begin)}
	var all, burst, set, burstSet []time.Duration
	for id := 0; id < planned; id++ {
		burstMember := inBurst(uint64(id))
		if burstMember {
			result.BurstPlanned++
		}
		switch {
		case completedFlags[id]:
			result.Completed++
			result.Attempted++
			all = append(all, latencies[id])
			if writeFlags[id] {
				set = append(set, latencies[id])
			}
			if burstMember {
				result.BurstCompleted++
				result.BurstAttempted++
				burst = append(burst, latencies[id])
				if writeFlags[id] {
					burstSet = append(burstSet, latencies[id])
				}
			}
		case droppedFlags[id]:
			result.Dropped++
			result.Attempted++
			if burstMember {
				result.BurstDropped++
				result.BurstAttempted++
			}
		default:
			result.Unissued++
			if burstMember {
				result.BurstUnissued++
			}
		}
	}
	result.Reads = int(atomic.LoadInt64(&reads))
	result.Hits = int(atomic.LoadInt64(&hits))
	var err error
	if result.All, err = summarizeLoad(all); err != nil {
		return loadResult{}, err
	}
	if result.Burst, err = summarizeLoad(burst); err != nil {
		return loadResult{}, err
	}
	if result.Set, err = summarizeLoad(set); err != nil {
		return loadResult{}, err
	}
	if result.BurstSet, err = summarizeLoad(burstSet); err != nil {
		return loadResult{}, err
	}
	if runErr != nil {
		return result, runErr
	}
	return result, nil
}

// realLoadClock implements loadClock with real time. The scheduler is its
// single owner and reuses one timer for every wait; Close stops it. The
// Reset sequence follows the Go 1.18 timer rules: Stop first, drain a fired
// event non-blockingly (this consumer is the only receiver), then Reset.
type realLoadClock struct {
	mu    sync.Mutex
	timer *time.Timer
}

var errRealLoadClockClosed = errors.New("load: real load clock closed")

func newRealLoadClock() *realLoadClock {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	return &realLoadClock{timer: timer}
}

// stopDrainedTimer stops the timer and, per the Go 1.18 timer rules, drains
// an event that already fired so the channel is clean before the next use.
// This clock's single-owner scheduler is the only receiver, so a
// non-blocking drain cannot steal an event anyone else is waiting on.
func stopDrainedTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func (c *realLoadClock) Now() time.Time { return time.Now() }

// WaitUntil must be called from one goroutine at a time (the scheduler).
func (c *realLoadClock) WaitUntil(ctx context.Context, target time.Time) error {
	c.mu.Lock()
	timer := c.timer
	c.mu.Unlock()
	if timer == nil {
		return errRealLoadClockClosed
	}
	if d := time.Until(target); d > 0 {
		stopDrainedTimer(timer)
		timer.Reset(d)
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			stopDrainedTimer(timer)
			return ctx.Err()
		}
	}
	return nil
}

func (c *realLoadClock) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timer != nil {
		stopDrainedTimer(c.timer)
		c.timer = nil
	}
}

// fakeLoadClock is the deterministic self-test clock. AdvanceTo moves time
// forward (never backward) and wakes every waiter; a waiter whose target has
// not been reached yet re-parks. WaitUntil also honors context cancellation.
type fakeLoadClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters map[int64]chan struct{}
	next    int64
}

func newFakeLoadClock(start time.Time) *fakeLoadClock {
	return &fakeLoadClock{now: start, waiters: make(map[int64]chan struct{})}
}

func (c *fakeLoadClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeLoadClock) WaitUntil(ctx context.Context, target time.Time) error {
	for {
		c.mu.Lock()
		if !c.now.Before(target) {
			c.mu.Unlock()
			return nil
		}
		ticket := c.next
		c.next++
		wake := make(chan struct{})
		c.waiters[ticket] = wake
		c.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			c.mu.Lock()
			delete(c.waiters, ticket)
			c.mu.Unlock()
			return ctx.Err()
		}
	}
}

func (c *fakeLoadClock) AdvanceTo(t time.Time) {
	c.mu.Lock()
	if t.After(c.now) {
		c.now = t
	}
	woken := make([]chan struct{}, 0, len(c.waiters))
	for ticket, wake := range c.waiters {
		woken = append(woken, wake)
		delete(c.waiters, ticket)
	}
	c.mu.Unlock()
	for _, wake := range woken {
		close(wake)
	}
}

func TestLoadQuantiles(t *testing.T) {
	samples := make([]time.Duration, 100)
	for i := range samples {
		samples[i] = time.Duration(i + 1)
	}
	q, err := summarizeLoad(samples)
	if err != nil || q.Samples != 100 || q.P50 != 50 || q.P95 != 95 || q.P99 != 99 || q.Max != 100 {
		t.Fatalf("quantiles=%+v error=%v", q, err)
	}
	for i := range samples {
		if samples[i] != time.Duration(i+1) {
			t.Fatal("summarize mutated its input")
		}
	}
	empty, err := summarizeLoad(nil)
	if err != nil || empty.Samples != 0 {
		t.Fatalf("empty samples: %+v err=%v", empty, err)
	}
	if _, err := summarizeLoad([]time.Duration{time.Millisecond, -time.Nanosecond}); err == nil {
		t.Fatal("negative sample accepted")
	}
}

func TestOpenLoopRejectsInvalidConfig(t *testing.T) {
	valid := loadConfig{Rate: 1000, Duration: time.Second, Workers: 2, Queue: 8, BurstWindow: 250 * time.Millisecond}
	cases := []struct {
		name   string
		mutate func(*loadConfig)
	}{
		{"rate zero", func(c *loadConfig) { c.Rate = 0 }},
		{"rate too high", func(c *loadConfig) { c.Rate = 1000001 }},
		{"duration zero", func(c *loadConfig) { c.Duration = 0 }},
		{"duration too long", func(c *loadConfig) { c.Duration = 61 * time.Second }},
		{"workers zero", func(c *loadConfig) { c.Workers = 0 }},
		{"workers too many", func(c *loadConfig) { c.Workers = 65 }},
		{"queue negative", func(c *loadConfig) { c.Queue = -1 }},
		{"queue too large", func(c *loadConfig) { c.Queue = 65537 }},
		{"burst window zero", func(c *loadConfig) { c.BurstWindow = 0 }},
		{"burst window beyond duration", func(c *loadConfig) { c.BurstWindow = c.Duration + 1 }},
		{"planned below one", func(c *loadConfig) { c.Duration = 500 * time.Microsecond }},
		{"planned above cap", func(c *loadConfig) { c.Rate = 1000000; c.Duration = 3 * time.Second }},
	}
	clock := newFakeLoadClock(modelEpoch)
	for _, tc := range cases {
		cfg := valid
		tc.mutate(&cfg)
		calls := 0
		result, err := runOpenLoop(context.Background(), cfg, modelEpoch, clock, func(uint64) loadOutcome {
			calls++
			return loadOutcome{}
		})
		if err == nil || calls != 0 || !reflect.DeepEqual(result, loadResult{}) {
			t.Fatalf("%s: err=%v calls=%d result=%+v", tc.name, err, calls, result)
		}
	}
}

func TestOpenLoopAccountsOverload(t *testing.T) {
	base := modelEpoch
	clock := newFakeLoadClock(base)
	cfg := loadConfig{Rate: 1000, Duration: 3 * time.Millisecond, Workers: 1, Queue: 1, BurstWindow: 3 * time.Millisecond}
	entered := make(chan uint64, 8)
	gate := make(chan struct{})
	op := func(id uint64) loadOutcome {
		entered <- id
		<-gate
		return loadOutcome{Read: true, Hit: true}
	}
	done := make(chan struct{})
	var result loadResult
	var err error
	// With the worker blocked inside op(id0), advancing past 1ms and 2ms
	// lets the scheduler issue id1 into the freed queue slot and drop id2
	// on the full queue. Waiting for the issue loop to finish makes the
	// drop decision independent of real-time goroutine scheduling.
	issueDone := make(chan struct{})
	openLoopIssueDone = func() { close(issueDone) }
	defer func() { openLoopIssueDone = nil }()
	go func() {
		defer close(done)
		result, err = runOpenLoop(context.Background(), cfg, base, clock, op)
	}()
	if first := <-entered; first != 0 {
		t.Fatalf("first entered id = %d", first)
	}
	clock.AdvanceTo(base.Add(10 * time.Millisecond))
	<-issueDone
	close(gate)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if result.Planned != 3 || result.Attempted != 3 || result.Completed != 2 || result.Dropped != 1 || result.Unissued != 0 {
		t.Fatalf("counts = %+v", result)
	}
	if result.All.Samples != 2 || result.All.P99 != 10*time.Millisecond || result.All.Max != 10*time.Millisecond {
		t.Fatalf("latency measured from service start: %+v", result.All)
	}
	if result.BurstPlanned != 3 || result.BurstAttempted != 3 || result.BurstCompleted != 2 || result.BurstDropped != 1 || result.BurstUnissued != 0 {
		t.Fatalf("burst counts = %+v", result)
	}
	if result.Burst.Samples != 2 || result.Burst.P99 != 10*time.Millisecond {
		t.Fatalf("burst latency = %+v", result.Burst)
	}
}

func TestOpenLoopIncludesSchedulerLag(t *testing.T) {
	base := modelEpoch
	// The clock starts before base so the 0ms request actually waits.
	clock := newFakeLoadClock(base.Add(-time.Second))
	cfg := loadConfig{Rate: 1000, Duration: time.Millisecond, Workers: 1, Queue: 8, BurstWindow: time.Millisecond}
	done := make(chan struct{})
	var result loadResult
	var err error
	go func() {
		defer close(done)
		result, err = runOpenLoop(context.Background(), cfg, base, clock, func(uint64) loadOutcome {
			return loadOutcome{}
		})
	}()
	clock.AdvanceTo(base.Add(5 * time.Millisecond))
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if result.Planned != 1 || result.Completed != 1 || result.All.Samples != 1 {
		t.Fatalf("counts = %+v", result)
	}
	if result.All.P50 != 5*time.Millisecond || result.All.P99 != 5*time.Millisecond || result.All.Max != 5*time.Millisecond {
		t.Fatalf("scheduler lag hidden: %+v", result.All)
	}
}

func TestOpenLoopCancellationJoinsWorkers(t *testing.T) {
	base := modelEpoch
	clock := newFakeLoadClock(base)
	cfg := loadConfig{Rate: 1000, Duration: 10 * time.Millisecond, Workers: 2, Queue: 64, BurstWindow: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan uint64, 8)
	gate := make(chan struct{})
	op := func(id uint64) loadOutcome {
		entered <- id
		<-gate
		return loadOutcome{}
	}
	done := make(chan struct{})
	var result loadResult
	var err error
	go func() {
		defer close(done)
		result, err = runOpenLoop(ctx, cfg, base, clock, op)
	}()
	if first := <-entered; first != 0 {
		t.Fatalf("first entered id = %d", first)
	}
	cancel()
	close(gate)
	<-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if result.Planned != 10 || result.Attempted != 1 || result.Completed != 1 || result.Dropped != 0 || result.Unissued != 9 {
		t.Fatalf("counts = %+v", result)
	}
	if result.BurstPlanned != 10 || result.BurstAttempted != 1 || result.BurstCompleted != 1 || result.BurstDropped != 0 || result.BurstUnissued != 9 {
		t.Fatalf("burst counts = %+v", result)
	}
}

func TestOpenLoopSeparatesSetLatency(t *testing.T) {
	base := modelEpoch
	clock := newFakeLoadClock(base)
	cfg := loadConfig{Rate: 1000, Duration: 4 * time.Millisecond, Workers: 1, Queue: 4, BurstWindow: 4 * time.Millisecond}
	outcomes := []loadOutcome{
		{Read: true, Hit: true},
		{Write: true},
		{Read: true, Hit: true},
		{Write: true},
	}
	entered := make(chan uint64, 8)
	gates := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})}
	op := func(id uint64) loadOutcome {
		entered <- id
		<-gates[id]
		return outcomes[id]
	}
	done := make(chan struct{})
	var result loadResult
	var err error
	go func() {
		defer close(done)
		result, err = runOpenLoop(context.Background(), cfg, base, clock, op)
	}()
	if first := <-entered; first != 0 {
		t.Fatalf("first entered id = %d", first)
	}
	// Planned 0,1,2,3ms; completions at 1,3,10,13ms: lookups 1ms/8ms, sets 2ms/10ms.
	clock.AdvanceTo(base.Add(time.Millisecond))
	close(gates[0])
	if second := <-entered; second != 1 {
		t.Fatalf("second entered id = %d", second)
	}
	clock.AdvanceTo(base.Add(3 * time.Millisecond))
	close(gates[1])
	if third := <-entered; third != 2 {
		t.Fatalf("third entered id = %d", third)
	}
	clock.AdvanceTo(base.Add(10 * time.Millisecond))
	close(gates[2])
	if fourth := <-entered; fourth != 3 {
		t.Fatalf("fourth entered id = %d", fourth)
	}
	clock.AdvanceTo(base.Add(13 * time.Millisecond))
	close(gates[3])
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if result.Planned != 4 || result.Attempted != 4 || result.Completed != 4 || result.Dropped != 0 || result.Unissued != 0 {
		t.Fatalf("counts = %+v", result)
	}
	if result.Reads != 2 || result.Hits != 2 {
		t.Fatalf("reads=%d hits=%d", result.Reads, result.Hits)
	}
	if result.Set.Samples != 2 || result.Set.P99 != 10*time.Millisecond {
		t.Fatalf("set latency = %+v", result.Set)
	}
	if result.BurstSet.Samples != 2 || result.BurstSet.P99 != 10*time.Millisecond {
		t.Fatalf("burst set latency = %+v", result.BurstSet)
	}
	if result.All.Samples != 4 || result.All.P99 != 10*time.Millisecond {
		t.Fatalf("all latency = %+v", result.All)
	}
}

// TestRealLoadClockSmoke runs the real clock end to end with bounded,
// coarse assertions only: accounting equations, not nanosecond precision.
func TestRealLoadClockSmoke(t *testing.T) {
	clock := newRealLoadClock()
	defer clock.Close()
	base := clock.Now()
	cfg := loadConfig{Rate: 1000, Duration: 20 * time.Millisecond, Workers: 2, Queue: 64, BurstWindow: 20 * time.Millisecond}
	result, err := runOpenLoop(context.Background(), cfg, base, clock, func(uint64) loadOutcome {
		return loadOutcome{}
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Planned != 20 || result.Attempted != 20 || result.Completed != 20 || result.Dropped != 0 || result.Unissued != 0 {
		t.Fatalf("counts = %+v", result)
	}
	if result.All.Samples != 20 || result.All.P99 < 0 || result.All.P99 > result.All.Max {
		t.Fatalf("latency = %+v", result.All)
	}
	if result.Elapsed <= 0 {
		t.Fatalf("elapsed = %v", result.Elapsed)
	}
}
