package memcache

// CacheAble preserves the original generic constraint; any value is supported.
type CacheAble any

// node is the map value. The LRU links live in the node: a hit's moveFront
// writes the same allocation readers load. For narrow payloads such as int or
// uint64 the node is 56 bytes and rounds to the 64-byte size class, so it
// occupies one cache line and does not false-share with a neighbour; wider T
// values grow it past one line, so that property is per instantiation. A
// separate link object cost 8 more bytes per entry in the narrow case.
type node[T CacheAble] struct {
	key      string
	value    T
	prev     *node[T]
	next     *node[T]
	deadline int64 // monotonic nanoseconds since cacheState.start; 0 = no TTL
	// heapGen invalidates older expiry records. It is bumped whenever the
	// deadline changes, the record is cancelled, or the node is released, so
	// a heap record stays authoritative only while its stored generation
	// still matches. A plain counter suffices: a released node's generation
	// only ever moves forward, so a reacquired node cannot resurrect an old
	// record. Width matches the removed expiryIndex field, keeping narrow
	// nodes inside one cache line.
	heapGen uint64
}

// heapRec is one expiry-heap record. The deadline is an immutable snapshot
// taken at push time: ordering must not observe node.deadline, which mutates
// on renewal while the old record still orders the heap.
type heapRec[T CacheAble] struct {
	n        *node[T]
	deadline int64
	gen      uint64
}

// live reports whether rec is still its node's authoritative record.
func (rec heapRec[T]) live() bool { return rec.gen == rec.n.heapGen }

// expiryHeap holds at most one live record per expiring node, plus stale
// records left behind by renewals and cancellations. Stale records are
// dropped when they surface at the root and swept by compact, so renewal and
// cancellation never sift and never write nodes. The methods are concrete
// (not container/heap interface calls) so the hot TTL paths avoid interface
// dispatch.
type expiryHeap[T CacheAble] []heapRec[T]

func (h expiryHeap[T]) less(i, j int) bool { return h[i].deadline < h[j].deadline }

// swap moves records only; it writes no node, unlike the pre-lazy swap that
// maintained node indices.
func (h *expiryHeap[T]) swap(i, j int) {
	(*h)[i], (*h)[j] = (*h)[j], (*h)[i]
}

func (h *expiryHeap[T]) up(j int) {
	for {
		i := (j - 1) / 2
		if i == j || !h.less(j, i) {
			break
		}
		h.swap(i, j)
		j = i
	}
}

func (h *expiryHeap[T]) down(i0, n int) bool {
	i := i0
	for {
		j1 := 2*i + 1
		if j1 >= n || j1 < 0 {
			break
		}
		j := j1
		if j2 := j1 + 1; j2 < n && h.less(j2, j1) {
			j = j2
		}
		if !h.less(j, i) {
			break
		}
		h.swap(i, j)
		i = j
	}
	return i > i0
}

// push appends a record for n at its current generation. Callers bump
// n.heapGen before pushing so any earlier record of n is stale from here on.
func (h *expiryHeap[T]) push(n *node[T], deadline int64) {
	*h = append(*h, heapRec[T]{n: n, deadline: deadline, gen: n.heapGen})
	h.up(len(*h) - 1)
}

// popRoot removes and returns heap[0]. It never writes a node: the record may
// be stale, and its node may hold a different live record elsewhere.
func (h *expiryHeap[T]) popRoot() heapRec[T] {
	rec := (*h)[0]
	last := len(*h) - 1
	(*h)[0] = (*h)[last]
	(*h)[last] = heapRec[T]{}
	*h = (*h)[:last]
	if last > 0 {
		h.down(0, len(*h))
	}
	return rec
}

// compact drops stale records in place and restores heap order. The write
// index never passes the read index, so the in-place filter is safe.
func (h *expiryHeap[T]) compact() {
	out := (*h)[:0]
	for _, rec := range *h {
		if rec.live() {
			out = append(out, rec)
		}
	}
	for i := len(out)/2 - 1; i >= 0; i-- {
		out.down(i, len(out))
	}
	*h = out
}
