package memcache

import (
	"sync"
	"sync/atomic"
)

// MonitorAble is the observable portion of a cache.
type MonitorAble interface {
	Size() uint
	Cap() uint
	Clear()
}

// monitor is a sync.Map-backed registry with an atomic count. Create/close do
// not serialize on one mutex, which matters when an application churns many
// short-lived caches.
//
// count must stay the first field. 64-bit atomic operations require 8-byte
// alignment on 386, arm, and 32-bit mips, and only the first word of a global
// or of an allocated struct is guaranteed to be aligned. With sync.Map first,
// count lands at offset 20 on 386 (sync.Map is 20 bytes there) and every
// registration panics with "unaligned 64-bit atomic operation".
type monitor struct {
	count     int64
	instances sync.Map
}

func (m *monitor) add(cache MonitorAble) {
	if _, loaded := m.instances.LoadOrStore(cache, struct{}{}); !loaded {
		atomic.AddInt64(&m.count, 1)
	}
}

func (m *monitor) remove(cache MonitorAble) {
	if _, loaded := m.instances.LoadAndDelete(cache); loaded {
		atomic.AddInt64(&m.count, -1)
	}
}

func (m *monitor) snapshot() []MonitorAble {
	n := atomic.LoadInt64(&m.count)
	if n < 0 {
		n = 0
	}
	instances := make([]MonitorAble, 0, int(n))
	m.instances.Range(func(key, _ any) bool {
		instances = append(instances, key.(MonitorAble))
		return true
	})
	return instances
}

var lives = monitor{}

// CLEAR_ALL clears the caches registered when Range visits them. Registration
// is concurrent, so a cache created during the call may or may not be included;
// call CLEAR_ALL after the cache population is stable when that matters. It does
// not close or unregister caches.
func CLEAR_ALL() {
	for _, cache := range lives.snapshot() {
		cache.Clear()
	}
}

// Lives reports registered caches that have not been closed or finalized.
// Use Close for deterministic removal; GC finalization is only a fallback.
func Lives() int {
	return int(atomic.LoadInt64(&lives.count))
}
