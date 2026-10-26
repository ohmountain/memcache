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
	key         string
	value       T
	prev        *node[T]
	next        *node[T]
	deadline    int64 // monotonic nanoseconds since cacheState.start; 0 = no TTL
	expiryIndex int   // -1 when the node has no expiration record
}

// expiryHeap keeps exactly one record per expiring node. Indices permit updates
// and cancellation in O(log n), without accumulating stale deadline records.
// The methods are concrete (not container/heap interface calls) so the hot TTL
// paths avoid interface dispatch.
type expiryHeap[T CacheAble] []*node[T]

func (h expiryHeap[T]) less(i, j int) bool { return h[i].deadline < h[j].deadline }

func (h expiryHeap[T]) swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].expiryIndex = i
	h[j].expiryIndex = j
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

func (h *expiryHeap[T]) fix(i int) {
	if !h.down(i, len(*h)) {
		h.up(i)
	}
}

func (h *expiryHeap[T]) push(n *node[T]) {
	n.expiryIndex = len(*h)
	*h = append(*h, n)
	h.up(len(*h) - 1)
}

// removeAt removes the record at i and clears that node's index. The moved
// last node gets its new index; the removed node always ends at -1 so a later
// overwrite can never call removeAt again with a stale position.
func (h *expiryHeap[T]) removeAt(i int) {
	last := len(*h) - 1
	removed := (*h)[i]
	tail := (*h)[last]
	(*h)[last] = nil
	if i != last {
		(*h)[i] = tail
		tail.expiryIndex = i
		*h = (*h)[:last]
		removed.expiryIndex = -1
		h.fix(i)
		return
	}
	*h = (*h)[:last]
	removed.expiryIndex = -1
}

// rebuild restores the heap invariant after a test or an out-of-band deadline
// rewrite, and refreshes every index.
