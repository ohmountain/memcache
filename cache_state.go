package memcache

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxTTL               = time.Duration(1<<63 - 1)
	maxInt64             = int64(maxTTL)
	maxTTLSeconds        = int64(maxTTL / time.Second)
	sizePurgeBatch       = 1024
	shardSlotTarget      = 64
	maxFreeNodesPerShard = 1024
	maxIdleHeapCap       = 1024
	cleanupBatchDivisor  = 64
	maxCleanupBatchSize  = 4096
)

// shard owns one map/list/heap set and the lock that protects them. Padding
// keeps one shard's lock and hot fields off its neighbour's cache lines; the
// allocator does not guarantee 64-byte alignment, so this is best effort.
type shard[T CacheAble] struct {
	mu         sync.Mutex
	cap        int
	m          map[string]*node[T]
	head, tail *node[T]
	heap       expiryHeap[T]
	free       []*node[T]
	size       int // mirrors len(m); cached because the full-shard check runs on every insert

	hits        uint64
	misses      uint64
	sets        uint64
	deletes     uint64
	evictions   uint64
	expirations uint64
	peeks       uint64
	peekHits    uint64

	// The 160-byte body plus this 48-byte pad makes a 208-byte object. The
	// next shard's lock then starts in a line occupied only by this shard's
	// padding rather than by its hot fields; the allocator does not guarantee
	// 64-byte alignment, so this separation is best effort only.
	_ [48]byte
}

// cacheState is retained by the registry and cleaner, independently of the
// public handle. Mutable entry state is partitioned across shards; lifecycle
// and scheduler state stay cache-wide.
type cacheState[T CacheAble] struct {
	shards        []*shard[T]
	mask          uint64
	seed          uint64
	capacity      uint
	enableExpired bool
	clock         cacheClock
	start         time.Time

	// wake is a one-slot invalidation signal for the cleaner. It carries no
	// deadline: a notification only means "re-read the real heap roots".
	wake chan struct{}

	done    chan struct{}
	stopped chan struct{}

	lifeMu        sync.Mutex
	autoStart     bool
	started       bool
	workerStarted uint32
	stopOnce      sync.Once
	closed        uint32
}

func shardCount(capacity uint, requested int) int {
	if capacity == 0 || requested <= 1 {
		return 1
	}
	maxN := int(capacity) / shardSlotTarget
	if maxN < 1 {
		maxN = 1
	}
	n := 1
	for n*2 <= requested && n*2 <= maxN {
		n *= 2
	}
	return n
}

// hashKey mixes the per-cache seed so an attacker cannot precompute keys that
// all land in one shard. The unsharded path never calls it, so every real call
// uses the nonzero seed installed by the constructor.
func hashKey(s string, seed uint64) uint64 {
	h := uint64(14695981039346656037) ^ seed
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h += seed
	return h
}

func newCacheState[T CacheAble](capacity uint, enableExpired bool, clock cacheClock, shards int, preallocate bool) *cacheState[T] {
	n := shardCount(capacity, shards)
	s := &cacheState[T]{
		mask:          uint64(n - 1),
		capacity:      capacity,
		enableExpired: enableExpired,
		clock:         clock,
		done:          make(chan struct{}),
		stopped:       make(chan struct{}),
	}
	if n > 1 {
		// Seed only sharded caches; the unsharded path never hashes. This
		// wall-clock read is not the TTL clock, so sharded tests get a
		// nondeterministic key-to-shard mapping by design.
		s.seed = uint64(time.Now().UnixNano()) ^ uint64(capacity)<<17
	}
	if enableExpired {
		s.start = clock.Now()
	}
	if enableExpired && capacity > 0 {
		s.wake = make(chan struct{}, 1)
	}

	base := int(capacity) / n
	rem := int(capacity) % n
	hint := 0
	if preallocate {
		hint = base
	}
	s.shards = make([]*shard[T], n)
	for i := range s.shards {
		shardCap := base
		if i < rem {
			shardCap++
		}
		s.shards[i] = &shard[T]{cap: shardCap, m: make(map[string]*node[T], hint)}
	}
	return s
}

func (s *cacheState[T]) isClosed() bool { return atomic.LoadUint32(&s.closed) != 0 }

func (s *cacheState[T]) nowNanos() int64 { return s.nanosAt(s.clock.Now()) }

func (s *cacheState[T]) nanosAt(t time.Time) int64 { return int64(t.Sub(s.start)) }

func (s *cacheState[T]) shardFor(key string) *shard[T] {
	if len(s.shards) == 1 {
		return s.shards[0]
	}
	return s.shards[hashKey(key, s.seed)&s.mask]
}

