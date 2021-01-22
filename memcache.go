package memcache

import (
	"io"
)

//
// LRU策略的K/V内存缓存器
// 这个实现是并发安全的
//
type memcache struct {

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

	// 用于从reader里面恢复之前的存储
	reader io.Reader

	// 将当前的缓存状态写入到writer中
	writer io.Writer
}

// 使用WithLRU策略的缓存器
func WithLRU(cap uint) *memcache {
	return &memcache{
		size:   0,
		cap:    cap,
		header: nil,
		tail:   nil,
		locker: make(chan struct{}, 1),
		holder: map[string]*node{},
	}
}

func (m *memcache) Set(key string, value interface{}) {
	m.locker <- struct{}{}
	defer func() {
		<-m.locker
	}()

	// 最初一个缓存内容
	if m.size == 0 {
		ins := &node{
			value: value,
			prv:   nil,
			nxt:   nil,
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
			value: value,
			prv:   nil,
			nxt:   m.header,
		}

		if m.header == nil {
			m.header = ins
		} else {
			m.header.prv = ins
			ins.nxt = m.header
			m.header = ins
		}

		// 处理最后尾巴
		if m.size == m.cap {
			tail := m.tail
			if tail.prv != nil {
				prv := tail.prv
				m.tail = prv
				prv.nxt = nil
			}
			tail = nil

		} else {
			m.size = m.size + 1
		}

		m.holder[key] = ins
	} else {

		ins.value = value

		// 如果这个key不是头部
		// 那么将它移动到头部
		// 并关联它的下一个到它的上一个
		if ins.prv != nil {

			// 首尾相连
			ins.prv.nxt = ins.nxt
			ins.prv = nil

			// 移动到头部
			m.header.prv = ins
			ins.nxt = m.header

			m.header = ins
		}
	}
}

func (m *memcache) Get(key string) interface{} {
	ins, ext := m.holder[key]
	if !ext {
		return nil
	}

	if ins.prv != nil {

		m.locker <- struct{}{}

		// 如果已读的这个是尾巴
		// 那么它的上一个将变成尾巴
		if ins.nxt == nil {
			m.tail = ins.prv
		}

		// 首尾相连
		ins.prv.nxt = ins.nxt
		ins.prv = nil

		// 移动到头部
		m.header.prv = ins
		ins.nxt = m.header
		m.header = ins

		<-m.locker
	}

	return ins.value
}

func (m *memcache) Size() uint {
	return m.size
}

func (m *memcache) Cap() uint {
	return m.cap
}
