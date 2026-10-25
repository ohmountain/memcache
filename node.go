package memcache

import (
	"container/list"
	"time"
)

// CacheAble preserves the original generic constraint; any value is supported.
type CacheAble any

type node[T CacheAble] struct {
	key         string
	value       T
	element     *list.Element
	expiresAt   time.Time
	expiryIndex int // -1 when the node has no expiration record
}

// expiryHeap keeps exactly one record per expiring node. Indices permit updates
// and cancellation in O(log n), without accumulating stale deadline records.
type expiryHeap[T CacheAble] []*node[T]

func (h expiryHeap[T]) Len() int           { return len(h) }
func (h expiryHeap[T]) Less(i, j int) bool { return h[i].expiresAt.Before(h[j].expiresAt) }
func (h expiryHeap[T]) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].expiryIndex = i
	h[j].expiryIndex = j
}
func (h *expiryHeap[T]) Push(value any) {
	n := value.(*node[T])
	n.expiryIndex = len(*h)
	*h = append(*h, n)
}
func (h *expiryHeap[T]) Pop() any {
	last := len(*h) - 1
	n := (*h)[last]
	(*h)[last] = nil // Do not retain removed values in the backing array.
	*h = (*h)[:last]
	n.expiryIndex = -1
	return n
}
