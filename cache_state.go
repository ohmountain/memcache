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
	// Lazy-heap compaction bounds; see maybeCompactLocked.
	maxHeapRecordsPerEntry = 2
	minHeapCompactLen      = 64
)

// shard owns one map/list/heap set and the lock that protects them. The
// 104-byte body holds the mutex, the map, the list/heap/free/size fields and
// the approximate-read clock sample in the first cache line and a half, and
// the eight counters from offset 104 to 167.
type shard[T CacheAble] struct {
	mu         sync.Mutex
	cap        int
	m          map[string]*node[T]
	head, tail *node[T]
	heap       expiryHeap[T]
	free       []*node[T]
	size       int // mirrors len(m); cached because the full-shard check runs on every insert
	// nowSample is the newest clock reading any holder of mu has published,
	// for Options.ApproximateTTLReads. It is a lower bound of the real clock,
	// so deadline <= nowSample proves expiry while deadline > nowSample only
	// means "not yet observed expired".
	nowSample int64
	// heapLive counts authoritative records in heap: one per map entry with
	// a deadline. Stale records are excluded, so heapLive reaching zero is
	// the live-empty signal that must stop the worker's timer even while
	// stale records still occupy the array.
	heapLive int

	hits        uint64
	misses      uint64
	sets        uint64
	deletes     uint64
	evictions   uint64
	expirations uint64
	peeks       uint64
	peekHits    uint64

	// This 16-byte pad keeps the object 192 bytes, which Go allocates from a
	// class whose minimum alignment is 64 bytes (measured: 192-byte objects
	// land at 64-byte offsets every time, while 160-byte objects alternate
	// between offsets 0 and 32). Each shard therefore starts on a cache-line
	// boundary and covers exactly three lines, so neither a neighbour in the
	// same multi-shard cache nor an unrelated single-shard cache allocated
	// next to it can share a line with its hot fields. A 48-byte pad was used
	// before; 208-byte objects are only 16-byte aligned, so that layout relied
	// on a phase argument instead of alignment and cost 16 more bytes.
	_ [16]byte
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
	// collectStats gates every per-operation counter store. Those stores dirty
	// a shard cache line that otherwise stays read-only on Peek, so at high core
	// counts they measurably cap read throughput; Options.DisableStats turns
	// them off for callers that do not export the counters.
	collectStats bool
	// approxReads gates the coarse-clock read path (Options.ApproximateTTLReads).
	approxReads bool
	clock       cacheClock
	start       time.Time

	// wake, done, and stopped exist only once a cleaner worker does. They are
	// created by startWorkerNow under lifeMu before the goroutine starts, so a
	// cache that never writes a TTL holds neither a goroutine nor its channels
	// (about 220 bytes: two of the three ~112-byte channels are unconditional
	// in a TTL-enabled cache, and all three are for one that is idle). A nil
	// stopped means "no worker was ever started", which is exactly the case
	// that needs no join.
	//
	// wake is not an immutable field: notifyCleaner may only read it after
	// observing workerStarted, which startWorkerNow publishes after assigning
	// wake. A caller that still observes zero has no worker to notify, and the
	// worker's first deadlineSnapshot acquires each shard lock, which is itself
	// ordered after every writer that already released one.
	wake    chan struct{}
	done    chan struct{}
	stopped chan struct{}

	// teardown lets a concurrent second Close wait until the first one has
	// finished emptying and unregistering the cache. Add happens under lifeMu
	// before the closed flag is published, and every Wait happens after
	// reacquiring lifeMu, so Add can never race a Wait. It is embedded, so it
	// costs no allocation.
	teardown sync.WaitGroup

	lifeMu        sync.Mutex
	autoStart     bool
	started       bool
	workerStarted uint32
	closed        uint32
}

