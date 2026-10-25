package memcache

import (
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// modelOp is one cache operation. Kind: 0 Set, 1 SetExpire, 2 Lookup,
// 3 Delete, 4 Clear, 5 Advance, 6 Clean, 7 Get, 8 Size, 9 Close.
type modelOp struct {
	Kind    uint8
	Key     string
	Value   int
	TTL     int64
	Advance time.Duration
}

// modelResult carries only the fields an operation can observably return.
type modelResult struct {
	Value   int
	Present bool
	Size    uint
}

var modelEpoch = time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)

type modelEntry struct {
	value    int
	deadline time.Time // zero means no TTL
}

// cacheModel is the independent reference model: a map plus a recency slice,
// with logical expiry filtering. It never touches the production heap, list,
// or cleaner.
type cacheModel struct {
	capacity  int
	enableTTL bool
	closed    bool
	now       time.Time
	items     map[string]modelEntry
	order     []string // most-recently-used first
}

func newCacheModel(capacity int, ttl bool, now time.Time) *cacheModel {
	return &cacheModel{capacity: capacity, enableTTL: ttl, now: now, items: make(map[string]modelEntry)}
}

func (m *cacheModel) clone() *cacheModel {
	fork := &cacheModel{
		capacity:  m.capacity,
		enableTTL: m.enableTTL,
		closed:    m.closed,
		now:       m.now,
		items:     make(map[string]modelEntry, len(m.items)),
		order:     make([]string, len(m.order)),
	}
	copy(fork.order, m.order)
	for key, entry := range m.items {
		fork.items[key] = entry
	}
	return fork
}

func (m *cacheModel) removeKey(key string) {
	delete(m.items, key)
	for i, existing := range m.order {
		if key == existing {
			m.order = append(m.order[:i], m.order[i+1:]...)
			return
		}
	}
}

func (m *cacheModel) touch(key string) {
	for i, existing := range m.order {
		if key == existing {
			copy(m.order[1:i+1], m.order[:i])
			m.order[0] = key
			return
		}
	}
}

// purgeExpired drops entries whose deadline passed at the model's now,
// mirroring purgeExpiredLocked.
func (m *cacheModel) purgeExpired() {
	for key, entry := range m.items {
		if !entry.deadline.IsZero() && !entry.deadline.After(m.now) {
			m.removeKey(key)
		}
	}
}

// view returns live values and live LRU order at the given now, without
// mutating the model. Empty containers compare equal to nil ones.
func (m *cacheModel) view(now time.Time) (map[string]int, []string) {
	values := make(map[string]int, len(m.items))
	order := make([]string, 0, len(m.order))
	for _, key := range m.order {
		entry, ok := m.items[key]
		if !ok {
			continue
		}
		if !entry.deadline.IsZero() && !entry.deadline.After(now) {
			continue
		}
		values[key] = entry.value
		order = append(order, key)
	}
	return values, order
}

func (m *cacheModel) set(key string, value int, ttl int64, expiring bool) {
	if m.closed || m.capacity == 0 {
		return
	}
	m.purgeExpired()
	var deadline time.Time
	if expiring && m.enableTTL {
		if ttl <= 0 {
			if _, ok := m.items[key]; ok {
				m.removeKey(key)
			}
			return
		}
		duration := maxTTL
		if ttl <= int64(maxTTL/time.Second) {
			duration = time.Duration(ttl) * time.Second
		}
		deadline = m.now.Add(duration)
	}
	if _, ok := m.items[key]; ok {
		m.items[key] = modelEntry{value: value, deadline: deadline}
		m.touch(key)
		return
	}
	if len(m.items) >= m.capacity {
		m.removeKey(m.order[len(m.order)-1])
	}
	m.items[key] = modelEntry{value: value, deadline: deadline}
	m.order = append([]string{key}, m.order...)
}

