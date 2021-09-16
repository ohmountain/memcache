package memcache

import (
	"fmt"
	"math/rand"
	"reflect"
	"sync"
	"testing"
	"time"
)

func run(n int, t *testing.T, wg *sync.WaitGroup) {

	defer wg.Done()

	capacity := uint(n)
	m := WithLRU(capacity, false)
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

func Test_Delete(t *testing.T) {
	m := WithLRU(1, false)
	m.Set("hello", "world")
	if !reflect.DeepEqual(m.Get("hello"), "world") {
		t.Logf("Error")
		t.FailNow()
	}

	m.Delete("hello")
	if m.Size() != 0 {
		t.Logf("Error size")
		t.FailNow()
	}

	if m.header != nil {
		t.Logf("Error header")
		t.FailNow()
	}

	if m.tail != nil {
		t.Logf("Error tail")
		t.FailNow()
	}

	m = WithLRU(2, false)
	m.Set("1", 1)
	m.Set("2", 2)

	m.Delete("1")
	if !reflect.DeepEqual(2, m.header.Value) {
		t.Logf("Error header, 2 != %d", m.header.Value)
		t.FailNow()
	}
}

func Test_TTl(t *testing.T) {
	m := WithLRU(100, true)
	ticker := time.NewTicker(3600 * time.Second)

	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("%d", i)
		val := fmt.Sprintf("hello-%d", i)
		m.SetExpire(key, val, 2)
	}

	t.Logf("缓存器还有数据哦: %d", m.Size())

	for i := 0; i < 100; i++ {
		c := WithLRU(100, true)
		for j := 0; j < 100; j++ {
			key := fmt.Sprintf("%d", j)
			c.SetExpire(key, "bbbb", int64(j+1))
		}
	}

loop:
	for {
		select {
		case <-ticker.C:
			break loop
		}
	}

	if m.Size() > 0 {
		t.Logf("错误：缓存器还有数据哦: %d", m.Size())
	}

	t.Logf("%#v", m.expired)
}
