package memcache

import (
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordHistory executes every worker program concurrently against m,
// bracketing each operation with atomic call/return sequence numbers. Workers
// write only their own pre-allocated slots; results merge after the join, in
// call order, with IDs assigned by merged position. Kind 5 (Advance) is not
// supported: the shared test clock must not advance concurrently.
func recordHistory(m *Memcache[int], programs [][]modelOp) []historyEvent {
	var sequence uint64
	var wg sync.WaitGroup
	start := make(chan struct{})
	recorded := make([][]historyEvent, len(programs))
	for w, program := range programs {
		wg.Add(1)
		go func(w int, program []modelOp) {
			defer wg.Done()
			<-start
			events := make([]historyEvent, len(program))
			for i, op := range program {
				call := atomic.AddUint64(&sequence, 1)
				result := executeRecordedOp(m, op)
				ret := atomic.AddUint64(&sequence, 1)
				events[i] = historyEvent{Call: call, Return: ret, Op: op, Result: result}
			}
			recorded[w] = events
		}(w, program)
	}
	close(start)
	wg.Wait()
	total := 0
	for _, events := range recorded {
		total += len(events)
	}
	merged := make([]historyEvent, 0, total)
	for _, events := range recorded {
		merged = append(merged, events...)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Call < merged[j].Call })
	for i := range merged {
		merged[i].ID = i
	}
	return merged
}

func executeRecordedOp(m *Memcache[int], op modelOp) modelResult {
	switch op.Kind {
	case 0:
		m.Set(op.Key, op.Value)
	case 1:
		m.SetExpire(op.Key, op.Value, op.TTL)
	case 2:
		value, present := m.Lookup(op.Key)
		return modelResult{Value: value, Present: present}
	case 3:
		m.Delete(op.Key)
	case 4:
		m.Clear()
	case 6:
		m.checkExpired()
	case 7:
		if pointer := m.Get(op.Key); pointer != nil {
			return modelResult{Value: *pointer, Present: true}
		}
	case 8:
		return modelResult{Size: m.Size()}
	case 9:
		m.Close()
	}
	return modelResult{}
}

// requireValidHistory records concurrent worker programs on m, checks the
// history against a fresh model with the post-join snapshot as the final
// state, and requires a valid verdict — anything else fails with the full
// history printed.
func requireValidHistory(t *testing.T, m *Memcache[int], capacity uint, ttl bool, programs [][]modelOp) {
	t.Helper()
	events := recordHistory(m, programs)
	final := snapshotCache(m)
	verdict, _ := checkHistory(newCacheModel(int(capacity), ttl, modelEpoch), events, &final, historyDefaultBudget)
	if verdict != historyValid {
		t.Fatalf("verdict %q, history: %+v", verdict, events)
	}
	assertInvariants(t, m)
}

// TestHistoryRecordedConcurrentOperations records real concurrent histories
// on a fresh cache per seed and requires the checker to find a valid
// serialization for every one. Inconclusive is a failure: it prints the full
// history.
func TestHistoryRecordedConcurrentOperations(t *testing.T) {
	for seed := 0; seed < 100; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(int64(seed)))
			workers := 2 + rng.Intn(2)
			total := 4 + rng.Intn(5)
			keys := []string{"a", "b", "c"}[:2+rng.Intn(2)]
			ttl := seed%2 == 0
			m, _, _ := newModelSequenceFixture(t, 2, ttl)
			programs := make([][]modelOp, workers)
			for i := 0; i < total; i++ {
				key := keys[rng.Intn(len(keys))]
				var op modelOp
				switch pick := rng.Intn(20); {
				case pick < 7:
					op = modelOp{Kind: 0, Key: key, Value: rng.Intn(50)}
				case pick < 10:
					op = modelOp{Kind: 1, Key: key, Value: rng.Intn(50), TTL: 3600}
				case pick < 15:
					op = modelOp{Kind: 2, Key: key}
				case pick < 18:
					op = modelOp{Kind: 3, Key: key}
				case pick == 18:
					op = modelOp{Kind: 4}
				case pick == 19:
					op = modelOp{Kind: 9}
				}
				programs[i%workers] = append(programs[i%workers], op)
			}
			requireValidHistory(t, m, 2, ttl, programs)
		})
	}
}

// TestHistoryScenarios pins deterministic coverage of same-key read/write/
// delete races, capacity eviction, Clear, and Close, which the random seed
// sweep only hits probabilistically.
func TestHistoryScenarios(t *testing.T) {
	scenarios := []struct {
		name     string
		ttl      bool
		capacity uint
		programs [][]modelOp
	}{
		{"clear then read", false, 4, [][]modelOp{
			{{Kind: 0, Key: "a", Value: 1}, {Kind: 4}},
			{{Kind: 2, Key: "a"}},
		}},
		{"close then write", false, 4, [][]modelOp{
			{{Kind: 0, Key: "a", Value: 1}, {Kind: 9}},
			{{Kind: 0, Key: "b", Value: 2}, {Kind: 2, Key: "b"}},
		}},
		{"eviction under capacity", false, 2, [][]modelOp{
			{{Kind: 0, Key: "a", Value: 1}, {Kind: 0, Key: "c", Value: 3}},
			{{Kind: 0, Key: "b", Value: 2}, {Kind: 2, Key: "a"}},
		}},
		{"ttl renewal race", true, 1, [][]modelOp{
			{{Kind: 1, Key: "k", Value: 2, TTL: 3600}},
			{{Kind: 6}},
		}},
	}
	for _, scenario := range scenarios {
		scenario := scenario
		t.Run(scenario.name, func(t *testing.T) {
			m, _, _ := newModelSequenceFixture(t, scenario.capacity, scenario.ttl)
			requireValidHistory(t, m, scenario.capacity, scenario.ttl, scenario.programs)
		})
	}
}

// TestHistoryRenewalAndCleanup races a TTL renewal against checkExpired on a
// frozen clock, then advances past the original deadline: the renewed value
// must survive. Workers never call t.Fatal.
func TestHistoryRenewalAndCleanup(t *testing.T) {
	m, clock, _ := newModelSequenceFixture(t, 1, true)
	m.SetExpire("k", 1, 1)
	events := recordHistory(m, [][]modelOp{
		{{Kind: 1, Key: "k", Value: 2, TTL: 3600}},
		{{Kind: 6}},
	})
	final := snapshotCache(m)
	model := newCacheModel(1, true, modelEpoch)
	model.apply(modelOp{Kind: 1, Key: "k", Value: 1, TTL: 1})
	if verdict, _ := checkHistory(model, events, &final, historyDefaultBudget); verdict != historyValid {
		t.Fatalf("verdict %q, history: %+v", verdict, events)
	}
	clock.Advance(time.Second) // past the original deadline
	requireValue(t, m, "k", 2)
	assertInvariants(t, m)
}
