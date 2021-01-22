package memcache

type node struct {
	value interface{}
	prv   *node
	nxt   *node
}