// notifyCleaner wakes the worker after a heap mutation. The one-slot channel
// coalesces bursts, and a dropped send is harmless because the worker re-reads
// the current heap roots after every wake.
func (s *cacheState[T]) notifyCleaner() {
	if s.wake == nil {
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// ensureCleaner starts the one worker on the first real TTL write. Public
// caches that never set a TTL therefore hold no goroutine at all.
func (s *cacheState[T]) ensureCleaner() {
	if !s.autoStart || s.wake == nil || s.isClosed() {
		return
	}
	// The common case is a worker that has already been started; avoid the
	// cache-wide lifeMu on every TTL write.
	if atomic.LoadUint32(&s.workerStarted) != 0 {
		return
	}
	s.startWorkerNow()
}

// startWorkerNow starts the worker regardless of autoStart. Constructors use it
// for the eager test seam; public caches use ensureCleaner on the first TTL.
func (s *cacheState[T]) startWorkerNow() {
	if s.wake == nil || s.isClosed() {
		return
	}
	s.lifeMu.Lock()
	if !s.started && !s.isClosed() {
		s.started = true
		atomic.StoreUint32(&s.workerStarted, 1)
		go s.runCleaner()
	}
	s.lifeMu.Unlock()
}

func (s *cacheState[T]) closeStopped() {
	s.stopOnce.Do(func() { close(s.stopped) })
}

func (s *shard[T]) pushFront(n *node[T]) {
	n.prev = nil
	n.next = s.head
	if s.head != nil {
		s.head.prev = n
	} else {
		s.tail = n
	}
	s.head = n
}

// detach removes n from the list without clearing its links; callers that
// reuse n immediately, such as moveFront, avoid two redundant stores.
func (s *shard[T]) detach(n *node[T]) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		s.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		s.tail = n.prev
	}
}

func (s *shard[T]) unlink(n *node[T]) {
	s.detach(n)
	n.prev, n.next = nil, nil
}

func (s *shard[T]) moveFront(n *node[T]) {
	if s.head == n {
		return
	}
	s.detach(n)
	n.prev = nil
	n.next = s.head
	if s.head != nil {
		s.head.prev = n
	} else {
		s.tail = n
	}
	s.head = n
}

func (s *shard[T]) acquire(key string, value T) *node[T] {
	if last := len(s.free) - 1; last >= 0 {
		n := s.free[last]
		s.free = s.free[:last]
		n.key, n.value = key, value
		n.prev, n.next = nil, nil
		n.deadline = 0
		n.expiryIndex = -1
		return n
	}
	return &node[T]{key: key, value: value, expiryIndex: -1}
}

func (s *shard[T]) release(n *node[T]) {
	var zero T
	n.key = ""
	n.value = zero
	n.prev, n.next = nil, nil
	n.deadline = 0
	n.expiryIndex = -1
	if len(s.free) >= maxFreeNodesPerShard {
		return
	}
	s.free = append(s.free, n)
}

// removeLocked removes one entry from the list, index, and expiry heap.
func (s *shard[T]) removeLocked(n *node[T]) {
	if n.expiryIndex >= 0 {
		s.heap.removeAt(n.expiryIndex)
	}
	s.unlink(n)
	delete(s.m, n.key)
	s.size--
	s.release(n)
	if len(s.heap) == 0 && cap(s.heap) > maxIdleHeapCap {
		s.heap = nil
	}
}

func (s *shard[T]) purgeExpiredLocked(now int64, limit int) int {
	removed := 0
	for removed < limit && len(s.heap) > 0 && s.heap[0].deadline <= now {
		s.expirations++
		s.removeLocked(s.heap[0])
		removed++
	}
	return removed
}

// reclaimLocked frees exactly one slot: a due heap root when one exists,
// otherwise the LRU tail. It never scans for further expired entries, so one
// write's reclamation work stays bounded.
func (s *shard[T]) reclaimLocked(now int64) {
	if s.purgeExpiredLocked(now, 1) > 0 {
		return
	}
	if s.tail == nil {
		return
	}
	s.evictions++
	s.removeLocked(s.tail)
}

func (s *shard[T]) clearLocked() {
	s.m = make(map[string]*node[T])
	s.head, s.tail = nil, nil
	s.heap = nil
	s.free = nil
	s.size = 0
}

// rootDeadline is the earliest deadline in this shard (zero when none). The
// cleaner only needs a wake when that value changes; recency updates and
// renewals that do not change the minimum leave it untouched.
func (s *shard[T]) rootDeadline() int64 {
	if len(s.heap) == 0 {
		return 0
	}
	return s.heap[0].deadline
}

