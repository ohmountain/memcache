package memcache

import (
	"fmt"
	"math/rand"
	"reflect"
	"sync"
	"testing"
)

func run(n int, t *testing.T, wg *sync.WaitGroup) {

	defer wg.Done()

	capacity := uint(n)
	m := WithLRU(capacity)
	if !reflect.DeepEqual(capacity, m.Cap()) {
		t.Fatalf("容量应该是：%d, 但是确是：%d", capacity, m.Cap())
	}

	for i := 0; i < int(capacity); i++ {
		m.Set(fmt.Sprintf("%d", i), i)
	}

	if !reflect.DeepEqual(capacity, m.Size()) {
		t.Fatalf("长度应该是：%d, 但是确是：%d", capacity, m.Size())
	}

	for i := 0; i < int(capacity); i++ {
		v := m.Get(fmt.Sprintf("%d", i))
		if !reflect.DeepEqual(v, i) {
			t.Fatalf("Want: %v, got: %v", i, v)
		}

		// 每次读取后，读取的节点都会变成header
		if !reflect.DeepEqual(m.header.Value, v) {
			t.Fatalf("m.Header error, Want: %v, got: %v", v, m.header.Value)
		}

		// 尾巴
		func() {
			if uint(i) < capacity-1 {
				if !reflect.DeepEqual(m.tail.Value, i+1) {
					t.Fatalf("m.Tail error, Want: %v, got: %v", i+1, m.tail.Value)
				}
			}

			// 循环一遍
			if uint(i) == capacity-1 {
				if !reflect.DeepEqual(m.tail.Value, 0) {
					t.Fatalf("m.Tail error, Want: %v, got: %v", 0, m.tail.Value)
				}
			}
		}()
	}
}

func Test_RandomCapacity(t *testing.T) {

	wg := sync.WaitGroup{}
	ch := make(chan struct{}, 8)

	for i := 0; i < 100; i++ {
		ch <- struct{}{}
		wg.Add(1)
		go func() {
			r := rand.Intn(100)
			run(r, t, &wg)
			<-ch
		}()
	}

	wg.Wait()

}

func Test_Shadow(t *testing.T) {
	m := WithLRU(10)
	for i := 0; i < int(m.cap); i++ {
		m.Set(fmt.Sprintf("%d", i), i)
	}

	s := m.shadow()

	m1 := s.Memcache()

	h1 := m1.header
	l1 := m1.tail

	err := s.Persist()
	if err != nil {
		t.Fatalf("Error: %s", err)
	}

	file := fmt.Sprintf("%d.bin", s.Version)
	s1 := shadow{}
	s1.FromFile(file)
	if err != nil {
		t.Fatalf("Error: %s", err)
	}
	m2 := s1.Memcache()

	h2 := m2.header
	l2 := m2.tail

	//
	// 这个测试就是要保证保存快照到硬盘后，再从硬盘快照中恢复时
	// 缓存内容是一致的，淘汰机制是一致的
	//

	// 按顺序测试
	for {
		if h1 == nil || h2 == nil {
			break
		}

		if !reflect.DeepEqual(h1.Key, h2.Key) {
			t.Fatalf("Next.Key fail: expected [%s], got [%s]", h1.Key, h2.Key)
		}

		if !reflect.DeepEqual(h1.Value, h2.Value) {
			t.Fatalf("Next.Value fail: expected [%s], got [%s]", h1.Value, h2.Value)
		}

		h1 = h1.Nxt
		h2 = h2.Nxt
	}

	// 按逆序测试
	for {

		if l1 == nil || l2 == nil {
			break
		}

		if !reflect.DeepEqual(l1.Key, l2.Key) {
			t.Fatalf("Prev.Key fail: expected [%s], got [%s]", l1.Key, l2.Key)
		}

		if !reflect.DeepEqual(l1.Value, l2.Value) {
			t.Fatalf("Prev.Value fail: expected [%s], got [%s]", l1.Value, l2.Value)
		}

		l1 = l1.Prv
		l2 = l2.Prv
	}
}
