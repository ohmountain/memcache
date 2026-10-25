# Changelog

## v0.1.0 — 2026-10-25

First versioned release of the `github.com/infinitete/memcache` fork.

### Fixed

- Enforce cache capacity by removing evicted entries from every index.
- Keep LRU recency and deletion consistent using a standard-library list.
- Make deletion and TTL updates atomic and protect all mutable cache state.
- Replace stale expiration jobs with an indexed heap containing at most one
  expiration record per entry; renewal and key recreation cannot trigger old TTLs.
- Reject expired entries on reads and remove due entries before capacity eviction.
- Return shallow value snapshots from Get and synchronize Size and Lives.
- Remove process signal handlers and unconditional panic recovery.
- Release registration, contents, and cleaner resources through explicit Close;
  provide best-effort finalization for abandoned handles.

### Added

- `Lookup(key) (T, bool)` and concurrent-safe, idempotent `Close()`.
- Deterministic regression tests, clock-controlled TTL tests, structural
  invariants, concurrent lifecycle tests, subprocess signal tests, model fuzzing,
  executable examples, and allocation-reporting benchmarks.
- Make targets for checks, race tests, coverage, fuzzing, and benchmarks.
- CI for Go 1.18 on Linux and stable Go on Linux, macOS, and Windows.

### Migration

- Import this fork using `github.com/infinitete/memcache`.
- `Get` returns a pointer to a shallow value copy. Referenced maps, slices,
  pointers, and references within structs still require caller synchronization.
- The previously embedded `sync.RWMutex` is no longer exposed. Remove external
  cache `Lock`/`Unlock` calls.
- Zero-capacity caches store nothing. With TTL enabled, nonpositive TTLs delete
  immediately, ordinary Set clears a previous TTL, and expiry is enforced on reads.
- With TTL disabled, SetExpire behaves like Set instead of dropping the write.
- Always close caches when finished. Payloads or callbacks referencing a cache
  handle can prevent fallback GC finalization; explicit Close remains effective.

The minimum supported Go version remains 1.18. There are no external runtime
dependencies. Capacity limits entry count, not payload bytes.
