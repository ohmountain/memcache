package memcache

import (
	"testing"
	"time"
)

// TestGetInto pins the allocation-free read-into API: a hit writes the value
// copy into the caller's destination, a miss leaves it untouched, and the
// expiry semantics match Lookup.
func TestGetInto(t *testing.T) {
	m, clock := clockCache(t, 4)
	m.Set("a", 1)

	dst := 0
	if ok := m.GetInto("a", &dst); !ok || dst != 1 {
		t.Fatalf("GetInto = (%d, %v), want (1, true)", dst, ok)
	}
	dst = 42
	if ok := m.GetInto("a", &dst); !ok || dst != 1 {
		t.Fatalf("GetInto = (%d, %v), want (1, true) overwriting dst", dst, ok)
	}

	dst = 42
	if ok := m.GetInto("missing", &dst); ok || dst != 42 {
		t.Fatalf("GetInto on miss = (%d, %v), want dst untouched and false", dst, ok)
	}

	// Expired entries miss and are removed, exactly like Lookup.
	m.SetExpire("b", 2, 1)
	clock.Advance(2 * time.Second)
	dst = 42
	if ok := m.GetInto("b", &dst); ok || dst != 42 {
		t.Fatalf("GetInto on expired = (%d, %v), want dst untouched and false", dst, ok)
	}
	if got := m.Len(); got != 1 {
		t.Fatalf("Len = %d after expired GetInto, want 1", got)
	}

	allocs := testing.AllocsPerRun(100, func() {
		if ok := m.GetInto("a", &dst); !ok {
			t.Fatal("GetInto on a live entry must hit")
		}
	})
	if allocs != 0 {
		t.Fatalf("GetInto allocated %v times per run, want 0", allocs)
	}

	closed := WithLRU[int](2, false)
	closed.Close()
	dst = 42
	if ok := closed.GetInto("a", &dst); ok || dst != 42 {
		t.Fatalf("GetInto on closed cache = (%d, %v), want dst untouched and false", dst, ok)
	}
}
