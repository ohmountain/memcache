package memcache

import (
	"strconv"
	"testing"
)

func TestRefillHintFor(t *testing.T) {
	cases := []struct {
		size, cap, want int
	}{
		{0, 100, 0},
		{50, 100, 50},
		{100, 100, 100},
		{5000, 100, 100},
		{10000, 10000, 4096},
		{3, 10000, 3},
	}
	for _, tc := range cases {
		if got := refillHintFor(tc.size, tc.cap); got != tc.want {
			t.Errorf("refillHintFor(%d, %d) = %d, want %d", tc.size, tc.cap, got, tc.want)
		}
	}
}

// TestClearRefillKeepsEntries: a Clear followed by a refill still stores and
// retrieves everything, and the hint recomputes from the size actually
// present at each Clear — a sparse refill lowers it, so a Clear-and-forget
// sequence does not pin the map at the old high-water mark.
func TestClearRefillKeepsEntries(t *testing.T) {
	m := WithOptions[int](Options{Capacity: 100})
	defer m.Close()
	for i := 0; i < 100; i++ {
		m.Set(strconv.Itoa(i), i)
	}
	m.Clear()
	for i := 0; i < 100; i++ {
		m.Set(strconv.Itoa(i), i*2)
	}
	if got := m.Len(); got != 100 {
		t.Fatalf("Len = %d after refill, want 100", got)
	}
	for i := 0; i < 100; i++ {
		if v, ok := m.Lookup(strconv.Itoa(i)); !ok || v != i*2 {
			t.Fatalf("key %d = (%d, %v) after refill", i, v, ok)
		}
	}
	assertInvariants(t, m)

	m.Clear()
	if hint := int(m.state.shards[0].refillHint); hint != 100 {
		t.Fatalf("refillHint after a full cycle = %d, want 100", hint)
	}
	for i := 0; i < 3; i++ {
		m.Set(strconv.Itoa(i), i)
	}
	m.Clear()
	if hint := int(m.state.shards[0].refillHint); hint != 3 {
		t.Fatalf("refillHint after a sparse cycle = %d, want 3", hint)
	}
	assertInvariants(t, m)
}
