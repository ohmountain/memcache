package memcache

type node struct {
	Key   string
	Value interface{}
	Prv   *node
	Nxt   *node
}
