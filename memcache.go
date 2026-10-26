// Package memcache provides a bounded, concurrent LRU cache with optional TTLs.
package memcache

import (
	"runtime"
	"time"
)

// Memcache is a concurrent LRU cache. Construct it with WithLRU or WithOptions;
// its zero value is not initialized. Do not copy a Memcache. Call Close when it
// is no longer needed to synchronously release its contents, registration, and
// cleaner.
type Memcache[T CacheAble] struct {
	state *cacheState[T]
}

// Options configures WithOptions. Capacity is the maximum number of entries
// (zero stores nothing). EnableExpired enables TTLs and background cleanup.
// Shards zero or one keeps one exact global LRU; a value above one opts into
// approximate per-shard LRU for multi-core throughput. The shard count is
// rounded down to a power of two and capped at Capacity/64 so no shard is
// sized below about 64 slots; Capacity is split evenly across the result.
// No process signal handlers are installed.
type Options struct {
	Capacity      uint
	EnableExpired bool
	Shards        int
	// Preallocate reserves map buckets for each shard up front. It trades
	// memory for fewer growth stalls; leave it off for many small or mostly
	// empty caches.
	Preallocate bool
	// DisableStats turns off the hit/miss/set/delete/eviction/expiration
	// counters. Stats then reports only Capacity and Entries. Each counter
	// update is a store to a shard cache line that a read-only path such as
	// Peek otherwise never dirties, so disabling them raises multicore read
	// throughput; do it when the counters are not exported.
	DisableStats bool
}

// Stats is a point-in-time snapshot of cache counters. Entries is physical
// (Len semantics) and does not purge expired entries. Hits and Misses count
// Lookup and Get; Peek traffic is reported separately by Peeks and PeekHits.
// With Options.DisableStats every field except Capacity and Entries stays zero.
type Stats struct {
	Capacity    uint
	Entries     uint
	Hits        uint64
	Misses      uint64
	Sets        uint64
	Deletes     uint64
	Evictions   uint64
	Expirations uint64
	Peeks       uint64
	PeekHits    uint64
}

// cleanerMode selects how the background worker starts. It replaces the old
// adjacent (startCleaner, autoStart) booleans, whose false/true combinations
// were easy to swap at call sites.
type cleanerMode uint8

const (
	cleanerNone cleanerMode = iota
	cleanerEager
	cleanerLazy
)

// WithLRU creates a cache holding at most capacity items. Zero capacity disables
// storage. enableExpired enables TTLs and a background cleaner. The default is
// one exact global LRU; use WithOptions to opt into sharding.
func WithLRU[T CacheAble](capacity uint, enableExpired bool) *Memcache[T] {
	return WithOptions[T](Options{Capacity: capacity, EnableExpired: enableExpired})
}

// WithOptions creates a cache from an Options value. The background cleaner is
// started on the first TTL write, so a TTL-enabled cache that never stores a
// TTL holds no worker goroutine.
func WithOptions[T CacheAble](opts Options) *Memcache[T] {
	return newMemcacheConfigured[T](opts, productionClock, cleanerLazy)
}

// The clock and cleaner switch allow deterministic TTL tests without sleeps.
// An injected clock carries no monotonic reading, so systemClock keeps the
// portable Now().Sub(start) path for it.
func newMemcache[T CacheAble](capacity uint, enableExpired bool, now func() time.Time, startCleaner bool) *Memcache[T] {
	mode := cleanerNone
	if startCleaner {
		mode = cleanerEager
	}
	return newMemcacheConfigured[T](Options{Capacity: capacity, EnableExpired: enableExpired}, systemClock{now: now}, mode)
}

func newMemcacheConfigured[T CacheAble](opts Options, clock cacheClock, mode cleanerMode) *Memcache[T] {
	m := newMemcacheWithClockShards[T](opts.Capacity, opts.EnableExpired, clock, mode == cleanerEager, opts.Shards, opts.Preallocate, !opts.DisableStats)
	m.state.autoStart = mode == cleanerLazy
	return m
}

// newMemcacheWithClock is the single-shard test seam: the injected clock
// supplies both deadline reads and cleaner scheduling.
func newMemcacheWithClock[T CacheAble](capacity uint, enableExpired bool, clock cacheClock, startCleaner bool) *Memcache[T] {
	return newMemcacheWithClockShards[T](capacity, enableExpired, clock, startCleaner, 1, false, true)
}

