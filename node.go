package memcache

type CacheAble any

type node[T CacheAble] struct {
	Key   string
	Value T
	Prv   *node[T]
	Nxt   *node[T]
}
