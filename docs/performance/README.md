# Performance evidence

This directory holds the reproducible performance baseline and the method
behind it. Stage A of the cache evolution only builds measurement tooling and
freezes a baseline; it changes no runtime policy and claims no optimization
win. Later stages compare candidates against these numbers on the same
machine, Go version, and CPU configuration.

## How to collect

```sh
make perf-quick                     # short working-tree measurements for development
make perf-baseline PERFSUITE=smoke   # verifies the pipeline only; NOT a baseline
make perf-baseline                   # full formal collection into a fresh directory
```

`scripts/perf-baseline.sh` refuses to write into an existing `--out`
directory, runs everything serially, and writes every
command, its log, and a SHA256SUMS inventory into a `manifest.txt`. On
Windows run the equivalent `go test` commands listed in the script's `full`
section directly.

`smoke` and `full` verify that the runtime files still match the frozen
runtime baseline commit (`12698ab…`). `quick` measures the current working
tree, including modified runtime files, and records whether they match the
baseline. Its manifest carries `acceptance_evidence=no`: the reduced matrix
is for development feedback, not a formal baseline or acceptance verdict.

Layered entries:

| Target | What runs |
| --- | --- |
| `make bench` | Ordinary 1K-key micro benchmarks once (200ms each) |
| `make bench-full` | Same benchmarks plus 100K/1M scenarios (`-cache-perf-full=true`), 10 repetitions, `-cpu=1,2,4,8` |
| `make bench-expiry` | First Set/Lookup/Size after a full-cache expiry event, one operation per fresh fixture (`-benchtime=1x`), 10 repetitions |
| `make perf-quick` | `BenchmarkCache` at the runtime-default CPU count, 100ms × 3; noop/steady/expiry-burst at 10K rps, 1s × 3; expiry latency only, no profiles |
| `make perf-baseline` | Full micro and expiry benchmark matrix plus open-loop scenarios, idle/lifecycle runs, and profiles |

`make perf-baseline PERFSUITE=quick` is equivalent to `make perf-quick`.
Both accept `PERFOUT=/tmp/<new-directory>`. A quick expiry sample still
preloads 100000 expiring entries and waits about one second for the first
deadline before its one-second load window.

### Measure expiry latency or reclamation independently

For `expiry-burst`, `expiry-spread`, and `size-burst`, select the measurement
with `-cache-perf-expiry-measure`:

| Value | Work performed | JSON evidence |
| --- | --- | --- |
| `both` (default) | Original independent latency run, then a separate monitor run | `load` contains unmonitored latency; `resources.reclaim_duration_ns` contains successful reclamation time |
| `latency` | One fixture and load, without the monitor run | `expiry_measurement="latency"`; no reclamation metric |
| `reclaim` | One monitor fixture and load, without the independent latency run | `expiry_measurement="reclaim"`; successful reclamation metric; `load` is all zero because monitor latencies are not latency evidence |

For example, collect just reclamation under the standard 3s load:

```sh
go test -run '^TestCachePerformance$' -count=1 -timeout=60s . -args \
  -cache-perf-case=expiry-burst -cache-perf-expiry-measure=reclaim \
  -cache-perf-out=/tmp/memcache-reclaim-001.json
```

Choose a fresh output path on each run. The non-default measurement values are
rejected for other scenarios. With `both`, the existing schema-1 JSON stays
unchanged (`expiry_measurement` is omitted), and the original snapshot-based
monitor, fixture sizes, and load configuration are retained.

## Comparing a candidate against a control

Stage B adds a comparison entry point that is strategy-neutral: the control and
candidate runtimes are exported from fixed commits and measured with one shared
test harness.

```sh
make perf-compare \
  PERFCONTROL=12698ab PERFCANDIDATE=<candidate-sha> PERFHARNESS=<harness-sha> \
  PERFROLE=original PERFSUITE=smoke PERFOUT=/tmp/memcache-compare-001
```

- All three commits and a fresh `PERFOUT` are required. A missing value, a
  non-commit SHA, or an existing `PERFOUT` fails before any evidence is
  written, and the Stage A baseline files are left untouched.
- Only root-level non-test `*.go` files plus `go.mod` come from the control and
  candidate commits. The working tree is never read, so an uncommitted
  candidate cannot leak into a measurement, and the current branch is never
  switched.
