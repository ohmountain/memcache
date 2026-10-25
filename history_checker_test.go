package memcache

import (
	"testing"
)

// historyEvent records one operation with atomic call/return sequence numbers
// and its observable result.
type historyEvent struct {
	ID     int
	Call   uint64
	Return uint64
	Op     modelOp
	Result modelResult
}

type historyVerdict string

const (
	historyValid        historyVerdict = "valid"
	historyInvalid      historyVerdict = "invalid"
	historyInconclusive historyVerdict = "inconclusive"
)

const (
	historyMaxEvents     = 8
	historyDefaultBudget = 100000
)

// historyResultMatches compares only the fields the operation can observably
// return; void operations carry no requirement.
func historyResultMatches(event historyEvent, got modelResult) bool {
	switch event.Op.Kind {
	case 2, 7:
		return got.Present == event.Result.Present &&
			(!event.Result.Present || got.Value == event.Result.Value)
	case 8:
		return got.Size == event.Result.Size
	}
	return true
}

// checkHistory searches for a serialization of events that respects real
// time — A must precede B only when Return(A) < Call(B) — and reproduces
// every recorded result on the model. When final is non-nil, the leaf must
// also match the frozen-moment live values and live LRU order; a leaf that
// does not match only prunes that branch. It returns "valid" plus the
// witness event IDs in applied order, "invalid" when the whole search space
// is exhausted, and "inconclusive" for malformed input, more than eight
// events, or an exhausted state budget (the root counts as one state).
func checkHistory(initial *cacheModel, events []historyEvent, final *cacheSnapshot, maxStates int) (historyVerdict, []int) {
	if maxStates <= 0 || len(events) == 0 || len(events) > historyMaxEvents {
		return historyInconclusive, nil
	}
	seen := make(map[int]bool, len(events))
	for _, event := range events {
		if seen[event.ID] || event.Call >= event.Return {
			return historyInconclusive, nil
		}
		seen[event.ID] = true
	}
	n := len(events)
	// predecessors[i] is the bitmask of events real time forces before i.
	predecessors := make([]uint16, n)
	for i := range events {
		for j := range events {
			if i != j && events[j].Return < events[i].Call {
				predecessors[i] |= 1 << uint(j)
			}
		}
	}
	allChosen := uint16(1)<<uint(n) - 1
	states := 0
	budgetExhausted := false
	witness := make([]int, 0, n)
	var dfs func(model *cacheModel, chosen uint16, order []int) bool
	dfs = func(model *cacheModel, chosen uint16, order []int) bool {
		states++
		if states > maxStates {
			budgetExhausted = true
			return false
		}
		if chosen == allChosen {
			if final == nil {
				witness = append(witness[:0], order...)
				return true
			}
			modelValues, modelOrder := model.view(model.now)
			finalValues, finalOrder := liveSnapshot(*final, model.now)
			if sameIntMaps(modelValues, finalValues) && sameStringSlices(modelOrder, finalOrder) {
				witness = append(witness[:0], order...)
				return true
			}
			return false
		}
		for i := 0; i < n; i++ {
			bit := uint16(1) << uint(i)
			if chosen&bit != 0 {
				continue
			}
			if predecessors[i]&^chosen != 0 {
				continue
			}
			branch := model.clone()
			if !historyResultMatches(events[i], branch.apply(events[i].Op)) {
				continue
			}
			if dfs(branch, chosen|bit, append(order, events[i].ID)) {
				return true
			}
		}
		return false
	}
	if dfs(initial.clone(), 0, make([]int, 0, n)) {
		return historyValid, witness
	}
	if budgetExhausted {
		return historyInconclusive, nil
	}
	return historyInvalid, nil
}

// setThenReadEvents builds a two-event history where the Set fully completes
// before the read is invoked.
func setThenReadEvents(readResult modelResult) []historyEvent {
	return []historyEvent{
		{ID: 0, Call: 1, Return: 2, Op: modelOp{Kind: 0, Key: "k", Value: 1}},
		{ID: 1, Call: 3, Return: 4, Op: modelOp{Kind: 2, Key: "k"}, Result: readResult},
	}
}

func TestHistoryRejectsImpossibleRead(t *testing.T) {
	events := setThenReadEvents(modelResult{Value: 2, Present: true})
	v, witness := checkHistory(newCacheModel(2, false, modelEpoch), events, nil, 100000)
	if v != historyVerdict("invalid") || len(witness) != 0 {
		t.Fatal("accepted impossible history")
	}
}

func TestHistoryRespectsRealTime(t *testing.T) {
	// The Set returned before the Lookup was invoked, so every serialization
	// must see the write: a reported miss is impossible.
	events := setThenReadEvents(modelResult{})
	v, _ := checkHistory(newCacheModel(2, false, modelEpoch), events, nil, 100000)
	if v != historyVerdict("invalid") {
		t.Fatalf("verdict = %q, want invalid", v)
	}
}

