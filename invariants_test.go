package memcache

import (
	"reflect"
	"testing"
)

func assertInvariants(t testing.TB, m *Memcache[int]) {
	t.Helper()
	s := m.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.holder) != s.order.Len() || uint(len(s.holder)) > s.capacity {
		t.Fatalf("inconsistent size: map=%d list=%d capacity=%d", len(s.holder), s.order.Len(), s.capacity)
	}
	seen := make(map[string]bool)
	for e := s.order.Front(); e != nil; e = e.Next() {
		n := e.Value.(*node[int])
		if seen[n.key] || s.holder[n.key] != n || n.element != e {
			t.Fatal("list and index disagree")
		}
		seen[n.key] = true
		if n.expiresAt.IsZero() != (n.expiryIndex == -1) {
			t.Fatal("deadline and heap membership disagree")
		}
		if n.expiryIndex >= 0 && (n.expiryIndex >= len(s.expiry) || s.expiry[n.expiryIndex] != n) {
			t.Fatal("incorrect expiry index")
		}
	}
	for i, n := range s.expiry {
		if n.expiryIndex != i || s.holder[n.key] != n || n.expiresAt.IsZero() {
			t.Fatal("stale expiry entry")
		}
		if i > 0 && n.expiresAt.Before(s.expiry[(i-1)/2].expiresAt) {
			t.Fatal("expiry heap is out of order")
		}
	}
	if len(s.expiry) > len(s.holder) {
		t.Fatal("expiry metadata exceeds cache size")
	}
}

func requireOrder(t *testing.T, m *Memcache[int], want ...string) {
	t.Helper()
	assertInvariants(t, m)
	var got []string
	for e := m.state.order.Front(); e != nil; e = e.Next() {
		got = append(got, e.Value.(*node[int]).key)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LRU order = %v, want %v", got, want)
	}
}
