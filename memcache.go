// Package memcache provides a bounded, concurrent LRU cache with optional TTLs.
package memcache

import (
	"container/list"
	"runtime"
	"time"
)

// Memcache is a concurrent LRU cache. Construct it with WithLRU; its zero value
// is not initialized. Do not copy a Memcache. Call Close when it is no longer
// needed to synchronously release its contents, registration, and cleaner.
type Memcache[T CacheAble] struct {
	state *cacheState[T]
}

// WithLRU creates a cache holding at most capacity items. Zero capacity disables
// storage. enableExpired enables TTLs and a background cleaner. No process
// signal handlers are installed.
func WithLRU[T CacheAble](capacity uint, enableExpired bool) *Memcache[T] {
	return newMemcache[T](capacity, enableExpired, time.Now, true)
}

// The clock and cleaner switch allow deterministic TTL tests without sleeps.
func newMemcache[T CacheAble](capacity uint, enableExpired bool, now func() time.Time, startCleaner bool) *Memcache[T] {
	return newMemcacheWithClock[T](capacity, enableExpired, systemClock{now: now}, startCleaner)
}

// newMemcacheWithClock is the shared constructor: the injected clock supplies
// both deadline reads (state.now) and cleaner scheduling (state.clock).
func newMemcacheWithClock[T CacheAble](capacity uint, enableExpired bool, clock cacheClock, startCleaner bool) *Memcache[T] {
	s := &cacheState[T]{
		capacity:      capacity,
		holder:        make(map[string]*node[T]),
		order:         list.New(),
		enableExpired: enableExpired,
		clock:         clock,
		now:           clock.Now,
		done:          make(chan struct{}),
		stopped:       make(chan struct{}),
	}
	m := &Memcache[T]{state: s}
	lives.add(s)
	if enableExpired && capacity > 0 && startCleaner {
		go s.runCleaner()
	} else {
		close(s.stopped)
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
	m.state.set(key, value, ttl, true)
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

// Delete removes a key and its expiration record, if present.
func (m *Memcache[T]) Delete(key string) {
	m.state.delete(key)
	runtime.KeepAlive(m)
}

// Size returns the number of live entries, purging any expired entries first.
func (m *Memcache[T]) Size() uint {
	size := m.state.Size()
	runtime.KeepAlive(m)
	return size
}

// Cap returns the configured maximum number of entries, even after Close.
func (m *Memcache[T]) Cap() uint {
	capacity := m.state.Cap()
	runtime.KeepAlive(m)
	return capacity
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

func (m *Memcache[T]) checkExpired() {
	m.state.checkExpired()
	runtime.KeepAlive(m)
}
