///
/// THIS IS GLOBAL MONITOR FOR MEMCACHE
///

package memcache

import "sync"

// 用来保存实例索引，
// 方便获得一些观察的内容
type monitor struct {
	sync.RWMutex
	ins []*Memcache
}

func (ins *monitor) add(m *Memcache) {
	ins.Lock()
	ins.ins = append(ins.ins, m)
	ins.Unlock()
}

var lives = monitor{
	ins: make([]*Memcache, 0),
}

// 清除整个缓存池子
func CLEAR_ALL() {
	lives.Lock()
	for _, m := range lives.ins {
		m.Clear()
	}
	lives.Unlock()
}

// 缓存实例数量
func Lives() int {
	return len(lives.ins)
}
