# memcache

A bounded, concurrent, in-process LRU cache with optional TTLs. Requires Go 1.18
or newer and has no external runtime dependencies.

```sh
go get github.com/ohmountain/memcache
```

```go
package main

import (
    "fmt"
    "github.com/ohmountain/memcache"
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
  stores nothing. Successful `Lookup` reads and writes refresh LRU order; `Peek` does not.
- `Get(key) *T` returns nil for a miss, or a pointer to a **shallow value copy**.
  `Lookup(key) (T, bool)` distinguishes misses from stored zero/nil values without
  allocating a result pointer. `Peek(key) (T, bool)` returns the same shallow copy
  **without refreshing recency**, which makes it the read path for multi-core
  workloads that can accept approximate LRU. Updating a returned scalar cannot
  update the cache.
- `Len()` reports the physical entry count without purging expired entries; it
  never reads the clock. `Size()` reports live entries and purges expired ones.
- `Stats()` reports cumulative hits, misses, sets, deletes, evictions,
  expirations, peeks, physical entries, and capacity. Counters are updated under
  the shard lock and do not affect stored values.
- Referenced payloads (maps, slices, pointers, and references inside structs) are
  shared. Callers must synchronize mutations or copy payloads as appropriate.
- With TTL enabled, an entry is unavailable at or after its deadline, even before
  background cleanup runs. `Size()` counts live entries. Renewing a TTL replaces
  its previous deadline. `Set()` removes any previous TTL.
- An ordinary write does not sweep other keys' expired entries. It touches its
  target key and, when a shard is full, reclaims at most one extra entry: a due
  heap root if one exists, otherwise the LRU tail. Expired entries can therefore
  stay physically present until a read, a write that needs their slot, `Size()`,
  or background cleanup removes them. `Size()` purges in bounded chunks and
  returns the live count after the final chunk of each shard; concurrent writes
  can change the count while the call runs.
- `SetExpire` uses seconds. With TTL enabled, a TTL <= 0 deletes the key immediately;
  excessively large TTLs are clamped to the maximum `time.Duration` (about 292 years).
  With TTL disabled, `SetExpire` behaves like `Set` and ignores the TTL.
  `SetExpireDuration` is the same operation with sub-second `time.Duration`
  precision.
- `Clear()` empties a cache and keeps it usable. `Close()` empties and unregisters
  it, stops and joins background cleanup, and is safe to call repeatedly or
  concurrently. After Close, writes do nothing, reads miss, `Size()` is zero,
  and `Cap()` retains its configured value.
- `WithLRU(capacity, enableExpired)` keeps one exact global LRU. Use
  `WithOptions(Options{...})` to opt into `Shards > 1`: entries are routed by a
  seeded hash to per-shard map/list/heap sets, capacity is split evenly, and LRU
  becomes approximate (a global hit cannot protect a victim in another shard).
  Shards are rounded down to a power of two and capped so no shard is sized
  below about 64 slots — `Options.ShardSlots` lowers that floor for small
  caches (16 slots let a 1024-entry cache use 64 shards) at the cost of a
  coarser approximate LRU. Preallocation is off by default; `Preallocate`
  trades memory for fewer map-growth stalls.
- `Options.DisableStats` turns off the hit/miss/set/delete/eviction/expiration
  counters. Each counter update dirties a shard cache line that `Peek` otherwise
  only reads, so disabling them raises multicore read throughput (about 15% at
  24 threads with 64 shards); `Stats()` then reports only `Capacity` and
  `Entries`. Counters stay on by default.
- `Options.ApproximateTTLReads` speeds up reads of entries with a TTL. By
  default every such read checks the clock, so an expired entry misses
  immediately. With the option on, reads compare the deadline against the
  shard's last observed clock sample instead: about 57% faster single-thread
  reads (measured 39 → 16 ns), at the cost of serving an entry as a hit for up
  to one observation window past its deadline. Every TTL write, every cleaner
  round, and `Size()` publish fresh samples, so write-active shards observe
  expiry within one operation.
- The background cleaner starts on the first real TTL write, so a TTL-enabled
  cache that never stores a TTL holds no worker goroutine and none of its
  channels. An empty expiry heap has no timer at all.
- Use `WithLRU`/`WithOptions` to construct caches; the zero value is not
  initialized. Do not copy cache handles. Always call `Close` for timely cleanup.
  Abandoned handles have a best-effort GC finalizer, which is not a substitute for
  explicit lifecycle management. Payloads or callbacks that refer back to the
  cache handle can keep it reachable and prevent fallback finalization altogether;
  explicit `Close()` still releases it.
- `Lives()` counts registered caches. `CLEAR_ALL()` clears the caches it observes
  without closing them; registration is concurrent, so a cache created during the
  call may or may not be cleared.
- The library does not install process signal handlers. The application owns shutdown.

## Implementation and costs

Each shard has one private mutex covering its item map, intrusive LRU, and
lazy-deletion expiration heap. Nodes are pooled per shard; an eviction and
reinsert in steady state allocates nothing. The LRU links live inside the node;
for narrow payloads such as the int case below the 56-byte node rounds to the
64-byte size class, so a hit's recency write stays on the node's own cache line
rather than false-sharing with a neighbouring entry. Deadlines are int64
monotonic nanoseconds since construction. Expiry records carry their own
deadline snapshot and a generation counter: renewing pushes a fresh record
(the old one becomes stale) and cancelling or deleting just bumps the
generation, so neither sifts and neither writes a node. Stale records are
dropped when they surface at the heap root and swept by a bounded compaction
(when the array holds more than twice the entry count), so expiry metadata
cannot grow without bound.

Lookup and Peek are O(1) for unexpired entries; removing an expired entry costs
O(1) to cancel plus the amortized stale sweep. A write pushes a TTL record in
O(log n) worst case — monotone renewals append at the bottom and sift nothing,
measured 46 ns on the benchmark host — and cancels in O(1); it is otherwise
O(1) excluding lock acquisition. A new key on a full shard reclaims exactly one
entry in O(log n). `Len()` and `Cap()` are O(shards) and O(1). `Size()` purges in
1024-entry chunks per shard: total work is O(k log n) for k expired entries, but
no single shard lock is held for the whole sweep. `Stats()` is O(shards).

The default configuration measured about 83 bytes of live heap per resident int
entry at 100K entries with a pre-generated key set (cache-only, key strings
excluded); counting freshly allocated keys raises it to about 104 bytes. An idle
instance costs about 500 bytes whether or not TTLs are enabled, including its
registry entry, so 1000 idle TTL-enabled caches fit in about 0.47 MiB; creating
and closing one costs about 230 ns. One
exact LRU is preserved. A 24-thread host showed the
single-shard lookup path flattening, so opt-in sharding (typically 8-64 shards)
is available; it raises read throughput but loses a small amount of effective
capacity on uniform full-working-set workloads and changes LRU to per-shard.

Background cleanup keeps one worker per cache with a deadline, and none for a
TTL-enabled cache that never stored one. Across shards the worker sleeps until
the earliest root, reclaims an adaptive batch (64 up to 4096) per lock round,
yields between batches, and treats a timer event only as a hint to re-read the
roots. A heap change wakes it through a one-slot coalescing notification, but
only when the change can matter: a deadline that moves later never wakes the
worker, because the already-armed timer fires at the older deadline, re-reads the
real roots, and re-arms. Wakes are sent for an earlier root, for an emptied heap,
and for an empty-to-non-empty transition. Renewal is the common TTL write and it
only moves a deadline later, so a cache under sustained renewal keeps one sleeping
worker instead of one spinning on every shard lock.
Reclamation is best-effort — it is not a promise that payloads are freed within a
fixed number of milliseconds. Close stops the timer, waits for the worker, and is
safe to repeat. Capacity does not bound the size of user payloads or the number of
cache instances created by an application.

## Compatibility notes

The named cache methods remain available and `WithLRU` keeps its exact-LRU
default. `Peek`, `Len`, `Stats`, `SetExpireDuration`, and `WithOptions` are
additive. The formerly exposed embedded `sync.RWMutex` has been removed; external
`Lock`/`Unlock` calls must be removed. `Get` no longer exposes internal scalar
storage. `SetExpire` on a non-TTL cache stores the value. Zero-capacity behavior,
nonpositive TTLs, strict expiry on reads, and TTL removal on ordinary Set are
defined above. `Size()` still purges all due entries at its sampling instant, but
does so in chunks; concurrent writers can observe a slightly different count.

## Development

Install golangci-lint v2.15.0 (the version pinned in CI) using the
[official installer](https://golangci-lint.run/docs/welcome/install/local/).
For Linux/macOS:

```sh
curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/v2.15.0/install.sh \
  | sh -s -- -b "$(go env GOPATH)/bin" v2.15.0
export PATH="$(go env GOPATH)/bin:$PATH"
golangci-lint version
```

Run `make check` after every code change and before submitting it:

```sh
make check       # formatting, lint, vet, unit/integration tests, shuffled race tests
make lint        # lint all packages, including tests
make lint-fix    # apply supported lint/formatting fixes; review the diff afterward
make coverage    # report statement coverage; writes ignored coverage.out
make fuzz        # 15-second reference-model fuzz run, four workers
make bench       # bounded benchmark and allocation report
make bench-sharded # exact LRU vs opt-in sharded read path
```

Lint uses `.golangci.yml`: the standard linters plus durationcheck, errorlint,
and nolintlint, with gofmt/goimports formatting checks. GitHub Actions checks
the full codebase on every push and pull request. Run lint on a modern Go
toolchain supported by golangci-lint; the library still supports Go 1.18.
Use `GOLANGCI_LINT=/path/to/golangci-lint make lint` for a custom binary path.

The tests use only Go's testing tools. See [docs/testing.md](docs/testing.md) for
coverage, reproducibility, platform behavior, and direct Go commands.
