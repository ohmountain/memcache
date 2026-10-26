package memcache

import (
	"math"
	"testing"
	"time"
)

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func clockCache(t *testing.T, capacity uint) (*Memcache[int], *testClock) {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)}
	m := newMemcache[int](capacity, true, clock.Now, false)
	t.Cleanup(m.Close)
	return m, clock
}

func TestExpirationBoundaryAndSize(t *testing.T) {
	m, clock := clockCache(t, 2)
	m.SetExpire("a", 1, 2)
	clock.Advance(2*time.Second - time.Nanosecond)
	requireValue(t, m, "a", 1)
	clock.Advance(time.Nanosecond)
	requireMissing(t, m, "a")
	if m.Size() != 0 {
		t.Fatal("expired item counted as live")
	}
	assertInvariants(t, m)
}

func TestExpiredItemsDoNotEvictLiveItems(t *testing.T) {
	m, clock := clockCache(t, 2)
	m.Set("live", 1)
	m.SetExpire("expired", 2, 1)
	clock.Advance(time.Second)
	m.Set("new", 3)
	requireValue(t, m, "live", 1)
	requireValue(t, m, "new", 3)
	requireMissing(t, m, "expired")
	assertInvariants(t, m)
}

func TestRenewalAndOverwriteCancelOldExpiry(t *testing.T) {
	for _, operation := range []string{"renew", "set", "delete-reinsert", "clear-reinsert", "evict-reinsert"} {
		t.Run(operation, func(t *testing.T) {
			m, clock := clockCache(t, 1)
			m.SetExpire("k", 1, 1)
			switch operation {
			case "renew":
				m.SetExpire("k", 2, 3600)
			case "set":
				m.Set("k", 2)
			case "delete-reinsert":
				m.Delete("k")
				m.Set("k", 2)
			case "clear-reinsert":
				m.Clear()
				m.Set("k", 2)
			case "evict-reinsert":
				m.Set("other", 0)
				m.Set("k", 2)
			}
			clock.Advance(2 * time.Second)
			m.checkExpired()
			requireValue(t, m, "k", 2)
			assertInvariants(t, m)
		})
	}
}

func TestTTLBounds(t *testing.T) {
	m, clock := clockCache(t, 1)
	for _, ttl := range []int64{0, -1, math.MinInt64} {
		m.Set("k", 1)
		m.SetExpire("k", 2, ttl)
		requireMissing(t, m, "k")
		assertInvariants(t, m)
	}
	m.SetExpire("k", 3, math.MaxInt64)
	clock.Advance(time.Hour)
	requireValue(t, m, "k", 3)
	assertInvariants(t, m)
}

func TestExpiryMetadataIsBounded(t *testing.T) {
	m, _ := clockCache(t, 2)
	for i := 0; i < 10000; i++ {
		m.SetExpire("k", i, int64(i+1))
	}
	if m.state.expiryLen() != 1 {
		t.Fatalf("renewal left %d expiry records", m.state.expiryLen())
	}
	m.SetExpire("other", 0, 1)
	m.SetExpire("third", 0, 2)
	assertInvariants(t, m)
	m.Delete("other")
	m.Set("third", 1)
	if m.state.expiryLen() != 0 {
		t.Fatal("delete/overwrite retained expiry metadata")
	}
	m.SetExpire("third", 2, 1)
	m.Clear()
	if m.state.expiryLen() != 0 {
		t.Fatal("Clear retained expiry metadata")
	}
	assertInvariants(t, m)
}

func TestShorteningAndCleanerRemovesDueItems(t *testing.T) {
	m, clock := clockCache(t, 3)
	m.SetExpire("a", 1, 3600)
	m.SetExpire("b", 2, 20)
	m.SetExpire("c", 3, 30)
	m.SetExpire("a", 4, 10)
	clock.Advance(10 * time.Second)
	m.checkExpired()
	if m.state.hasKey("a") {
		t.Fatal("cleaner retained due entry")
	}
	requireValue(t, m, "b", 2)
	requireValue(t, m, "c", 3)
	clock.Advance(20 * time.Second)
	if m.Size() != 0 {
		t.Fatal("Size did not purge expired entries")
	}
	assertInvariants(t, m)
}
