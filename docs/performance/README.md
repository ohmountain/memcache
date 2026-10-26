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
