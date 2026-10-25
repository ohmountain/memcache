package memcache_test

import (
	"fmt"

	"github.com/infinitete/memcache"
)

func Example() {
	cache := memcache.WithLRU[string](2, true)
	defer cache.Close()
	cache.Set("hello", "world")
	value, ok := cache.Lookup("hello")
	fmt.Println(value, ok)
	cache.SetExpire("temporary", "value", 0)
	fmt.Println(cache.Get("temporary") == nil)
	// Output:
	// world true
	// true
}

func ExampleMemcache_Get() {
	cache := memcache.WithLRU[int](1, false)
	defer cache.Close()
	cache.Set("k", 1)
	snapshot := cache.Get("k")
	cache.Set("k", 2)
	fmt.Println(*snapshot, *cache.Get("k"))
	// Output: 1 2
}
