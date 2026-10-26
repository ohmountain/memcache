package memcache

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestLenCompletesWhileShardLocked pins the lock-free read: Len must not
// acquire a shard mutex. The current lock-based Len blocks on the held lock
// and fails by timeout.
func TestLenCompletesWhileShardLocked(t *testing.T) {
	m := WithOptions[int](Options{Capacity: 64, Shards: 4, ShardSlots: 16})
	defer m.Close()
	for i := 0; i < 64; i++ {
		m.Set(strconv.Itoa(i), i)
	}
	sh := m.state.shards[0]
	sh.mu.Lock()
	defer sh.mu.Unlock()

	done := make(chan uint, 1)
	go func() {
		done <- m.Len()
	}()
	select {
	case n := <-done:
		if n == 0 || n > 64 {
			t.Fatalf("Len = %d, want within (0, 64]", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Len blocked on a held shard mutex; it must read atomically")
	}
}

// TestLenBoundedUnderChurn pins the read's consistency: Len never exceeds the
// capacity while writers churn, and settles to the physical count once they
// stop.
func TestLenBoundedUnderChurn(t *testing.T) {
	const capacity = 512
	m := WithOptions[int](Options{Capacity: capacity, Shards: 64, ShardSlots: 16})
	defer m.Close()
	for i := 0; i < capacity; i++ {
		m.Set(strconv.Itoa(i), i)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				m.Set(strconv.Itoa(w*1000000+i), i)
				m.Delete(strconv.Itoa(w*1000000 + i/2))
				i++
			}
		}(w)
	}
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case <-deadline:
			close(stop)
			wg.Wait()
			if got := m.Len(); got == 0 || got > capacity {
				t.Fatalf("settled Len = %d, want within (0, %d]", got, capacity)
			}
			return
		default:
			if got := m.Len(); got > capacity {
				close(stop)
				wg.Wait()
				t.Fatalf("Len = %d under churn, want at most %d", got, capacity)
			}
		}
	}
}
