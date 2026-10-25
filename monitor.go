package memcache

import "sync"

// MonitorAble is the observable portion of a cache.
type MonitorAble interface {
	Size() uint
	Cap() uint
	Clear()
}

type monitor struct {
	mu        sync.RWMutex
	instances map[MonitorAble]struct{}
}

func (m *monitor) add(cache MonitorAble) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.instances[cache] = struct{}{}
}

func (m *monitor) remove(cache MonitorAble) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.instances, cache)
}

func (m *monitor) snapshot() []MonitorAble {
	m.mu.RLock()
	defer m.mu.RUnlock()
	instances := make([]MonitorAble, 0, len(m.instances))
	for cache := range m.instances {
		instances = append(instances, cache)
	}
	return instances
}

var lives = monitor{instances: make(map[MonitorAble]struct{})}

// CLEAR_ALL clears a snapshot of the currently registered caches. Caches created
// after the snapshot are unaffected. It does not close or unregister caches.
func CLEAR_ALL() {
	for _, cache := range lives.snapshot() {
		cache.Clear()
	}
}

// Lives reports registered caches that have not been closed or finalized.
// Use Close for deterministic removal; GC finalization is only a fallback.
func Lives() int {
	lives.mu.RLock()
	defer lives.mu.RUnlock()
	return len(lives.instances)
}