// apply advances the model by one operation and returns its observable
// result.
func (m *cacheModel) apply(op modelOp) modelResult {
	switch op.Kind {
	case 0:
		m.set(op.Key, op.Value, 0, false)
	case 1:
		m.set(op.Key, op.Value, op.TTL, true)
	case 2, 7:
		entry, ok := m.items[op.Key]
		if ok && !entry.deadline.IsZero() && !entry.deadline.After(m.now) {
			m.removeKey(op.Key)
			return modelResult{}
		}
		if !ok {
			return modelResult{}
		}
		m.touch(op.Key)
		return modelResult{Value: entry.value, Present: true}
	case 3:
		if _, ok := m.items[op.Key]; ok {
			m.removeKey(op.Key)
		}
	case 4:
		if !m.closed {
			m.items = make(map[string]modelEntry)
			m.order = nil
		}
	case 5:
		m.now = m.now.Add(op.Advance)
	case 6:
		m.purgeExpired()
	case 8:
		m.purgeExpired()
		return modelResult{Size: uint(len(m.items))}
	case 9:
		m.closed = true
		m.items = make(map[string]modelEntry)
		m.order = nil
	}
	return modelResult{}
}

func sameIntMaps(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if other, ok := b[key]; !ok || other != value {
			return false
		}
	}
	return true
}

func sameStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// stepModelOp runs one operation against both the real cache and the model,
// compares the observable result and the Task 1 live view, and returns the
// result together with the post-operation physical snapshot.
func stepModelOp(t testing.TB, step int, m *Memcache[int], clock *testClock, model *cacheModel, op modelOp) (modelResult, cacheSnapshot) {
	t.Helper()
	want := model.apply(op)
	var got modelResult
	switch op.Kind {
	case 0:
		m.Set(op.Key, op.Value)
	case 1:
		m.SetExpire(op.Key, op.Value, op.TTL)
	case 2:
		value, present := m.Lookup(op.Key)
		got = modelResult{Value: value, Present: present}
	case 3:
		m.Delete(op.Key)
	case 4:
		m.Clear()
	case 5:
		clock.Advance(op.Advance)
	case 6:
		m.checkExpired()
	case 7:
		pointer := m.Get(op.Key)
		if pointer != nil {
			got = modelResult{Value: *pointer, Present: true}
			*pointer = -1
		}
	case 8:
		got = modelResult{Size: m.Size()}
	case 9:
		m.Close()
	}
	if op.Kind == 2 || op.Kind == 7 {
		if got.Present != want.Present || (want.Present && got.Value != want.Value) {
			t.Fatalf("step %d op %+v: cache %+v, model %+v", step, op, got, want)
		}
	}
	if op.Kind == 8 && got.Size != want.Size {
		t.Fatalf("step %d op %+v: Size cache=%d, model=%d", step, op, got.Size, want.Size)
	}
	snap := snapshotCache(m)
	assertInvariants(t, m)
	gotValues, gotOrder := liveSnapshot(snap, clock.Now())
	wantValues, wantOrder := model.view(clock.Now())
	if !sameIntMaps(gotValues, wantValues) || !sameStringSlices(gotOrder, wantOrder) {
		t.Fatalf("step %d op %+v: live values cache=%v model=%v, order cache=%v model=%v",
			step, op, gotValues, wantValues, gotOrder, wantOrder)
	}
	if op.Kind == 6 {
		for key, item := range snap.Items {
			if !item.Deadline.IsZero() && !item.Deadline.After(clock.Now()) {
				t.Fatalf("step %d: Clean left expired physical entry %q", step, key)
			}
		}
	}
	return want, snap
}

// runModelSequence executes ops against a fresh cache and model, verifying
// every step.
func runModelSequence(t testing.TB, capacity uint, ttl bool, ops []modelOp) {
	t.Helper()
	clock := &testClock{now: modelEpoch}
	m := newMemcache[int](capacity, ttl, clock.Now, false)
	defer m.Close()
	model := newCacheModel(int(capacity), ttl, modelEpoch)
	for step, op := range ops {
		stepModelOp(t, step, m, clock, model, op)
	}
}