- Test code comes from the harness commit using a closed list:
  `benchmark_test.go`, `benchmark_expiration_test.go`,
  `benchmark_payload_test.go`, `workload_test.go`, `performance_load_test.go`,
  `performance_test.go`, `performance_resources_test.go`, `memcache_test.go`,
  `expiration_test.go`, `invariants_test.go`, `model_snapshot_test.go`,
  `model_state_test.go`, `history_checker_test.go`,
  `history_integration_test.go`. A helper that exists only in the candidate's
  commit fails the compile step instead of silently widening the harness. Both
  sides are compiled before anything is timed.
- `PERFROLE=original` requires the control's runtime blobs to be byte-identical
  to the frozen Stage A runtime (`12698ab…`) and records that check in the
  manifest. `PERFROLE=incremental` accepts B1 as the control but stamps the
  manifest `stage_b_conclusion=attribution-only`: such a run cannot produce a
  Stage B acceptance verdict.
- `PERFSUITE=smoke` runs one short micro benchmark and one noop load per side.
  It checks the pipeline only; never use it for a percentage verdict.
  `PERFSUITE=full` runs the Stage A matrix — micro benchmarks at `-cpu=1,2,4,8`,
  the first post-expiry operation, five open-loop scenarios × 1000/10000/50000,
  idle 1/100/1000, lifecycle, and separate-process profiles — as ten
  interleaved control/candidate pairs (AB/BA rotation), one subprocess per
  sample. Control and candidate never run concurrently. Idle and lifecycle
  scenarios run once per side, matching Stage A's cadence.
- `PERFSUITE=quick` runs `BenchmarkCache` at the runtime-default CPU count
  for 100ms and noop/steady/expiry-burst at 10K rps for 1s, as three serial
  AB/BA pairs. Expiry uses `latency` only; large tiers, idle, lifecycle, and
  profiles are omitted. Each side produces nine load JSON records. The
  manifest marks `acceptance_evidence=no` regardless of comparison role.
  Use a harness commit containing `-cache-perf-expiry-measure`; the script
  still exports all sources from the specified commits, never the working tree.
- Each sample keeps its exact command, exit code, and log; `manifest.txt` records
  the three full SHAs, per-file runtime and harness checksums, the environment,
  and the SHA256 of every exported file and scenario JSON. Each JSON's workload
  (action) SHA256 is recorded too, so a reader can confirm both sides replayed
  the same actions. Profiles run in separate processes and never enter a
  comparison table.
- Load accounting is enforced by the harness itself (`verifyLoadAccounting`): a
  broken run fails instead of emitting a plausible-looking JSON file.

## What a scenario JSON records

Every `TestCachePerformance` run writes one schema-1 JSON file (created with
O_EXCL — never overwritten): environment (Go version, GOOS/GOARCH,
GOMAXPROCS), the fixed seed (20261025), the complete `loadConfig`, the
workload summary and the SHA256 of the actually replayed action sequence,
the full `loadResult`, and a `resources` map. Durations are encoded as
integer nanoseconds throughout.

`expiry_measurement` is an optional schema-1 field for latency-only or
reclamation-only expiry runs (see the mode table above). An omitted field
on an expiry record means the original `both` measurement. Reclamation-only
records have no latency measurement; their zero `load` must not be treated
as a measured zero latency or throughput.

`loadResult` satisfies fixed accounting equations — check any file against
them before trusting it:

- Normal end: `attempted == planned == completed + dropped` and `unissued == 0`.
- Canceled: `planned == attempted + unissued` and `attempted == completed + dropped`.
- The `burst_*` fields follow the same equations restricted to requests
  planned inside `[start, start+burst_window)`.
- `all.samples == completed`; `set.*` summarizes only `Write` samples.

The fixed `resources` keys are case-dependent: `reclaim_duration_ns` means
expired-entry reclamation time in the expiry scenarios but total Close time
in `lifecycle`; `owned_workers` is recorded when the run owns cache
instances with a meaningful worker count (steady, idle, and lifecycle —
steady reports 0 because its TTL-less cache starts no cleaner); always
disambiguate via the `case` field. Non-applicable keys are omitted — a
missing key never means zero.

Idle records carry three `now()` instrumentation counters (each call costs one
atomic increment):

- `empty_heap_checks` — total calls since the cache was constructed. Kept for
  compatibility with Stage A/B1 records; it includes one deadline read per
  initial write and one startup observation per worker.
