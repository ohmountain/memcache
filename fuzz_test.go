package memcache

import (
	"reflect"
	"testing"
	"time"
)

// The model uses a small slice and a logical clock, independently of the
// production linked list and heap. Fuzzing compares observable values and the
// complete LRU order after every operation.
type modelItem struct {
	value    int
	deadline int64
}
type cacheModel struct {
	capacity int
	now      int64
	items    map[string]modelItem
	order    []string
}

func (m *cacheModel) remove(key string) {
	delete(m.items, key)
	for i, existing := range m.order {
		if key == existing {
			m.order = append(m.order[:i], m.order[i+1:]...)
			return
		}
	}
}

func (m *cacheModel) purge() {
	for key, item := range m.items {
		if item.deadline > 0 && item.deadline <= m.now {
			m.remove(key)
		}
	}
}

func (m *cacheModel) set(key string, value int, ttl int64, expiring bool) {
	m.purge()
	if m.capacity == 0 {
		return
	}
	if expiring && ttl <= 0 {
		m.remove(key)
		return
	}
	m.remove(key)
	if len(m.items) == m.capacity {
		m.remove(m.order[len(m.order)-1])
	}
	item := modelItem{value: value}
	if expiring {
		item.deadline = m.now + ttl
	}
	m.items[key] = item
	m.order = append([]string{key}, m.order...)
}

func (m *cacheModel) get(key string) (int, bool) {
	m.purge()
	item, ok := m.items[key]
	if !ok {
		return 0, false
	}
	m.remove(key)
	m.items[key] = item
	m.order = append([]string{key}, m.order...)
	return item.value, true
}

func FuzzCacheModel(f *testing.F) {
	f.Add(uint8(2), []byte{0, 0, 1, 0, 1, 2, 0, 2, 3, 2, 0, 0})
	f.Add(uint8(3), []byte{1, 0, 2, 1, 0, 6, 5, 0, 3, 6, 0, 0, 2, 0, 0})
	f.Add(uint8(0), []byte{0, 0, 1, 1, 1, 2, 2, 0, 0})
	f.Add(uint8(1), []byte{1, 0, 3, 3, 0, 0, 0, 0, 7, 5, 0, 5, 7, 0, 0})
	f.Fuzz(func(t *testing.T, rawCapacity uint8, operations []byte) {
		if len(operations) > 2048 {
			t.Skip("bounded operation sequence")
		}
		capacity := int(rawCapacity % 8)
		clock := &testClock{now: time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)}
		m := newMemcache[int](uint(capacity), true, clock.Now, false)
		defer m.Close()
		model := cacheModel{capacity: capacity, items: make(map[string]modelItem)}
		keys := []string{"", "a", "b", "c", "中文", "\x00", "another-key", "last"}
		for i := 0; i+2 < len(operations); i += 3 {
			key := keys[int(operations[i+1])%len(keys)]
			value := int(operations[i+2])
			switch operations[i] % 9 {
			case 0:
				m.Set(key, value)
				model.set(key, value, 0, false)
			case 1:
				ttl := int64(operations[i+2]%8) - 1
				m.SetExpire(key, value, ttl)
				model.set(key, value, ttl, true)
			case 2:
				got, ok := m.Lookup(key)
				want, present := model.get(key)
				if got != want || ok != present {
					t.Fatalf("step %d Lookup(%q)=(%d,%v), want (%d,%v)", i/3, key, got, ok, want, present)
				}
			case 3:
				m.Delete(key)
				model.remove(key)
			case 4:
				m.Clear()
				model.items = make(map[string]modelItem)
				model.order = nil
			case 5:
				seconds := int64(operations[i+2]%5) + 1
				clock.Advance(time.Duration(seconds) * time.Second)
				model.now += seconds
			case 6:
				m.checkExpired()
			case 7:
				got := m.Get(key)
				want, present := model.get(key)
				if (got != nil) != present || (got != nil && *got != want) {
					t.Fatalf("step %d Get(%q) disagrees with model", i/3, key)
				}
				if got != nil {
					*got = -1
				}
			case 8:
				m.Size()
			}
			model.purge()
			if size := m.Size(); int(size) != len(model.items) {
				t.Fatalf("step %d size=%d, want %d", i/3, size, len(model.items))
			}
			assertInvariants(t, m)
			var order []string
			for e := m.state.order.Front(); e != nil; e = e.Next() {
				order = append(order, e.Value.(*node[int]).key)
			}
			if len(order) != len(model.order) || (len(order) > 0 && !reflect.DeepEqual(order, model.order)) {
				t.Fatalf("step %d LRU order=%q, want %q", i/3, order, model.order)
			}
			for key, item := range model.items {
				n := m.state.holder[key]
				if n == nil || n.value != item.value {
					t.Fatalf("step %d value mismatch for %q", i/3, key)
				}
			}
		}
	})
}