// TestModelExpiredSequenceWithoutSize drives a real cache and the reference
// model through an expiry sequence, comparing the Task 1 live view at every
// step. No observation may purge or refresh anything: after Lookup of b the
// expired physical entry a must still exist.
func TestModelExpiredSequenceWithoutSize(t *testing.T) {
	clock := &testClock{now: modelEpoch}
	m := newMemcache[int](3, true, clock.Now, false)
	defer m.Close()
	model := newCacheModel(3, true, modelEpoch)
	var last cacheSnapshot
	for step, op := range []modelOp{
		{Kind: 1, Key: "a", Value: 1, TTL: 1},
		{Kind: 0, Key: "b", Value: 2},
		{Kind: 5, Advance: time.Second},
		{Kind: 2, Key: "b"},
		{Kind: 3, Key: "missing"},
		{Kind: 2, Key: "a"},
		{Kind: 0, Key: "c", Value: 3},
	} {
		_, last = stepModelOp(t, step, m, clock, model, op)
		if op.Kind == 2 && op.Key == "b" {
			if _, stillThere := last.Items["a"]; !stillThere {
				t.Fatal("Lookup of another key purged the expired physical entry")
			}
		}
	}
	values, order := liveSnapshot(last, clock.Now())
	if !reflect.DeepEqual(values, map[string]int{"b": 2, "c": 3}) {
		t.Fatalf("final live values = %v", values)
	}
	if !reflect.DeepEqual(order, []string{"c", "b"}) {
		t.Fatalf("final live order = %v", order)
	}
}

func TestModelDisabledTTLAndClosedWrites(t *testing.T) {
	ops := []modelOp{
		{Kind: 1, Key: "k", Value: 7, TTL: 0},
		{Kind: 2, Key: "k"},
		{Kind: 1, Key: "k", Value: 8, TTL: -1},
		{Kind: 2, Key: "k"},
		{Kind: 9},
		{Kind: 0, Key: "k", Value: 9},
		{Kind: 2, Key: "k"},
		{Kind: 4},
		{Kind: 8},
	}
	// Model self-test against the legacy contract.
	model := newCacheModel(1, false, modelEpoch)
	if got := model.apply(modelOp{Kind: 1, Key: "k", Value: 7, TTL: 0}); got.Present {
		t.Fatalf("SetExpire result %+v", got)
	}
	if got := model.apply(modelOp{Kind: 2, Key: "k"}); !got.Present || got.Value != 7 {
		t.Fatal(got)
	}
	model.apply(modelOp{Kind: 1, Key: "k", Value: 8, TTL: -1})
	if got := model.apply(modelOp{Kind: 2, Key: "k"}); !got.Present || got.Value != 8 {
		t.Fatalf("nonpositive TTL with TTL disabled: %+v", got)
	}
	model.apply(modelOp{Kind: 9})
	model.apply(modelOp{Kind: 0, Key: "k", Value: 9})
	if got := model.apply(modelOp{Kind: 2, Key: "k"}); got.Present {
		t.Fatal("closed model reopened")
	}
	// The same sequence must match the real cache.
	runModelSequence(t, 1, false, ops)
}