- `window_now_checks` — calls inside the 3s observation window. Startup
  observations can still race the window start, so a small nonzero value here
  is startup, not periodic waking.
- `tail_now_checks` — calls after the load finished (the settled part of the
  window). Any periodic wake would show up here; zero is the intended value
  for the earliest-deadline worker.

No counter alone proves "no timer exists": that proof is the fake-timer test
(`TestCleanerEmptyHeapSleeps`), which asserts no timer is created and no clock
is read for an empty heap.

The lifecycle bytes4k run additionally carries `resources_held`: the same
post-GC heap keys as `resources`, sampled while the payload references are
still pinned (`runtime.KeepAlive`), where `resources` holds the after-release
sample of the same heap. The distance between the two maps is the retained
payload footprint; the per-round logs behind them are preserved by running
the two lifecycle commands with `go test -v` (see `scripts/perf-baseline.sh`)
so the label logs carry the full trail, not just a final `ok`.

## Reading the numbers honestly

- Latency runs from the **planned** arrival time to completion: queueing and
  scheduler lag are included. A drop is reported as a drop, never hidden
  from the latency table by serving fewer requests.
- `noop` is the generator floor. A scenario whose P99 approaches the noop
  P99 at the same rate measures noise, not the cache; such a tier is marked
  low-signal and is not used for percentage acceptance later.
- `set.p99` with fewer than 100 samples is flagged low-sample in reports;
  the raw value is still kept.
- Compare only runs with the same machine, Go version, load configuration,
  and CPU count (`-cpu`). Comparing across different GOMAXPROCS values
  answers a different question.
- Expiry scenarios measure from the (earliest) deadline, after filling
  100000 expiring plus 1024 permanent entries, so no warm-up traffic evicts
  the target keys. Reclamation time comes from a separate monitor run using
  pure snapshots — `Size` never helps cleaning there.
- Idle runs count `now()` calls through instrumentation (one atomic increment
  per call). `empty_heap_checks` is the total since construction, kept for
  compatibility; `window_now_checks` and `tail_now_checks` separate startup
  observations from the settled window. The idle record therefore reports both
  the startup cost and whether anything wakes periodically, and it includes the
  instrumentation overhead.
- CPU, mutex, and heap profiles are collected in separate processes; numbers
  from profiled runs never fill the latency comparison tables.
- `benchstat` (external tool, never a dependency of this module) compares
  repeated benchmark files; install it into a scratch GOBIN:

```sh
bin="$(mktemp -d)"
GOBIN="$bin" go install golang.org/x/perf/cmd/benchstat@latest
"$bin/benchstat" old.log new.log
```

Measurement stability is checked by splitting one 10-repetition file into
two halves and comparing them with benchstat before believing any delta.

## Reports

- `2026-10-25-baseline.md` — the Stage A baseline frozen from an actual full
  collection; the authoritative comparison point for Stage B/C candidates.
- `2026-10-25-ttl-b1.md` — the B1 intermediate acceptance checkpoint against
  the frozen Stage A runtime. Foreground reclamation is bounded and the first
  post-expiry Set collapses from 7.21ms to 2.19µs, but the old ticker cleaner
  still full-purges, so the concentrated-expiry P99 and full-reclamation gates
  are not met and B1 is not a Stage B pass.
- `2026-10-25-ttl-b2.md` — the B2 Stage B acceptance report against A on the
  final runtime plus the B1→B2 attribution. Earliest-deadline scheduling with
  batches of 64 cuts the valid-tier burst Set P99 by ~99.3% (20/40-pair
  samples) and removes the idle periodic wake. A post-review fix removed
  scheduler work from the lookup hit path, closing the non-TTL and allocation
  gates. Reclamation is 1.52× the control under a common 1ms heap-length
  measurement; the harness's 10ms full-snapshot monitor reads 2.30–2.42× and is
  documented as resolution-limited. Every measured gate passes; the
  Linux/Windows matrix was not run, and that is the remaining acceptance item.
- `2026-10-26-optimization-opportunities.md` — a measured opportunity audit
  rather than an acceptance report. It profiles single-thread costs (lazy clock
  reads, intrusive LRU nodes, int64 deadlines), negative parallel scaling on a
  24-thread host, sharding tradeoffs, the cost of recency writes on the read
  path, `Size()` purge stalls, and idle TTL-instance overhead. Numbers come from
  an uncommitted `.perf-lab/` prototype and are scale evidence for a roadmap,
  not a Stage C verdict.
