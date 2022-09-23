package memcache

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Memcache
// LRU策略的K/V内存缓存器
// 这个实现是并发安全的
//
type Memcache[T CacheAble] struct {

	// 读写控制器
	// 所有读写操作都必须获得这个锁
	// 操作完成必须释放这个锁
	sync.RWMutex

	// 当前已缓存数量
	size uint

	// 允许缓存容量
	cap uint

	// 记录最后访问的一个数据
	header *node[T]

	// 记录最早访问的一个数据
	// 如果容量溢出，将不再缓存
	tail *node[T]

	// 数据存储器，一个map
	// 读取和写入的复杂度都为O(1)
	holder map[string]*node[T]

	// 是否允许超时处理
	enableExpired bool

	// 如果设置了过期时间
	// 此处存储了数据的过期时间和key
	expired map[int64][]string
}

// WithLRU 使用WithLRU策略的缓存器
func WithLRU[T CacheAble](cap uint, enableExpired bool) *Memcache[T] {

	ins := &Memcache[T]{
		size:          0,
		cap:           cap,
		header:        nil,
		tail:          nil,
		holder:        map[string]*node[T]{},
		expired:       make(map[int64][]string),
		enableExpired: enableExpired,
	}

	lives.add(ins)

	if !enableExpired {
		return ins
	}

	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGHUP, syscall.SIGINT, syscall.SIGSEGV, os.Interrupt, os.Kill)
	ticker := time.NewTicker(time.Second / 10) // 0.1s

	go func() {
		for {
			select {
			case <-c:
				ticker.Stop()
				return
			case <-ticker.C:
				ins.checkExpired()
			}
		}
	}()

	return ins
}

func (m *Memcache[T]) Set(key string, value T) {
	m.Lock()

	// 最初一个缓存内容
	if m.size == 0 {
		ins := &node[T]{
			Key:   key,
			Value: value,
			Prv:   nil,
			Nxt:   nil,
		}
		m.header = ins
		m.tail = ins
		m.size = m.size + 1

		m.holder[key] = ins
		m.Unlock()
		return
	}

	ins, ext := m.holder[key]
	if !ext {
		ins = &node[T]{
			Key:   key,
			Value: value,
			Prv:   nil,
			Nxt:   m.header,
		}

		if m.header == nil {
			m.header = ins
		} else {
			m.header.Prv = ins
			ins.Nxt = m.header
			m.header = ins
		}

		// 处理最后尾巴
		if m.size == m.cap {
			tail := m.tail
			if tail.Prv != nil {
				prv := tail.Prv
				m.tail = prv
				prv.Nxt = nil
			}
			tail = nil

		} else {
			m.size = m.size + 1
		}

		m.holder[key] = ins
	} else {

		ins.Value = value

		// 如果这个key不是头部
		// 那么将它移动到头部
		// 并关联它的下一个到它的上一个
		if ins.Prv != nil {

			// 首尾相连
			ins.Prv.Nxt = ins.Nxt
			ins.Prv = nil

			// 移动到头部
			m.header.Prv = ins
			ins.Nxt = m.header

			m.header = ins
		}
	}

	m.Unlock()
}

func (m *Memcache[T]) SetExpire(key string, value T, ttl int64) {

	// 如果初始化时没有启用过期机制
	// 则不设置值
	if !m.enableExpired {
		return
	}

	m.Lock()
	ttlKey := time.Now().UnixNano()/1e6 + ttl*1000
	ext, ok := m.expired[ttlKey]

	if !ok {
		ext = make([]string, 0)
		ext = append(ext, key)
		m.expired[ttlKey] = ext
		m.Unlock()
		m.Set(key, value)
		return
	}

	ext = append(ext, key)
	m.expired[ttlKey] = ext
	m.Unlock()

	m.Set(key, value)
}

func (m *Memcache[T]) checkExpired() {
	now := time.Now().UnixNano() / 1e6
	for ttl, values := range m.expired {
		if ttl <= now {
			for _, key := range values {
				m.Delete(key)
			}

			m.Lock()
			delete(m.expired, ttl)
			m.Unlock()
		}
	}
}

// Get 获取一个缓存
func (m *Memcache[T]) Get(key string) *T {
	m.Lock()
	ins, ext := m.holder[key]
	if !ext {
		m.Unlock()
		return nil
	}

	if ins.Prv != nil {

		// 如果已读的这个是尾巴
		// 那么它的上一个将变成尾巴
		if ins.Nxt == nil {
			m.tail = ins.Prv
		}

		// 首尾相连
		ins.Prv.Nxt = ins.Nxt
		ins.Prv = nil

		// 移动到头部
		m.header.Prv = ins
		ins.Nxt = m.header
		m.header = ins

	}
	m.Unlock()

	return &(ins.Value)
}

// Delete 主动删除缓存
func (m *Memcache[T]) Delete(key string) {
	if m.Get(key) == nil {
		return
	}

	m.Lock()
	origin := m.header

	if origin.Nxt != nil {
		origin.Prv = nil
		m.header = origin.Nxt
	} else {
		m.header = nil
		m.tail = nil
	}
	m.size = m.size - 1
	delete(m.holder, key)
	m.Unlock()
}

// Size 获取当前长度
func (m *Memcache[T]) Size() uint {
	return m.size
}

// Cap 获取缓存容量
func (m *Memcache[T]) Cap() uint {
	return m.cap
}

// 清除所有缓存
func (m *Memcache[T]) Clear() {
	m.Lock()
	header := m.header
	for {
		if header == nil {
			break
		}

		// 解引用
		next := header.Nxt
		header.Nxt = nil
		header = next
	}

	m.header = nil
	m.tail = nil
	m.size = 0
	m.expired = make(map[int64][]string)
	m.holder = make(map[string]*node[T])
	m.Unlock()
}
