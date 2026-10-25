# Testing the cache

No third-party test framework is required. The harness uses Go's `testing`,
race detector, coverage instrumentation, built-in fuzzing, and benchmarks.

## Regression coverage

| File | Behavior checked |
| --- | --- |
| `memcache_test.go` | Capacity, LRU read/update order, deleting every position, Clear/reuse, snapshots, zero and nil values |
| `invariants_test.go` | Map/list agreement, capacity bound, heap ordering and indices, one expiry record per item |
| `expiration_test.go` | Exact expiry boundaries, renewal/overwrite, delete/evict/clear then reinsert, TTL bounds, metadata retention |
| `lifecycle_test.go` | Close/unregister/join, global clear, collection of abandoned handles, real cleaner, process-signal isolation |
| `concurrency_test.go` | Same-key deletion, mixed operations, concurrent Close/global clear/registration, snapshot stability |
| `model_snapshot_test.go` | Lock-held snapshots that preserve expired physical entries, no aliasing, consistency under concurrent mutation |
| `model_state_test.go` | Independent reference model (map + recency slice), TTL-disabled and post-Close contracts, boundary/clone self-tests, large deterministic sequences |
| `fuzz_test.go` | Byte-encoded operations replayed against the model; observation uses snapshots only, so watching the cache never purges or refreshes |
| `history_checker_test.go` | Bounded serial-order search honoring real time; impossible reads, budget exhaustion, malformed events, final-state matching |
| `history_integration_test.go` | Recorded concurrent histories on a real cache replayed through the checker; renewal racing checkExpired |
| `workload_test.go` | Deterministic read/write mixes (positional ratios), distribution validation, default-seed behavior |
| `benchmark_*_test.go` | Named micro benchmarks: workloads, payloads, first post-expiry operation with `-benchtime=1x`, and the one-deadline expiry fixture test |
| `performance_load_test.go` | Open-loop generator: nearest-rank quantiles, overload drop accounting, scheduler lag, cancellation join, Set-only latency |
| `performance*_test.go` | Scenario evidence: JSON schema with O_EXCL writes, real expiry fixtures, instance/lifecycle resource rounds |
| `example_test.go` | Executable public-package examples |
| `benchmark_test.go` | Get/Lookup/update/TTL renewal and concurrent reads, with allocations reported |

The deterministic TTL tests inject a clock and disable the real cleaner. They
advance directly to deadlines, so a test does not pass merely because a long
sleep eventually permits cleanup. Separate integration tests exercise the real
cleaner and GC finalization with bounded five-second deadlines. Finalization
timing is deliberately not part of the public contract; use Close in application
code. The signal test sends SIGINT only to a dedicated child test process and is
skipped on Windows/Plan 9.

Tests close their own cache instances and do not depend on execution order. The
global-monitor tests must not use `t.Parallel`, because `CLEAR_ALL` intentionally
acts on other registered caches in the same process.

## Commands

```sh
go vet ./...
go test -count=1 -timeout=60s ./...
go test -race -shuffle=on -count=3 -timeout=120s ./...
go test -count=1 -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
go test -run='^$' -fuzz='^FuzzCacheModel$' -fuzztime=15s -parallel=4 .
go test -run='^$' -bench='^(BenchmarkCache|BenchmarkCacheWorkload|BenchmarkCachePayload)$' -benchmem -benchtime=200ms ./...
```

Benchmarks are layered: `make bench` runs the short 1K-key workloads;
`make bench-full` adds the 100K/1M scenarios behind `-cache-perf-full=true`
with ten repetitions across `-cpu=1,2,4,8`; `make bench-expiry` measures one
post-expiry operation per fresh fixture and therefore requires
`-benchtime=1x` — with an adaptive `b.N` it skips instead of refilling
100K/1M caches repeatedly. `TestCachePerformance` skips under a plain
`go test ./...`; the staged performance scenarios run explicitly:

```sh
go test -run='^TestCachePerformance$' -count=1 -timeout=60s . \
  -args -cache-perf-case=expiry-burst -cache-perf-out=/tmp/new-evidence.json
```

The output file is created with O_EXCL: evidence is written once and never
overwritten — pick a new path for every run instead of deleting old results.
`scripts/perf-baseline.sh` (Linux/macOS; Windows uses the equivalent
go-test commands from docs/performance/README.md) collects the whole bundle:

```sh
make perf-baseline PERFSUITE=smoke            # pipeline check, NOT a baseline
make perf-baseline                            # full formal collection
make perf-baseline PERFSUITE=full PERFOUT=/tmp/memcache-perf-002
```

To replay a recorded history or model failure, rerun with `-run` naming the
failing test and the seed or shuffle seed Go printed; scenario JSON records
carry the seed and workload SHA256 next to every measurement.

The race detector requires a supported platform and a C toolchain. Use a shuffle
seed printed by Go to replay ordering; use `-run` to isolate a regression. Fuzz
failures are saved by Go under `testdata/fuzz/FuzzCacheModel/`; retain a failing
input as a regression and rerun the command Go prints. To extend the search:

```sh
make fuzz FUZZTIME=60s FUZZWORKERS=4
```

Fuzz sequences are capped at 2048 bytes and caches at seven entries, making the
reference model cheap. Benchmarks use bounded caches, close every instance, and
report observations rather than imposing machine-dependent latency thresholds.
Statement coverage measures executed code; race/model/behavior checks are still
required and cannot be replaced by a coverage percentage.

CI runs vet, tests, and shuffled race checks on Go 1.18/Linux and stable Go on
Linux, macOS, and Windows. Stable Linux also checks formatting, coverage, and
fuzzing. Actions are pinned to immutable commits. Remote CI results require an
actual GitHub run; writing the workflow does not establish those results.