func TestModelBoundaries(t *testing.T) {
	cases := []struct {
		name     string
		capacity uint
		ttl      bool
		ops      []modelOp
		want     map[int]modelResult
	}{
		{
			name:     "deadline strictly before and exactly at now",
			capacity: 1,
			ttl:      true,
			ops: []modelOp{
				{Kind: 1, Key: "a", Value: 1, TTL: 1},
				{Kind: 5, Advance: time.Second - time.Nanosecond},
				{Kind: 2, Key: "a"},
				{Kind: 5, Advance: time.Nanosecond},
				{Kind: 2, Key: "a"},
				{Kind: 8},
			},
			want: map[int]modelResult{2: {Value: 1, Present: true}, 4: {}, 5: {Size: 0}},
		},
		{
			name:     "zero capacity stores nothing",
			capacity: 0,
			ttl:      true,
			ops: []modelOp{
				{Kind: 0, Key: "a", Value: 5},
				{Kind: 2, Key: "a"},
				{Kind: 1, Key: "a", Value: 6, TTL: 1},
				{Kind: 2, Key: "a"},
				{Kind: 8},
			},
			want: map[int]modelResult{1: {}, 3: {}, 4: {Size: 0}},
		},
		{
			name:     "empty key round-trips",
			capacity: 2,
			ttl:      true,
			ops: []modelOp{
				{Kind: 0, Key: "", Value: 7},
				{Kind: 2, Key: ""},
			},
			want: map[int]modelResult{1: {Value: 7, Present: true}},
		},
		{
			name:     "zero int value is a hit",
			capacity: 2,
			ttl:      false,
			ops: []modelOp{
				{Kind: 0, Key: "k", Value: 0},
				{Kind: 2, Key: "k"},
				{Kind: 7, Key: "k"},
			},
			want: map[int]modelResult{1: {Value: 0, Present: true}, 2: {Value: 0, Present: true}},
		},
		{
			name:     "saturated TTL stays live",
			capacity: 1,
			ttl:      true,
			ops: []modelOp{
				{Kind: 1, Key: "k", Value: 3, TTL: math.MaxInt64},
				{Kind: 5, Advance: time.Hour},
				{Kind: 2, Key: "k"},
			},
			want: map[int]modelResult{2: {Value: 3, Present: true}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := &testClock{now: modelEpoch}
			m := newMemcache[int](tc.capacity, tc.ttl, clock.Now, false)
			defer m.Close()
			model := newCacheModel(int(tc.capacity), tc.ttl, modelEpoch)
			for step, op := range tc.ops {
				got, _ := stepModelOp(t, step, m, clock, model, op)
				want, ok := tc.want[step]
				if !ok {
					continue
				}
				switch op.Kind {
				case 2, 7:
					if got.Present != want.Present || (want.Present && got.Value != want.Value) {
						t.Fatalf("step %d %+v: got %+v, want %+v", step, op, got, want)
					}
				case 8:
					if got.Size != want.Size {
						t.Fatalf("step %d Size: got %d, want %d", step, got.Size, want.Size)
					}
				}
			}
		})
	}
	// clone independence: mutating the original never reaches the fork.
	original := newCacheModel(2, true, modelEpoch)
	original.apply(modelOp{Kind: 0, Key: "a", Value: 1})
	original.apply(modelOp{Kind: 1, Key: "b", Value: 2, TTL: 5})
	fork := original.clone()
	original.apply(modelOp{Kind: 0, Key: "c", Value: 3}) // evicts LRU tail "a"
	original.apply(modelOp{Kind: 5, Advance: time.Hour})
	original.apply(modelOp{Kind: 6})
	forkValues, forkOrder := fork.view(modelEpoch)
	if !reflect.DeepEqual(forkValues, map[string]int{"a": 1, "b": 2}) || !reflect.DeepEqual(forkOrder, []string{"b", "a"}) {
		t.Fatalf("clone observed original mutation: values=%v order=%v", forkValues, forkOrder)
	}
	liveValues, liveOrder := original.view(modelEpoch.Add(time.Hour))
	if !reflect.DeepEqual(liveValues, map[string]int{"c": 3}) || !reflect.DeepEqual(liveOrder, []string{"c"}) {
		t.Fatalf("original after eviction+expiry: values=%v order=%v", liveValues, liveOrder)
	}
}

func TestModelLargeSequences(t *testing.T) {
	const seed = int64(20261025)
	for _, config := range []struct {
		capacity uint
		ttl      bool
	}{{64, true}, {64, false}, {1024, true}, {1024, false}} {
		t.Run(fmt.Sprintf("capacity=%d,ttl=%v", config.capacity, config.ttl), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			ops := make([]modelOp, 0, 4096)
			for i := 0; i < 4096; i++ {
				key := strconv.Itoa(rng.Intn(2 * int(config.capacity)))
				op := modelOp{Key: key, Value: rng.Intn(1000)}
				switch rng.Intn(10) {
				case 0:
					op.Kind = 0
				case 1:
					op.Kind = 1
					op.TTL = int64(rng.Intn(8)) - 1
				case 2:
					op.Kind = 2
				case 3:
					op.Kind = 3
				case 4:
					op.Kind = 4
				case 5:
					op.Kind = 5
					op.Advance = time.Duration(rng.Intn(5)+1) * time.Second
				case 6:
					op.Kind = 6
				case 7:
					op.Kind = 7
				case 8:
					op.Kind = 8
				case 9:
					op.Kind = 9
				}
				ops = append(ops, op)
			}
			t.Cleanup(func() {
				if t.Failed() {
					prefix := ops
					if len(prefix) > 16 {
						prefix = prefix[:16]
					}
					t.Logf("seed=%d config=%+v operation prefix: %+v", seed, config, prefix)
				}
			})
			runModelSequence(t, config.capacity, config.ttl, ops)
		})
	}
}
