package memcache

import (
	"fmt"
	"sync"
	"testing"
)

func TestConcurrentDeleteSameKey(t *testing.T) {
	m := testCache(t, 1, false)
	for round := 0; round < 100; round++ {
		m.Set("k", round)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; m.Delete("k") }()
		}
		close(start)
		wg.Wait()
		if m.Size() != 0 {
			t.Fatalf("round %d: size=%d after concurrent deletes", round, m.Size())
		}
		assertInvariants(t, m)
	}
}

func TestConcurrentOperations(t *testing.T) {
	m := testCache(t, 32, true)
	m.Set("shared", 1)
	snapshot := m.Get("shared")
	var wg sync.WaitGroup
	start := make(chan struct{})
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			for i := 0; i < 1000; i++ {
				key := fmt.Sprint((i + worker) % 64)
				switch (i + worker) % 10 {
				case 0:
					m.Set(key, i)
				case 1:
					m.SetExpire(key, i, 60)
				case 2:
					m.Get(key)
				case 3:
					m.Lookup(key)
				case 4:
					m.Delete(key)
				case 5:
					m.Size()
					m.Cap()
					Lives()
				case 6:
					m.Clear()
				case 7:
					m.checkExpired()
				case 8:
					m.Set("shared", i)
				case 9:
					if *snapshot != 1 {
						t.Error("snapshot changed")
					}
				}
			}
		}(worker)
	}
	close(start)
	wg.Wait()
	assertInvariants(t, m)
}

func TestCloseAndGlobalClearConcurrentWithOperations(t *testing.T) {
	before := Lives()
	m := testCache(t, 16, true)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			for i := 0; i < 100; i++ {
				switch worker {
				case 0:
					m.Set("k", i)
					m.SetExpire("ttl", i, 1)
				case 1:
					m.Get("k")
					m.Lookup("ttl")
				case 2:
					m.Delete("k")
					m.Size()
				case 3:
					m.Clear()
					m.Cap()
				case 4:
					CLEAR_ALL()
					Lives()
				case 5:
					temporary := WithLRU[int](1, i%2 == 0)
					temporary.Set("k", i)
					temporary.Close()
				default:
					m.Close()
				}
			}
		}(worker)
	}
	close(start)
	wg.Wait()
	m.Close()
	if Lives() != before || m.Size() != 0 {
		t.Fatal("concurrent Close leaked cache registration or data")
	}
	assertInvariants(t, m)
}
