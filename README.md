# memcache

A bounded, concurrent, in-process LRU cache with optional TTLs. Requires Go 1.18
or newer and has no external runtime dependencies.

```sh
go get github.com/infinitete/memcache@v0.1.0
```

```go
package main

import (
    "fmt"
    "github.com/infinitete/memcache"
)

func main() {
    cache := memcache.WithLRU[string](10000, true)
    defer cache.Close()

    cache.Set("hello", "world")
    value, ok := cache.Lookup("hello")
    fmt.Println(value, ok) // world true

    cache.SetExpire("temporary", "value", 10) // TTL in seconds
    cache.Delete("temporary")
}
```

## Behavior

- Capacity limits the number of entries, not their total byte size. Zero capacity
  stores nothing. Successful reads and writes refresh LRU order.
- `Get(key) *T` returns nil for a miss, or a pointer to a **shallow value copy**.
  `Lookup(key) (T, bool)` distinguishes misses from stored zero/nil values without
  allocating a result pointer. Updating the returned scalar cannot update the cache.
- Referenced payloads (maps, slices, pointers, and references inside structs) are
  shared. Callers must synchronize mutations or copy payloads as appropriate.
- With TTL enabled, an entry is unavailable at or after its deadline, even before
  background cleanup runs. `Size()` counts live entries. Renewing a TTL replaces
  its previous deadline. `Set()` removes any previous TTL.
- `SetExpire` uses seconds. With TTL enabled, a TTL <= 0 deletes the key immediately;
  excessively large TTLs are clamped to the maximum `time.Duration` (about 292 years).
  With TTL disabled, `SetExpire` behaves like `Set` and ignores the TTL.
- `Clear()` empties a cache and keeps it usable. `Close()` empties and unregisters
  it, stops and joins background cleanup, and is safe to call repeatedly or
  concurrently. After Close, writes do nothing, reads miss, `Size()` is zero,
  and `Cap()` retains its configured value.
- Use `WithLRU` to construct caches; the zero value is not initialized. Do not copy
  cache handles. Always call `Close` for timely cleanup. Abandoned handles have a
  best-effort GC finalizer, which is not a substitute for explicit lifecycle management.
  Payloads or callbacks that refer back to the cache handle can keep it reachable
  and prevent fallback finalization altogether; explicit `Close()` still releases it.
- `Lives()` counts registered caches. `CLEAR_ALL()` clears a snapshot of registered
  caches without closing them. Caches registered after that snapshot are unaffected.
- The library does not install process signal handlers. The application owns shutdown.

## Implementation and costs

One private mutex protects the item map, standard-library LRU list, and indexed
expiration heap. There is at most one expiry record per live expiring entry;
deleting, evicting, renewing, or clearing entries cannot accumulate stale records.
Deadlines retain Go's monotonic time information.

Lookup is O(1) for unexpired entries; removing an expired entry costs O(log n).
Non-expiring mutations are O(1) excluding due-expiry purges. Expiring mutations
cost O(log n). `Set`, `SetExpire`, and `Size` also purge due entries in
O(k log n), where k entries have expired. An idle cleaner checks only the earliest
deadline every 100ms. Each positive-capacity TTL cache owns one cleaner goroutine;
Close waits for it to stop. Capacity does not bound the size of user payloads or
the number of cache instances created by an application.

## Compatibility notes

This fork is published as `github.com/infinitete/memcache`. Consumers migrating
from `github.com/ohmountain/memcache` must update their module dependency and
imports to obtain this release.

The named cache methods remain available. The formerly exposed embedded
`sync.RWMutex` has been removed; external `Lock`/`Unlock` calls must be removed.
`Get` no longer exposes internal scalar storage. `SetExpire` on a non-TTL cache
now stores the value. Zero-capacity behavior, nonpositive TTLs, strict expiry on
reads, and TTL removal on ordinary Set are defined above.

## Development

```sh
make check       # formatting, vet, unit/integration tests, shuffled race tests
make coverage    # report statement coverage; writes ignored coverage.out
make fuzz        # 15-second reference-model fuzz run, four workers
make bench       # bounded benchmark and allocation report
```

The tests use only Go's testing tools. See [docs/testing.md](docs/testing.md) for
coverage, reproducibility, platform behavior, and direct Go commands.