func (s *cacheState[T]) set(key string, value T, ttlNanos int64, expiring bool) {
	if s.capacity == 0 || s.isClosed() {
		return
	}
	expiring = expiring && s.enableExpired
	if expiring && ttlNanos <= 0 {
		// A nonpositive TTL is a delete request for this key only: it never
		// reclaims other expired entries.
		s.delete(key)
		return
	}
	if expiring {
		s.ensureCleaner()
	}

	sh := s.shardFor(key)
	sh.mu.Lock()
	if s.isClosed() {
		sh.mu.Unlock()
		return
	}
	n, ok := sh.m[key]
	rootBefore := sh.rootDeadline()

	// The clock is read only when a deadline participates in this write: an
	// expiring target, a new TTL, or a full shard whose expiry heap can have a
	// due root. A cache holding only non-expiring entries never reads it.
	needNow := expiring
	if ok && n.deadline != 0 {
		needNow = true
	}
	if !ok && sh.size >= sh.cap && len(sh.heap) > 0 {
		needNow = true
	}
	var now int64
	if needNow {
		now = s.nowNanos()
	}

	var deadline int64
	if expiring {
		deadline = now + ttlNanos
		if deadline < now {
			deadline = maxInt64
		}
	}

	if ok && n.deadline != 0 && n.deadline <= now {
		// An expired target is treated as absent: remove only its own record.
		sh.expirations++
		sh.removeLocked(n)
		n, ok = nil, false
	}

	if ok {
		n.value = value
		sh.moveFront(n)
	} else {
		if sh.size >= sh.cap {
			sh.reclaimLocked(now)
		}
		n = sh.acquire(key, value)
		sh.pushFront(n)
		sh.m[key] = n
		sh.size++
	}

	n.deadline = deadline
	switch {
	case deadline == 0 && n.expiryIndex >= 0:
		sh.heap.removeAt(n.expiryIndex)
	case deadline != 0 && n.expiryIndex >= 0:
		sh.heap.fix(n.expiryIndex)
	case deadline != 0:
		sh.heap.push(n)
	}
	sh.sets++
	changed := sh.rootDeadline() != rootBefore
	sh.mu.Unlock()
	if changed {
		s.notifyCleaner()
	}
}

func (s *cacheState[T]) lookup(key string) (T, bool) {
	if s.capacity == 0 || s.isClosed() {
		var zero T
		return zero, false
	}
	sh := s.shardFor(key)
	sh.mu.Lock()
	if s.isClosed() {
		sh.mu.Unlock()
		var zero T
		return zero, false
	}
	n, ok := sh.m[key]
	if !ok {
		sh.misses++
		sh.mu.Unlock()
		var zero T
		return zero, false
	}
	if n.deadline != 0 && n.deadline <= s.nowNanos() {
		rootBefore := sh.rootDeadline()
		sh.expirations++
		sh.removeLocked(n)
		changed := sh.rootDeadline() != rootBefore
		sh.misses++
		sh.mu.Unlock()
		if changed {
			s.notifyCleaner()
		}
		var zero T
		return zero, false
	}
	sh.moveFront(n)
	sh.hits++
	value := n.value
	sh.mu.Unlock()
	return value, true
}

// peek returns a value without refreshing recency. Expired entries are
// reported as misses but left in place for the cleaner; no heap or list state
// is touched.
func (s *cacheState[T]) peek(key string) (T, bool) {
	if s.capacity == 0 || s.isClosed() {
		var zero T
		return zero, false
	}
	sh := s.shardFor(key)
	sh.mu.Lock()
	if s.isClosed() {
		sh.mu.Unlock()
		var zero T
		return zero, false
	}
	sh.peeks++
	n, ok := sh.m[key]
	if !ok {
		sh.mu.Unlock()
		var zero T
		return zero, false
	}
	if n.deadline != 0 && n.deadline <= s.nowNanos() {
		sh.mu.Unlock()
		var zero T
		return zero, false
	}
	sh.peekHits++
	value := n.value
	sh.mu.Unlock()
	return value, true
}

func (s *cacheState[T]) delete(key string) {
	if s.capacity == 0 || s.isClosed() {
		return
	}
	sh := s.shardFor(key)
	sh.mu.Lock()
	if s.isClosed() {
		sh.mu.Unlock()
		return
	}
	changed := false
	if n, ok := sh.m[key]; ok {
		rootBefore := sh.rootDeadline()
		sh.deletes++
		sh.removeLocked(n)
		changed = sh.rootDeadline() != rootBefore
	}
	sh.mu.Unlock()
	if changed {
		s.notifyCleaner()
	}
}