- `2026-10-26-optimization-implementation.md` — what was actually implemented
  from that audit, with before/after numbers (lazy clock, intrusive nodes with
  embedded links, int64 deadlines, bounded `Size`, opt-in sharding, `Peek`/`Len`/`Stats`,
  lazy cleaner) and the items deliberately left for a later policy/measurement
  stage. Its §2.1 records which original acceptance gates were met, missed
  (idle memory, high-core sharding), or not measured; footprint now passes at
  1K/100K under the audit's cache-only methodology, with the key-inclusive
  variant documented separately.
- `2026-10-26-embed-link-prototype.md` — controlled A/B of embedding the LRU
  links in `node` instead of a separate `lruLink`: cache-only footprint,
  key-inclusive footprint, single-thread paths, and 8/24-thread `Lookup`.
  Its result was adopted: the production layout now uses the embedded node.
- `2026-10-26-round2-optimizations.md` — the next measured round: deadline
  reads take `time.Since`'s monotonic fast path (TTL `Lookup` −28%, `Peek`
  −29%, `RenewTTL` −14%), the shard router uses an 8-byte-chunked seeded hash
  (sharded `Lookup` −28% at 16-byte keys to −59% at 64-byte keys), and
  `Options.DisableStats` removes the per-operation counter stores (24-thread
  64-shard `Lookup` −12%, `Set` −19%, `Peek` −15%). It also records the
  measured, unimplemented opportunities (per-instance memory, the lock-bound
  sharded read ceiling, opt-in eviction policies, TTL heap cost) and their
  evidence.
- `2026-10-26-round3-optimizations.md` — the third measured round: the cleaner is
  woken only when an earliest deadline moves earlier or a heap empties, so
  sustained TTL renewal no longer spins the worker on every shard lock
  (`RenewTTL` −12% at 24 threads; the worker leaves the CPU profile entirely);
  per-instance fixed cost drops 38–46% (`805.8 → 498.7 B` non-TTL,
  `910.4 → 496.0 B` TTL-idle, `10.4 → 6.4` objects, create+close `367 → 226 ns`),
  which finally meets the Stage 3 "1000 idle TTL caches ≤ 0.5 MB" gate at
  0.47 MiB; a 192-byte, 64-byte-aligned shard replaces the phase-dependent
  208-byte padding; and a concurrent second `Close` now waits for the first one
  to empty and unregister the cache. It also records the measured negative
  results (a shard-lock clock hoist that cost 27% on the default single-shard
  configuration and was reverted, RWMutex read paths, batch APIs, partition
  purge) and the deferred architecture items (approximate recency, floating
  per-shard capacity, the 64-slot shard floor).
- `2026-10-26-round3-audit.md` — the independent audit of round 3: every number in
  the round-3 report re-derived (per-instance footprint with an exact allocation
  counter: `803.5 → 488.5 B`, gate `0.880 → 0.452 MiB`), the cleaner wake filter
  exercised end-to-end through the public API on 1/4/64 shards, and the change
  attacked with mutation testing. It found that the `extends` deadline-direction
  comparison and the "never wake for a later root" policy were both unprotected
  by tests (a plausible simplification of either passed the whole suite), that
  the concurrent-`Close` regression test only detected the old bug in 14 of 20
  runs, and that the library panicked on 32-bit targets on an unaligned 64-bit
  atomic. All four are fixed here; the report numbers that were imprecise (net
  `+8 B` rather than `+16 B`, `InsertEvict` `+5.3%` in the independent-process
  set, a stale `ab.sh` output that reproduces a pre-revert state) are corrected
  in place.
- `2026-10-26-round4-audit.md` — the independent audit of round 4
  (`c9d50ea..f942819`): an adversarial trace of the lazy-deletion heap, the
  wake contract, the atomic shard size, and the approximate TTL clock found no
  Critical defect; gates, Go 1.18 compilation, layout offsets, and the headline
  benchmark (−57.6% TTL read) were independently reproduced. It found the plan-
  mandated public `Len` concurrency-contract doc was never written (the internal
  comment had it, the godoc users read did not), a stale layout-comment
  regression, and an undocumented-but-correct fix of two arithmetically
  impossible plan test-table rows; all are fixed here and the deviations are
  now recorded.
