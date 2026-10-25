# Cache reliability implementation plan

**Goal:** Turn audit findings into regression tests and fix the cache without runtime dependencies or changing the Go 1.18 minimum.

**Architecture:** Keep existing public cache operations. Use a map, standard-library LRU list, and an indexed expiry heap with at most one record per item. A private state object owns locking and cleanup; a public handle owns its lifetime.

## Contract

- Preserve existing named cache methods and monitoring functions. `Get` returns a pointer to a shallow value copy; add `Lookup(key) (T, bool)`. Reference-type payloads remain the caller's synchronization responsibility. Remove the exposed embedded mutex so callers cannot interfere with internal locking.
- Capacity zero stores nothing. `Set` removes the previous TTL. With expiration disabled, `SetExpire` behaves like `Set`. With expiration enabled, TTL <= 0 deletes immediately; excessive TTLs saturate at the largest supported duration.
- Check TTL on reads and before eviction. Calculate deadlines and write values atomically. Remove expiry records on deletion, eviction, and clear.
- `Close` is concurrent-safe and idempotent: stop and join cleanup, unregister, release data, reject writes, make reads miss. `Clear` keeps the cache usable.
- Do not register process signals. The registry and worker retain state, never the public handle; a finalizer supplies best-effort cleanup for abandoned handles. Explicit `Close` is required for timely release.
- `CLEAR_ALL` clears a registry snapshot without holding the registry lock while locking caches.

## Tasks

- [x] Replace oversized, state-dependent tests with public API regressions. Observe behavioral failures on the original code.
- [x] Add clock-controlled TTL, lifecycle, mixed concurrency, process-signal, and structure-invariant tests. Implement the map/list/heap state and lifetime handling.
- [x] Add reference-model fuzzing, bounded benchmarks, executable examples, Make commands, and CI for Go 1.18 and stable.
- [x] Run formatting, vet, tests, shuffled race tests, coverage, fuzzing, benchmarks, and independent review; resolve substantive findings.

## Review focus

- Repeated renewal must not grow expiry metadata.
- Expired entries must not evict live entries.
- Close racing with global clear and cache operations must finish safely.
- Returned values must not expose internal scalar storage.
- The registry and cleaner must not retain abandoned handles; explicit close must synchronously release resources.

## Validation record (2026-10-25)

- RED: new public regressions failed on original capacity eviction, zero capacity, recency, pointer aliasing, and non-TTL SetExpire behavior. Lifecycle/clock test compilation also confirmed the missing interfaces before implementation.
- GREEN: Go 1.27.0 `make check` passed (vet, full tests, shuffled race suite x3). Additional lifecycle/concurrency race repetition passed; independent read-only review found no substantive issue and passed targeted race tests x25.
- Full-suite statement coverage was 100.0%; this is execution coverage, not proof of all scheduling/branch cases.
- A 15-second model fuzz run completed 305,160 executions without failures.
- `make bench` passed. Local scalar Lookup used 0 allocations and Get used 1 allocation, as expected for its snapshot pointer.
- Official checksum-verified Go 1.18.10 darwin/arm64: `CGO_ENABLED=0 go test -count=1 -timeout=60s ./...` and `go vet ./...` passed.
- Go 1.27 test binaries cross-compiled for Linux/amd64 and Windows/amd64 with CGO disabled; they were not executed on those operating systems.
- At the time of the pre-release local review, workflow YAML parsed successfully; remote CI had not run and changes had not yet been committed or pushed.

## Release preparation

- Publish the first fork release as `v0.1.0`; the remote repository had no tags or releases.
- Set the module and example imports to `github.com/infinitete/memcache` so the fork's tag can be consumed directly with Go modules.
- Document the independently reviewed Minor limitation: payload/closure back-references can prevent fallback finalization, so explicit Close remains required.