// Size purges every due entry in bounded chunks so a large monitoring call
// never holds one shard lock for the whole sweep. The returned count is the sum
// of each shard's live size after its last chunk, not one cross-shard instant.
// A writer that sampled the clock before this call can insert an entry whose
// deadline is already due at the frozen now, extending that shard's loop; each
// pass still removes at most sizePurgeBatch entries.
func (s *cacheState[T]) Size() uint {
	if s.capacity == 0 || s.isClosed() {
		return 0
	}
	if !s.enableExpired {
		return s.Len()
	}
	now := s.nowNanos()
	total := 0
	purged := false
	for _, sh := range s.shards {
		for {
			sh.mu.Lock()
			if s.isClosed() {
				sh.mu.Unlock()
				return 0
			}
			removed := sh.purgeExpiredLocked(now, sizePurgeBatch)
			if removed > 0 {
				purged = true
			}
			size := sh.size
			sh.mu.Unlock()
			if removed < sizePurgeBatch {
				total += size
				break
			}
		}
	}
	if purged {
		s.notifyCleaner()
	}
	return uint(total)
}

// Len returns the physical entry count without purging expired entries.
func (s *cacheState[T]) Len() uint {
	if s.capacity == 0 || s.isClosed() {
		return 0
	}
	total := 0
	for _, sh := range s.shards {
		sh.mu.Lock()
		total += sh.size
		sh.mu.Unlock()
	}
	return uint(total)
}

func (s *cacheState[T]) Cap() uint { return s.capacity }

func (s *cacheState[T]) Clear() {
	if s.capacity == 0 || s.isClosed() {
		return
	}
	s.clearShards()
	s.notifyCleaner()
}

func (s *cacheState[T]) clearShards() {
	for _, sh := range s.shards {
		sh.mu.Lock()
		sh.clearLocked()
		sh.mu.Unlock()
	}
}

func (s *cacheState[T]) close() {
	s.lifeMu.Lock()
	if s.isClosed() {
		s.lifeMu.Unlock()
		<-s.stopped
		return
	}
	atomic.StoreUint32(&s.closed, 1)
	close(s.done)
	if !s.started {
		s.closeStopped()
	}
	s.lifeMu.Unlock()

	s.clearShards()
	// Never hold a shard lock while acquiring the registry.
	lives.remove(s)
	<-s.stopped
}

func (s *cacheState[T]) purgeDueBatch(now int64, limit int) int {
	removed := 0
	for _, sh := range s.shards {
		if removed >= limit || s.isClosed() {
			break
		}
		sh.mu.Lock()
		removed += sh.purgeExpiredLocked(now, limit-removed)
		sh.mu.Unlock()
	}
	return removed
}

// deadlineSnapshot returns the earliest heap deadline and the total number of
// pending expiry records in one pass, so the cleaner does not need a separate
// shard sweep just to size its next batch.
func (s *cacheState[T]) deadlineSnapshot() (int64, int) {
	var earliest int64
	total := 0
	for _, sh := range s.shards {
		sh.mu.Lock()
		total += len(sh.heap)
		if d := sh.rootDeadline(); d != 0 && (earliest == 0 || d < earliest) {
			earliest = d
		}
		sh.mu.Unlock()
	}
	return earliest, total
}

// cleanupBatchSize adapts one reclamation batch to the current heap total.
// It is pure so the cleaner can reuse the count from its deadline snapshot
// instead of re-locking every shard once per batch.
func cleanupBatchSize(total int) int {
	if total <= defaultCleanupBatchSize {
		return defaultCleanupBatchSize
	}
	batch := total / cleanupBatchDivisor
	if batch < defaultCleanupBatchSize {
		batch = defaultCleanupBatchSize
	}
	if batch > maxCleanupBatchSize {
		batch = maxCleanupBatchSize
	}
	return batch
}

func (s *cacheState[T]) stats() Stats {
	stats := Stats{Capacity: s.capacity}
	for _, sh := range s.shards {
		sh.mu.Lock()
		stats.Entries += uint(sh.size)
		stats.Hits += sh.hits
		stats.Misses += sh.misses
		stats.Sets += sh.sets
		stats.Deletes += sh.deletes
		stats.Evictions += sh.evictions
		stats.Expirations += sh.expirations
		stats.Peeks += sh.peeks
		stats.PeekHits += sh.peekHits
		sh.mu.Unlock()
	}
	return stats
}