// shardCount derives the shard count from the capacity, the request, and the
// slot floor. A slotTarget below one selects the default floor; the request
// is rounded down to a power of two and capped so no shard is sized below
// about slotTarget slots.
func shardCount(capacity uint, requested int, slotTarget int) int {
	if capacity == 0 || requested <= 1 {
		return 1
	}
	if slotTarget < 1 {
		slotTarget = shardSlotTarget
	}
	maxN := int(capacity) / slotTarget
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
// uses the nonzero seed installed by the constructor. It consumes 8 bytes per
// round because a byte-at-a-time FNV loop cost ~6ns at 16-byte keys and ~85ns
// at 128-byte keys, which dominated sharded operations on realistic (URL,
// composite, UUID) keys. The final avalanche gives the low bits used by the
// shard mask full diffusion.
func hashKey(s string, seed uint64) uint64 {
	const prime = 0x9e3779b97f4a7c15
	h := seed ^ prime
	for len(s) >= 8 {
		h = (h ^ read8(s)) * prime
		h ^= h >> 29
		s = s[8:]
	}
	if len(s) > 0 {
		var tail uint64
		for i := 0; i < len(s); i++ {
			tail |= uint64(s[i]) << (8 * i)
		}
		h = (h ^ tail) * prime
		h ^= h >> 29
	}
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h += seed
	return h
}

// read8 assembles a little-endian word from the first eight bytes of s. Bounds
// are the caller's responsibility; the explicit byte loads keep the loop
// allocation-free on every supported toolchain.
func read8(s string) uint64 {
	return uint64(s[0]) | uint64(s[1])<<8 | uint64(s[2])<<16 | uint64(s[3])<<24 |
		uint64(s[4])<<32 | uint64(s[5])<<40 | uint64(s[6])<<48 | uint64(s[7])<<56
}

func newCacheState[T CacheAble](capacity uint, enableExpired bool, clock cacheClock, shards int, slotTarget int, preallocate bool, collectStats bool, approxReads bool) *cacheState[T] {
	n := shardCount(capacity, shards, slotTarget)
	s := &cacheState[T]{
		mask:          uint64(n - 1),
		capacity:      capacity,
		enableExpired: enableExpired,
		collectStats:  collectStats,
		approxReads:   approxReads,
		clock:         clock,
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
		sh := &shard[T]{cap: shardCap}
		// Leave the map nil unless preallocation was requested: an idle cache
		// then pays no map header, and Clear/Close stop allocating one per
		// shard they are about to empty. Reads, len, range, and delete on a
		// nil map are all safe, so the first new-key insert is the only site
		// that has to create it.
		if hint > 0 {
			sh.m = make(map[string]*node[T], hint)
		}
		s.shards[i] = sh
	}
	return s
}

func (s *cacheState[T]) isClosed() bool { return atomic.LoadUint32(&s.closed) != 0 }

func (s *cacheState[T]) nowNanos() int64 { return int64(s.clock.Since(s.start)) }

// publishNow records a precise clock sample on the shard for approximate
// reads. The caller must hold sh.mu. A sample older than the stored one is
// dropped: purgeDueBatch passes a now taken before it waited, and regressing
// the sample would re-serve entries a newer writer already proved expired.
func (s *cacheState[T]) publishNow(sh *shard[T], now int64) {
	if !s.approxReads || now <= sh.nowSample {
		return
	}
	sh.nowSample = now
}

// deadlineNow is the clock a read uses to test a deadline: the shard's last
// published sample when approximate reads are on, the precise clock otherwise.
// The caller must hold sh.mu.
func (s *cacheState[T]) deadlineNow(sh *shard[T]) int64 {
	if s.approxReads {
		return sh.nowSample
	}
	return s.nowNanos()
}

func (s *cacheState[T]) nanosAt(t time.Time) int64 { return int64(t.Sub(s.start)) }

func (s *cacheState[T]) shardFor(key string) *shard[T] {
	if len(s.shards) == 1 {
		return s.shards[0]
	}
	return s.shards[hashKey(key, s.seed)&s.mask]
}

// ttlCapable reports whether this cache can ever hold an expiry record. Both
// fields are immutable after construction, so this replaces the old "wake is
// non-nil" probe now that wake is created only when a worker starts.
func (s *cacheState[T]) ttlCapable() bool { return s.enableExpired && s.capacity > 0 }

// notifyCleaner wakes the worker after a heap mutation. The one-slot channel
// coalesces bursts, and a dropped send is harmless because the worker re-reads
// the current heap roots after every wake.
//
// The workerStarted load must come first: it is the publication barrier that
// makes the wake channel assignment safe to read without lifeMu.
func (s *cacheState[T]) notifyCleaner() {
	if atomic.LoadUint32(&s.workerStarted) == 0 {
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// ensureCleaner starts the one worker on the first real TTL write. Public
// caches that never set a TTL therefore hold no goroutine and no channels.
func (s *cacheState[T]) ensureCleaner() {
	if !s.autoStart || !s.ttlCapable() || s.isClosed() {
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
// The worker's channels are created here rather than at construction so an
// unused TTL cache pays for neither of them.
func (s *cacheState[T]) startWorkerNow() {
	if !s.ttlCapable() || s.isClosed() {
		return
	}
	s.lifeMu.Lock()
	if !s.started && !s.isClosed() {
		s.wake = make(chan struct{}, 1)
		s.done = make(chan struct{})
		s.stopped = make(chan struct{})
		s.started = true
		// Publish the channels before the workerStarted store: notifyCleaner
		// reads wake without lifeMu, so it must observe a non-nil channel.
		atomic.StoreUint32(&s.workerStarted, 1)
		go s.runCleaner()
	}
	s.lifeMu.Unlock()
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
		return n
	}
	return &node[T]{key: key, value: value}
}

func (s *shard[T]) release(n *node[T]) {
	var zero T
	n.key = ""
	n.value = zero
	n.prev, n.next = nil, nil
	n.deadline = 0
	// The bump kills any record still pointing at n, so reuse through the
	// freelist can never resurrect it.
	n.heapGen++
	if len(s.free) >= maxFreeNodesPerShard {
		return
	}
	s.free = append(s.free, n)
}

// removeLocked removes one entry from the list and index. Its expiry records
// are cancelled by bumping the generation; the stale records surface at the
// root later and are dropped, so removal does not sift.
func (s *shard[T]) removeLocked(n *node[T]) {
	if n.deadline != 0 {
		s.heapLive--
	}
	n.heapGen++
	s.unlink(n)
	delete(s.m, n.key)
	s.size--
	s.release(n)
	if len(s.heap) == 0 && cap(s.heap) > maxIdleHeapCap {
		s.heap = nil
	}
}

// maybeCompactLocked bounds stale-record memory: once the heap holds at least
// minHeapCompactLen records and more than maxHeapRecordsPerEntry per live
// entry, drop the stale ones. Called from every operation that grows the heap
// or shrinks the map (not from removeLocked itself, so purge loops stay
// amortized), which makes the bound an after-operation invariant.
func (s *shard[T]) maybeCompactLocked() {
	if len(s.heap) >= minHeapCompactLen && len(s.heap) > maxHeapRecordsPerEntry*len(s.m) {
		s.heap.compact()
		s.heapLive = len(s.heap)
	}
}

func (s *shard[T]) purgeExpiredLocked(now int64, limit int, collectStats bool) int {
	removed := 0
	for removed < limit && len(s.heap) > 0 && s.heap[0].deadline <= now {
		rec := s.heap.popRoot()
		if !rec.live() {
			// A record of a renewed, cancelled, or reused node: discard it
			// without touching the entry.
			continue
		}
		if collectStats {
			s.expirations++
		}
		s.removeLocked(rec.n)
		removed++
	}
	s.maybeCompactLocked()
	return removed
}

// reclaimLocked frees exactly one slot: a due heap root when one exists,
// otherwise the LRU tail. It never scans for further expired entries, so one
// write's reclamation work stays bounded.
func (s *shard[T]) reclaimLocked(now int64, collectStats bool) {
	if s.purgeExpiredLocked(now, 1, collectStats) > 0 {
		return
	}
	if s.tail == nil {
		return
	}
	if collectStats {
		s.evictions++
	}
	s.removeLocked(s.tail)
}

func (s *shard[T]) clearLocked() {
	s.m = nil
	s.head, s.tail = nil, nil
	s.heap = nil
	s.heapLive = 0
	s.free = nil
	s.size = 0
}

// rootDeadline is the earliest live deadline in this shard (zero when none).
// Stale records at the root are popped on the way: callers hold the shard
// lock, and letting a stale root survive would arm the timer at a deadline no
// live entry has, delaying the empty-heap stop.
func (s *shard[T]) rootDeadline() int64 {
	for len(s.heap) > 0 && !s.heap[0].live() {
		s.heap.popRoot()
	}
	if len(s.heap) == 0 {
		return 0
	}
	return s.heap[0].deadline
}

// rootNeedsWake reports whether the cleaner must re-examine its timer after one
// shard's earliest deadline moved from before to after. A root that only moves
// later (both values nonzero) can never make the armed timer too late: the
// timer fires at the older, earlier deadline, the worker re-reads the real
// roots, and re-arms. Every other change needs a wake, because an earlier root
// needs an earlier timer and an emptied or newly non-empty heap needs the timer
// stopped or armed.
//
// This filter is what keeps a cache under sustained TTL renewal from spinning
// the worker. Renewal pushes a deadline later, which is the common TTL write
// shape; notifying those changes made the one-slot wake permanently full, so
// the worker looped over every shard lock without ever reclaiming anything.
//
// Zero means "this shard's heap is empty", so an emptied heap arrives here as
// after == 0 with before > 0, which the after < before test already covers.
// There is deliberately no separate empty-heap clause: adding one back is
// equivalent, but it reads as if it carried a condition of its own.
func rootNeedsWake(before, after int64) bool {
	if before == after {
		return false
	}
	return before == 0 || after < before
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
	var oldDeadline int64
	if ok {
		oldDeadline = n.deadline
	}

	// One set of predicates serves the clock read, the root tracking, and the
	// reclaim step, so a full-shard insert does not re-evaluate them.
	reclaim := !ok && sh.size >= sh.cap
	// The heap is touched when this write adds a deadline, cancels one, or
	// reclaims a due root from a full shard.
	touchesHeap := expiring || oldDeadline != 0 || (reclaim && len(sh.heap) > 0)

	// The clock is read only when a deadline participates in this write. A
	// cache holding only non-expiring entries never reads it. The read stays
	// inside the critical section: with one shard it is cheaper to keep the
	// lock busy than to shorten the hold (measured, see the round-3 report).
	var now int64
	if touchesHeap {
		now = s.nowNanos()
		s.publishNow(sh, now)
	}

	var deadline int64
	if expiring {
		deadline = now + ttlNanos
		if deadline < now {
			deadline = maxInt64
		}
	}

	// Renewing an existing deadline to a later value can only raise this
	// shard's root, which the armed timer already covers, so skip both root
	// reads. This is the dominant TTL write shape.
	extends := expiring && oldDeadline != 0 && deadline > oldDeadline
	trackRoot := touchesHeap && !extends
	var rootBefore int64
	if trackRoot {
		rootBefore = sh.rootDeadline()
	}

	if oldDeadline != 0 && oldDeadline <= now {
		// An expired target is treated as absent: remove only its own record.
		if s.collectStats {
			sh.expirations++
		}
		sh.removeLocked(n)
		n, ok = nil, false
		// The removed node carried the old deadline; the insert below must
		// compare against zero or an equal-value rewrite would skip its push.
		oldDeadline = 0
	}

	if ok {
		n.value = value
		sh.moveFront(n)
	} else {
		if reclaim {
			sh.reclaimLocked(now, s.collectStats)
		}
		n = sh.acquire(key, value)
		sh.pushFront(n)
		if sh.m == nil {
			sh.m = make(map[string]*node[T])
		}
		sh.m[key] = n
		sh.size++
	}

	// A deadline change bumps the generation (cancelling any earlier record)
	// and pushes one fresh record; an unchanged deadline touches nothing.
	if deadline != oldDeadline {
		n.heapGen++
		n.deadline = deadline
		if oldDeadline != 0 {
			sh.heapLive-- // the replaced record just went stale
		}
		if deadline != 0 {
			sh.heap.push(n, deadline)
			sh.heapLive++
			sh.maybeCompactLocked()
		}
	} else {
		n.deadline = deadline
	}
	if s.collectStats {
		sh.sets++
	}
	changed := trackRoot && rootNeedsWake(rootBefore, sh.rootDeadline())
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
		if s.collectStats {
			sh.misses++
		}
		sh.mu.Unlock()
		var zero T
		return zero, false
	}
	if n.deadline != 0 && n.deadline <= s.deadlineNow(sh) {
		if s.collectStats {
			sh.expirations++
		}
		sh.removeLocked(n)
		sh.maybeCompactLocked()
		// Removing an entry can only raise the shard root or empty the heap,
		// and an armed timer already covers a root that moved later. Only a
		// live-empty heap needs the worker to stop its timer; stale records
		// left behind keep the array non-empty, and the worker drops them
		// when its timer next fires.
		changed := sh.heapLive == 0
		if s.collectStats {
			sh.misses++
		}
		sh.mu.Unlock()
		if changed {
			s.notifyCleaner()
		}
		var zero T
		return zero, false
	}
	sh.moveFront(n)
	if s.collectStats {
		sh.hits++
	}
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
	if s.collectStats {
		sh.peeks++
	}
	n, ok := sh.m[key]
	if !ok {
		sh.mu.Unlock()
		var zero T
		return zero, false
	}
	if n.deadline != 0 && n.deadline <= s.deadlineNow(sh) {
		sh.mu.Unlock()
		var zero T
		return zero, false
	}
	if s.collectStats {
		sh.peekHits++
	}
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
		if s.collectStats {
			sh.deletes++
		}
		sh.removeLocked(n)
		sh.maybeCompactLocked()
		// Same contract as the expired-read removal: a live-empty heap needs
		// the worker's timer stopped, stale records notwithstanding.
		changed = sh.heapLive == 0
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
			s.publishNow(sh, now)
			removed := sh.purgeExpiredLocked(now, sizePurgeBatch, s.collectStats)
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
	// done and stopped are written only under lifeMu, so reading them here is
	// race-free. Both are nil when no worker was ever started.
	stoppable := s.stopped
	if s.isClosed() {
		s.lifeMu.Unlock()
		// Another Close is already tearing this cache down. Join it rather
		// than returning while the contents and registration still exist.
		s.teardown.Wait()
		return
	}
	// Publish "closing" only after the teardown count is raised, so a closer
	// that observes the flag can safely Wait.
	s.teardown.Add(1)
	atomic.StoreUint32(&s.closed, 1)
	if s.done != nil {
		close(s.done)
	}
	s.lifeMu.Unlock()

	defer s.teardown.Done()
	s.clearShards()
	// Never hold a shard lock while acquiring the registry.
	lives.remove(s)
	if stoppable != nil {
		<-stoppable
	}
}

func (s *cacheState[T]) purgeDueBatch(now int64, limit int) int {
	removed := 0
	for _, sh := range s.shards {
		if removed >= limit || s.isClosed() {
			break
		}
		sh.mu.Lock()
		s.publishNow(sh, now)
		removed += sh.purgeExpiredLocked(now, limit-removed, s.collectStats)
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
