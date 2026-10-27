package memcache

import (
	"strconv"
	"testing"
	"time"
)

// newMultiShardFixture builds a TTL cache with a real worker and a fake
// clock. Tests synchronize through waitArmedDelay (observation only) and
// notifyCleaner (the production wake seam); never by firing the fake timer.
func newMultiShardFixture(t *testing.T, capacity uint, shards int) (*Memcache[int], *fakeClock) {
	t.Helper()
	fake := newFakeClock(modelEpoch)
	m := newMemcacheWithClockShards[int](capacity, true, fake, true, shards, shardSlotTarget, false, true, false)
	t.Cleanup(m.Close)
	return m, fake
}

// shardKeysFor picks keys that deterministically land in distinct shards:
// the shard of a candidate key is computed with the cache's own routing
// (seed included), so the mapping never depends on hash luck.
func shardKeysFor(m *Memcache[int], perShard int) [][]string {
	byShard := make([][]string, len(m.state.shards))
	shardIndex := func(key string) int {
		target := m.state.shardFor(key)
		for i, sh := range m.state.shards {
			if sh == target {
				return i
			}
		}
		return -1
	}
	for i := 0; ; i++ {
		done := true
		for _, keys := range byShard {
			if len(keys) < perShard {
				done = false
			}
		}
		if done {
			return byShard
		}
		key := "k" + strconv.Itoa(i)
		idx := shardIndex(key)
		if len(byShard[idx]) < perShard {
			byShard[idx] = append(byShard[idx], key)
		}
	}
}

// TestMultiShardCleanerArmsAtEarliestRoot: the worker arms one timer at the
// minimum root across shards, reclaims only the shards whose root came due,
// and re-arms at the next remaining root.
func TestMultiShardCleanerArmsAtEarliestRoot(t *testing.T) {
	m, fake := newMultiShardFixture(t, 128, 2)
	keys := shardKeysFor(m, 1)
	m.SetExpire(keys[0][0], 1, 2) // shard 0 root: 2s
	m.SetExpire(keys[1][0], 2, 5) // shard 1 root: 5s
	ch := seamSignals(fake)
	waitArmedDelay(t, ch, fake, 2*time.Second)

	fake.Advance(2500 * time.Millisecond)
	m.state.notifyCleaner()
	waitEventually(t, "purge of the shard-0 root only", func() bool { return m.Len() == 1 })
	requireMissing(t, m, keys[0][0])
	requireValue(t, m, keys[1][0], 2)

	// The timer must now point at the surviving shard-1 root.
	waitArmedDelay(t, ch, fake, 2500*time.Millisecond)
}

// TestMultiShardPurgeBatchSpansShards: one wake with every root due reclaims
// entries across all shards.
func TestMultiShardPurgeBatchSpansShards(t *testing.T) {
	m, fake := newMultiShardFixture(t, 256, 4)
	keys := shardKeysFor(m, 2)
	for _, shardKeys := range keys {
		for _, key := range shardKeys {
			m.SetExpire(key, 1, 3)
		}
	}
	ch := seamSignals(fake)
	waitArmedDelay(t, ch, fake, 3*time.Second)

	fake.Advance(3500 * time.Millisecond)
	m.state.notifyCleaner()
	waitEventually(t, "purge across all shards", func() bool { return m.Len() == 0 })
}

// TestMultiShardAllEmptyStopsTimer: once every heap is empty the worker stops
// its timer and creates no new one while parked.
func TestMultiShardAllEmptyStopsTimer(t *testing.T) {
	m, fake := newMultiShardFixture(t, 256, 4)
	keys := shardKeysFor(m, 1)
	for _, shardKeys := range keys {
		for _, key := range shardKeys {
			m.SetExpire(key, 1, 3)
		}
	}
	ch := seamSignals(fake)
	waitArmedDelay(t, ch, fake, 3*time.Second)
	beforeTimers := fake.newTimerCount()

	fake.Advance(3500 * time.Millisecond)
	m.state.notifyCleaner()
	waitEventually(t, "all shards purged", func() bool { return m.Len() == 0 })
	waitEventually(t, "worker stops its timer", func() bool {
		timer := fake.timer(0)
		return timer != nil && timer.stopCount() >= 1 && !timer.armedNow()
	})

	// A spurious wake on empty heaps must not arm a new timer.
	m.state.notifyCleaner()
	waitEventually(t, "worker parked after spurious wake", func() bool { return m.Len() == 0 })
	if got := fake.newTimerCount(); got != beforeTimers {
		t.Fatalf("newTimerCount = %d after parking, want %d", got, beforeTimers)
	}
}

// TestMultiShardShortenedRootRearms: shortening the TTL of a key that is its
// shard's heap root — while another shard holds the global root — must move
// the armed timer earlier and reclaim the entry on schedule. This is the
// multi-shard twin of TestCleanerShortenedDeadlineRearms.
func TestMultiShardShortenedRootRearms(t *testing.T) {
	m, fake := newMultiShardFixture(t, 128, 2)
	keys := shardKeysFor(m, 2)
	x, z := keys[0][0], keys[0][1]
	y := keys[1][0]
	m.SetExpire(y, 1, 90)  // global root on shard 1
	m.SetExpire(x, 2, 100) // shard 0 root
	m.SetExpire(z, 3, 200)
	ch := seamSignals(fake)
	waitArmedDelay(t, ch, fake, 90*time.Second)

	// Shorten x (its shard's root) from 100s to 10s: the global root moves
	// from 90s to 10s, so the timer must be re-armed earlier.
	m.SetExpire(x, 2, 10)
	waitArmedDelay(t, ch, fake, 10*time.Second)

	fake.Advance(10500 * time.Millisecond)
	m.state.notifyCleaner()
	waitEventually(t, "shortened entry reclaimed", func() bool { return m.Len() == 2 })
	requireMissing(t, m, x)
	requireValue(t, m, y, 1)
	requireValue(t, m, z, 3)
}