func TestHistoryBudgetExhausted(t *testing.T) {
	// Two overlapping writes plus a read need more than one search state.
	events := []historyEvent{
		{ID: 0, Call: 1, Return: 6, Op: modelOp{Kind: 0, Key: "k", Value: 1}},
		{ID: 1, Call: 2, Return: 5, Op: modelOp{Kind: 0, Key: "k", Value: 2}},
		{ID: 2, Call: 7, Return: 8, Op: modelOp{Kind: 2, Key: "k"}, Result: modelResult{Value: 2, Present: true}},
	}
	v, _ := checkHistory(newCacheModel(2, false, modelEpoch), events, nil, 1)
	if v != historyVerdict("inconclusive") {
		t.Fatalf("verdict = %q, want inconclusive", v)
	}
	// The same history must decide with a real budget.
	v, witness := checkHistory(newCacheModel(2, false, modelEpoch), events, nil, 100000)
	if v != historyVerdict("valid") || len(witness) != 3 {
		t.Fatalf("verdict = %q witness = %v, want valid full witness", v, witness)
	}
}

func TestHistoryAcceptsOverlappingWrites(t *testing.T) {
	// The two writes overlap, so either serialization is legal, but the read
	// happens after both returned and must observe the second value.
	events := []historyEvent{
		{ID: 0, Call: 1, Return: 6, Op: modelOp{Kind: 0, Key: "k", Value: 1}},
		{ID: 1, Call: 2, Return: 5, Op: modelOp{Kind: 0, Key: "k", Value: 2}},
		{ID: 2, Call: 7, Return: 8, Op: modelOp{Kind: 2, Key: "k"}, Result: modelResult{Value: 2, Present: true}},
	}
	v, witness := checkHistory(newCacheModel(2, false, modelEpoch), events, nil, 100000)
	if v != historyVerdict("valid") {
		t.Fatalf("verdict = %q, want valid", v)
	}
	// The witness must replay on a fresh model and reproduce every result.
	byID := map[int]historyEvent{}
	for _, e := range events {
		byID[e.ID] = e
	}
	model := newCacheModel(2, false, modelEpoch)
	for _, id := range witness {
		got := model.apply(byID[id].Op)
		switch byID[id].Op.Kind {
		case 2, 7:
			if got.Present != byID[id].Result.Present || (byID[id].Result.Present && got.Value != byID[id].Result.Value) {
				t.Fatalf("witness replay of %d: got %+v, want %+v", id, got, byID[id].Result)
			}
		case 8:
			if got.Size != byID[id].Result.Size {
				t.Fatalf("witness replay of %d: Size %d, want %d", id, got.Size, byID[id].Result.Size)
			}
		}
	}
}

func TestHistoryRejectsMalformedEvents(t *testing.T) {
	base := []historyEvent{
		{ID: 0, Call: 1, Return: 2, Op: modelOp{Kind: 0, Key: "k", Value: 1}},
		{ID: 1, Call: 3, Return: 4, Op: modelOp{Kind: 2, Key: "k"}, Result: modelResult{Value: 1, Present: true}},
	}
	cases := []struct {
		name   string
		events []historyEvent
		budget int
	}{
		{"duplicate ids", []historyEvent{
			{ID: 0, Call: 1, Return: 2, Op: modelOp{Kind: 0, Key: "k", Value: 1}},
			{ID: 0, Call: 3, Return: 4, Op: modelOp{Kind: 2, Key: "k"}, Result: modelResult{Value: 1, Present: true}},
		}, 100000},
		{"call not before return", []historyEvent{
			{ID: 0, Call: 5, Return: 5, Op: modelOp{Kind: 0, Key: "k", Value: 1}},
		}, 100000},
		{"empty history", nil, 100000},
		{"nonpositive budget", base, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, witness := checkHistory(newCacheModel(2, false, modelEpoch), tc.events, nil, tc.budget)
			if v != historyVerdict("inconclusive") || witness != nil {
				t.Fatalf("verdict = %q witness = %v, want inconclusive", v, witness)
			}
		})
	}
	// More than eight operations are refused outright.
	var tooMany []historyEvent
	for i := 0; i < 9; i++ {
		tooMany = append(tooMany, historyEvent{ID: i, Call: uint64(2*i + 1), Return: uint64(2*i + 2), Op: modelOp{Kind: 3, Key: "k"}})
	}
	if v, _ := checkHistory(newCacheModel(2, false, modelEpoch), tooMany, nil, 100000); v != historyVerdict("inconclusive") {
		t.Fatalf("verdict = %q, want inconclusive for oversized history", v)
	}
}

func TestHistoryMatchesFinalState(t *testing.T) {
	overlapping := []historyEvent{
		{ID: 0, Call: 1, Return: 6, Op: modelOp{Kind: 0, Key: "k", Value: 1}},
		{ID: 1, Call: 2, Return: 5, Op: modelOp{Kind: 0, Key: "k", Value: 2}},
	}
	withTwo := &cacheSnapshot{Capacity: 2, Items: map[string]snapshotItem{"k": {Value: 2}}, Order: []string{"k"}}
	v, witness := checkHistory(newCacheModel(2, false, modelEpoch), overlapping, withTwo, 100000)
	if v != historyVerdict("valid") || len(witness) != 2 {
		t.Fatalf("verdict = %q witness = %v, want valid witness", v, witness)
	}
	withThree := &cacheSnapshot{Capacity: 2, Items: map[string]snapshotItem{"k": {Value: 3}}, Order: []string{"k"}}
	v, witness = checkHistory(newCacheModel(2, false, modelEpoch), overlapping, withThree, 100000)
	if v != historyVerdict("invalid") || len(witness) != 0 {
		t.Fatalf("verdict = %q witness = %v, want invalid", v, witness)
	}
}
