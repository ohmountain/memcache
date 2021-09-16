package memcache

import (
	"os"
	"os/signal"
	"time"
)

//
// LRU策略的K/V内存缓存器
// 这个实现是并发安全的
//
type Memcache struct {

	// 当前已缓存数量
	size uint

	// 允许缓存容量
	cap uint

	// 记录最后访问的一个数据
	header *node

	// 记录最早访问的一个数据
	// 如果容量溢出，将不再缓存
	tail *node

	// 读写控制器
	// 所有读写操作都必须获得这个锁
	// 操作完成必须释放这个锁
	locker chan struct{}

	// 数据存储器，一个map
	// 读取和写入的复杂度都为O(1)
	holder map[string]*node

	// 是否允许超时处理
	enableExpired bool

	// 如果设置了过期时间
	// 此处存储了数据的过期时间和key
	expired map[int64][]string
}

// 使用WithLRU策略的缓存器
func WithLRU(cap uint, enableExpired bool) *Memcache {
	ins := &Memcache{
		size:          0,
		cap:           cap,
		header:        nil,
		tail:          nil,
		locker:        make(chan struct{}, 1),
		holder:        map[string]*node{},
		expired:       make(map[int64][]string),
		enableExpired: enableExpired,
	}

	if !enableExpired {
		return ins
	}

	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt)
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

func (m *Memcache) Set(key string, value interface{}) {
	m.locker <- struct{}{}
	defer func() {
		<-m.locker
	}()

	// 最初一个缓存内容
	if m.size == 0 {
		ins := &node{
			Key:   key,
			Value: value,
			Prv:   nil,
			Nxt:   nil,
		}
		m.header = ins
		m.tail = ins
		m.size = m.size + 1

		m.holder[key] = ins
		return
	}

	ins, ext := m.holder[key]
	if !ext {
		ins = &node{
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
}

func (m *Memcache) SetExpire(key string, value interface{}, ttl int64) {

	// 如果初始化时没有启用过期机制
	// 则不设置值
	if !m.enableExpired {
		return
	}

	m.locker <- struct{}{}
	ttlKey := time.Now().UnixNano()/1e6 + ttl*1000
	ext, ok := m.expired[ttlKey]
	if !ok {
		ext = make([]string, 0)
		ext = append(ext, key)
		m.expired[ttlKey] = ext
		<-m.locker
		m.Set(key, value)
		return
	}

	ext = append(ext, key)
	m.expired[ttlKey] = ext
	<-m.locker

	m.Set(key, value)
}

func (m *Memcache) checkExpired() {
	now := time.Now().UnixNano() / 1e6
	for ttl, values := range m.expired {
		if ttl <= now {
			for _, key := range values {
				m.Delete(key)
			}

			m.locker <- struct{}{}
			delete(m.expired, ttl)
			<-m.locker
		}
	}
}

func (m *Memcache) Get(key string) interface{} {
	ins, ext := m.holder[key]
	if !ext {
		return nil
	}

	if ins.Prv != nil {

		m.locker <- struct{}{}

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

		<-m.locker
	}

	return ins.Value
}

func (m *Memcache) Delete(key string) {
	if m.Get(key) == nil {
		return
	}

	m.locker <- struct{}{}
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
	<-m.locker
}

func (m *Memcache) Size() uint {
	return m.size
}

func (m *Memcache) Cap() uint {
	return m.cap
}
