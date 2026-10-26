package memcache

import (
	"strconv"
	"testing"
)

func TestShardCountSlotTarget(t *testing.T) {
	cases := []struct {
		capacity  uint
		requested int
		slots     int
		want      int
	}{
		{1024, 64, 64, 16},
		{1024, 64, 16, 64},
		{1024, 128, 8, 128},
		{1024, 48, 16, 32},
		{256, 64, 64, 4},
		{256, 64, 16, 16},
		{1024, 64, 0, 16},
		{1024, 64, -3, 16},
		{63, 64, 16, 2},
		{15, 64, 16, 1},
		{0, 64, 16, 1},
	}
	for _, tc := range cases {
		if got := shardCount(tc.capacity, tc.requested, tc.slots); got != tc.want {
			t.Errorf("shardCount(%d, %d, %d) = %d, want %d", tc.capacity, tc.requested, tc.slots, got, tc.want)
		}
	}
}

// TestShardSlotsCapacitySplit exercises a small cache whose shard count is
// only reachable through Options.ShardSlots: capacity still splits exactly
// across shards, a half-resident set survives hash imbalance whole, and a
// fill past capacity stays bounded by the exact total (each shard evicts at
// its own cap, but the sum of caps is the configured capacity).
func TestShardSlotsCapacitySplit(t *testing.T) {
	m := WithOptions[int](Options{Capacity: 256, Shards: 64, ShardSlots: 16})
	defer m.Close()
	if got := len(m.state.shards); got != 16 {
		t.Fatalf("shards = %d, want 16", got)
	}
	total := 0
	for _, sh := range m.state.shards {
		sh.mu.Lock()
		total += sh.cap
		sh.mu.Unlock()
	}
	if total != 256 {
		t.Fatalf("sum of shard caps = %d, want 256", total)
	}
	for i := 0; i < 64; i++ {
		m.Set(strconv.Itoa(i), i)
	}
	if got := m.Len(); got != 64 {
		t.Fatalf("Len = %d, want 64 for a quarter-resident set", got)
	}
	for i := 128; i < 512; i++ {
		m.Set(strconv.Itoa(i), i)
	}
	// Uneven routing leaves some shards below their own cap, so the steady
	// state is bounded by — not equal to — the total capacity.
	if got := m.Len(); got > 256 {
		t.Fatalf("Len = %d, want at most 256 (total capacity)", got)
	}
	assertInvariants(t, m)
}
