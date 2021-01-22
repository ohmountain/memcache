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
		if !reflect.DeepEqual(m.header.value, v) {
			t.Fatalf("m.Header error, Want: %v, got: %v", v, m.header.value)
		}

		// 尾巴
		func() {
			if uint(i) < capacity-1 {
				if !reflect.DeepEqual(m.tail.value, i+1) {
					t.Fatalf("m.Tail error, Want: %v, got: %v", i+1, m.tail.value)
				}
			}

			// 循环一遍
			if uint(i) == capacity-1 {
				if !reflect.DeepEqual(m.tail.value, 0) {
					t.Fatalf("m.Tail error, Want: %v, got: %v", 0, m.tail.value)
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
			r := rand.Intn(1000)
			run(r, t, &wg)
			<-ch
		}()
	}

	wg.Wait()

}