func newMemcacheWithClockShards[T CacheAble](capacity uint, enableExpired bool, clock cacheClock, startCleaner bool, shards int, preallocate bool, collectStats bool) *Memcache[T] {
	s := newCacheState[T](capacity, enableExpired, clock, shards, preallocate, collectStats)
	m := &Memcache[T]{state: s}
	lives.add(s)

	if enableExpired && capacity > 0 && startCleaner {
		s.startWorkerNow()
	}
	// Registry entries and the cleaner refer only to state, so an abandoned
	// public handle can be collected. Finalization is a best-effort fallback;
	// callers must use Close for prompt cleanup.
	runtime.SetFinalizer(m, func(cache *Memcache[T]) { cache.state.close() })
	return m
}

// Set inserts or updates a value and removes any previous TTL for the key.
func (m *Memcache[T]) Set(key string, value T) {
	m.state.set(key, value, 0, false)
	runtime.KeepAlive(m)
}

// SetExpire inserts or updates a value with a TTL in seconds. On a cache with
// expiration disabled it behaves like Set. Otherwise a nonpositive TTL deletes
// the key immediately. TTLs exceeding time.Duration's range are clamped.
func (m *Memcache[T]) SetExpire(key string, value T, ttl int64) {
	if ttl <= 0 {
		m.state.set(key, value, 0, true)
		runtime.KeepAlive(m)
		return
	}
	if ttl > maxTTLSeconds {
		ttl = maxTTLSeconds
	}
	m.state.set(key, value, ttl*int64(time.Second), true)
	runtime.KeepAlive(m)
}

// SetExpireDuration is SetExpire with sub-second precision. A nonpositive
// duration deletes the key when TTLs are enabled; excessive durations are
// clamped to the maximum time.Duration.
func (m *Memcache[T]) SetExpireDuration(key string, value T, ttl time.Duration) {
	m.state.set(key, value, int64(ttl), true)
	runtime.KeepAlive(m)
}

// Get returns a pointer to a shallow copy of the value, or nil on a miss.
// Modifying the returned scalar does not modify the cache. Maps, slices,
// pointers, and other referenced payloads are shared; synchronize their use
// separately. Use Lookup to avoid allocating the returned pointer.
func (m *Memcache[T]) Get(key string) *T {
	value, ok := m.Lookup(key)
	if !ok {
		return nil
	}
	return &value
}

// Lookup returns a shallow value copy and whether the key is present and live.
// Successful reads refresh the LRU order.
func (m *Memcache[T]) Lookup(key string) (T, bool) {
	value, ok := m.state.lookup(key)
	// Prevent finalization from closing state while an operation is in flight.
	runtime.KeepAlive(m)
	return value, ok
}

// Peek returns a shallow value copy without refreshing LRU order. It is the
// read path for multi-core workloads that can accept approximate recency:
// expired entries still miss, but are left for the cleaner. Peeks are counted
// separately in Stats.
func (m *Memcache[T]) Peek(key string) (T, bool) {
	value, ok := m.state.peek(key)
	runtime.KeepAlive(m)
	return value, ok
}

// Delete removes a key and its expiration record, if present.
func (m *Memcache[T]) Delete(key string) {
	m.state.delete(key)
	runtime.KeepAlive(m)
}

// Size returns the number of live entries, purging expired entries first. It
// purges in bounded chunks across shards so a large sweep does not hold one
// lock for its entire duration. Use Len for an O(1)-per-shard physical count.
func (m *Memcache[T]) Size() uint {
	size := m.state.Size()
	runtime.KeepAlive(m)
	return size
}

// Len returns the physical entry count without purging expired entries. It
// never reads the clock and is suitable for monitoring.
func (m *Memcache[T]) Len() uint {
	n := m.state.Len()
	runtime.KeepAlive(m)
	return n
}

// Cap returns the configured maximum number of entries, even after Close.
func (m *Memcache[T]) Cap() uint {
	capacity := m.state.Cap()
	runtime.KeepAlive(m)
	return capacity
}

// Stats returns a point-in-time snapshot of this cache's counters.
func (m *Memcache[T]) Stats() Stats {
	stats := m.state.stats()
	runtime.KeepAlive(m)
	return stats
}

// Clear removes all entries and expiration records, keeping the cache usable.
func (m *Memcache[T]) Clear() {
	m.state.Clear()
	runtime.KeepAlive(m)
}

// Close stops and joins the cleaner, unregisters the cache, and releases its
// contents. It is safe to call repeatedly and concurrently. After Close,
// writes are ignored, reads miss, and Size is zero.
func (m *Memcache[T]) Close() {
	m.state.close()
	runtime.SetFinalizer(m, nil)
	runtime.KeepAlive(m)
}
