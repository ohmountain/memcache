### memcache is a go package for memory cache

# Quick Start

**Install memcache**

```shell
go get -u -v github.com/ohmountain/memcache
```

**Use memcache**

```go
package main

import "time"
import "github.com/ohmountain/memcache"

func main() {
    // Use memcache without ttl
    cache := memcache.WithLRU(10000, false);
    cache.Set("Hello", "World");
    cache.Get("Hello")  // "World"
    cache.Get("world")  // nil 

    // Use memcache with ttl
    cache = memcache.WithLRU(10000, true)
    cache.SetExpire("Some", "Thing", 10)  // 10 seconds to live
    cache.Get("Some")  // "Thing"
    time.Sleep(11 * time.Second)
    cache.Get("Some")  // nil

    // Or delete
    cache.Set("Haha", "Huhu")
    cache.Delete("Haha") 
}
```

**Difference between TTL and Non-TTL**

If ttl is not used, it is just an LRU memory buffer; if ttl is used, then it adds a mechanism of expiration by time on the basis of LRU, which means that even if the LRU mechanism does not take effect, the buffer will automatically clean up expired k/v.
