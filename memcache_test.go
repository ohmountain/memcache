package memcache

import (
	"fmt"
	"testing"
)

func testCache(t *testing.T, capacity uint, expiry bool) *Memcache[int] {
	t.Helper()
	m := WithLRU[int](capacity, expiry)
	t.Cleanup(m.Close)
	return m
}

func requireValue(t *testing.T, m *Memcache[int], key string, want int) {
	t.Helper()
	got := m.Get(key)
	if got == nil || *got != want {
		t.Fatalf("Get(%q) = %v, want %d", key, got, want)
	}
}

func requireMissing(t *testing.T, m *Memcache[int], key string) {
	t.Helper()
	if got := m.Get(key); got != nil {
		t.Fatalf("Get(%q) = %d, want miss", key, *got)
	}
}

func TestCapacityAndEviction(t *testing.T) {
	for _, capacity := range []uint{0, 1, 2, 16} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			m := testCache(t, capacity, false)
			m.Set("a", 1)
			if capacity == 0 {
				requireMissing(t, m, "a")
				if m.Size() != 0 {
					t.Fatal("zero-capacity cache stores entries")
				}
				return
			}
			if m.Cap() != capacity {
				t.Fatal("capacity changed")
			}
		})
	}
	m := testCache(t, 2, false)
	m.Set("a", 1)
	m.Set("b", 2)
	m.Set("c", 3)
	requireMissing(t, m, "a")
	requireValue(t, m, "b", 2)
	requireValue(t, m, "c", 3)
	if m.Size() != 2 {
		t.Fatalf("size = %d, want 2", m.Size())
	}
}

func TestAccessAndUpdateRefreshRecency(t *testing.T) {
	for _, update := range []bool{false, true} {
		m := testCache(t, 3, false)
		m.Set("a", 1)
		m.Set("b", 2)
		m.Set("c", 3)
		if update {
			m.Set("a", 10)
		} else {
			requireValue(t, m, "a", 1)
		}
		m.Set("d", 4)
		requireMissing(t, m, "b")
		if update {
			requireValue(t, m, "a", 10)
		} else {
			requireValue(t, m, "a", 1)
		}
	}
}

func TestDeletePositionsAndReuse(t *testing.T) {
	for _, key := range []string{"a", "b", "c", "missing"} {
		t.Run(key, func(t *testing.T) {
			m := testCache(t, 3, false)
			m.Set("a", 1)
			m.Set("b", 2)
			m.Set("c", 3)
			m.Delete(key)
			requireMissing(t, m, key)
			want := uint(2)
			if key == "missing" {
				want = 3
			}
			if m.Size() != want {
				t.Fatalf("size = %d, want %d", m.Size(), want)
			}
			m.Delete(key)
			m.Clear()
			if m.Size() != 0 {
				t.Fatal("clear did not empty cache")
			}
			m.Set("new", 4)
			requireValue(t, m, "new", 4)
		})
	}
}

func TestGetReturnsSnapshot(t *testing.T) {
	m := testCache(t, 1, false)
	m.Set("k", 1)
	first := m.Get("k")
	m.Set("k", 2)
	if *first != 1 {
		t.Fatalf("previous Get changed to %d after Set", *first)
	}
	*first = 99
	requireValue(t, m, "k", 2)
}

func TestSetExpireWithoutCleaner(t *testing.T) {
	m := testCache(t, 1, false)
	m.SetExpire("k", 1, 60)
	requireValue(t, m, "k", 1)
}

func TestLookupZeroAndNilValues(t *testing.T) {
	m := testCache(t, 2, false)
	m.Set("zero", 0)
	if got, ok := m.Lookup("zero"); !ok || got != 0 {
		t.Fatal("zero value is not a miss")
	}
	if got, ok := m.Lookup("missing"); ok || got != 0 {
		t.Fatal("missing key must return zero, false")
	}
	pointers := WithLRU[*int](1, false)
	defer pointers.Close()
	pointers.Set("nil", nil)
	if got := pointers.Get("nil"); got == nil || *got != nil {
		t.Fatal("stored nil pointer must be distinguishable from miss")
	}
}

func TestListInvariantsAfterMoves(t *testing.T) {
	m := testCache(t, 3, false)
	m.Set("a", 1)
	m.Set("b", 2)
	m.Set("c", 3)
	requireValue(t, m, "b", 2)
	requireValue(t, m, "a", 1)
	requireOrder(t, m, "a", "b", "c")
	m.Set("c", 30)
	requireOrder(t, m, "c", "a", "b")
	m.Delete("c")
	requireOrder(t, m, "a", "b")
	m.Delete("b")
	requireOrder(t, m, "a")
	m.Delete("a")
	requireOrder(t, m)
}
