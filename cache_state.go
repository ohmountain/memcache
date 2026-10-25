package memcache

import (
	"container/heap"
	"container/list"
	"sync"
	"time"
)

const cleanerInterval = 100 * time.Millisecond
const maxTTL = time.Duration(1<<63 - 1)

// cacheState is retained by the registry and cleaner, independently of the
// public handle. All mutable fields, including the expiry heap, share one lock.
type cacheState[T CacheAble] struct {
	mu            sync.Mutex
	capacity      uint
	holder        map[string]*node[T]
	order         *list.List
	expiry        expiryHeap[T]
	enableExpired bool
	now           func() time.Time
	closed        bool
	done          chan struct{}
	stopped       chan struct{}
}

func (s *cacheState[T]) set(key string, value T, ttl int64, expiring bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.capacity == 0 {
		return
	}
	expiring = expiring && s.enableExpired
	if expiring && ttl <= 0 {
		// A nonpositive TTL is a delete request for this key only: it never
		// reclaims other expired entries.
		if n, ok := s.holder[key]; ok {
			s.removeLocked(n)
		}
		return
	}
	// One now sample serves the target-key deadline check, the capacity
	// decision, and the new deadline.
	now := s.now()
	var deadline time.Time
	if expiring {
		duration := maxTTL
		if ttl <= int64(maxTTL/time.Second) {
			duration = time.Duration(ttl) * time.Second
		}
		deadline = now.Add(duration)
	}
	n, ok := s.holder[key]
	if ok && !n.expiresAt.IsZero() && !n.expiresAt.After(now) {
		// An expired target is treated as absent: remove only its own record.
		s.removeLocked(n)
		n, ok = nil, false
	}
	if ok {
		n.value = value
		s.order.MoveToFront(n.element)
	} else {
		if uint(len(s.holder)) >= s.capacity {
			s.reclaimSlotLocked(now)
		}
		n = &node[T]{key: key, value: value, expiryIndex: -1}
		n.element = s.order.PushFront(n)
		s.holder[key] = n
	}
	n.expiresAt = deadline
	switch {
	case deadline.IsZero() && n.expiryIndex >= 0:
		heap.Remove(&s.expiry, n.expiryIndex)
	case !deadline.IsZero() && n.expiryIndex >= 0:
		heap.Fix(&s.expiry, n.expiryIndex)
	case !deadline.IsZero():
		heap.Push(&s.expiry, n)
	}
}

// reclaimSlotLocked frees exactly one slot for a new key. When the earliest
// deadline has already passed, that expired entry is reclaimed instead of a
// live one; otherwise the LRU tail is evicted as before. It never scans the
// map or the list for further expired entries, so a single write's reclamation
// work stays bounded.
func (s *cacheState[T]) reclaimSlotLocked(now time.Time) {
	if len(s.expiry) > 0 && !s.expiry[0].expiresAt.After(now) {
		s.removeLocked(s.expiry[0])
		return
	}
	s.removeLocked(s.order.Back().Value.(*node[T]))
}

func (s *cacheState[T]) lookup(key string) (T, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.holder[key]
	if ok && !n.expiresAt.IsZero() && !n.expiresAt.After(s.now()) {
		s.removeLocked(n)
		ok = false
	}
	if !ok {
		var zero T
		return zero, false
	}
	s.order.MoveToFront(n.element)
	return n.value, true
}

func (s *cacheState[T]) delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := s.holder[key]; ok {
		s.removeLocked(n)
	}
}

func (s *cacheState[T]) removeLocked(n *node[T]) {
	if n.expiryIndex >= 0 {
		heap.Remove(&s.expiry, n.expiryIndex)
	}
	s.order.Remove(n.element)
	delete(s.holder, n.key)
	n.element = nil
}

func (s *cacheState[T]) purgeExpiredLocked(now time.Time) {
	for len(s.expiry) > 0 && !s.expiry[0].expiresAt.After(now) {
		s.removeLocked(s.expiry[0])
	}
}

func (s *cacheState[T]) checkExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(s.now())
}

func (s *cacheState[T]) Size() uint {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(s.now())
	return uint(len(s.holder))
}

func (s *cacheState[T]) Cap() uint { return s.capacity }

func (s *cacheState[T]) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.clearLocked()
	}
}

func (s *cacheState[T]) clearLocked() {
	s.holder = make(map[string]*node[T])
	s.order.Init()
	s.expiry = nil
}

func (s *cacheState[T]) close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.clearLocked()
		close(s.done)
	}
	s.mu.Unlock()
	// Never hold the cache lock while acquiring the registry lock.
	lives.remove(s)
	<-s.stopped
}

func (s *cacheState[T]) runCleaner() {
	ticker := time.NewTicker(cleanerInterval)
	defer close(s.stopped)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.checkExpired()
		}
	}
}
