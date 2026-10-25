# Performance evidence

This directory holds the reproducible performance baseline and the method
behind it. Stage A of the cache evolution only builds measurement tooling and
freezes a baseline; it changes no runtime policy and claims no optimization
win. Later stages compare candidates against these numbers on the same
machine, Go version, and CPU configuration.

## How to collect

```sh
make perf-baseline PERFSUITE=smoke   # verifies the pipeline only; NOT a baseline
make perf-baseline                   # full formal collection into a fresh directory
```

`scripts/perf-baseline.sh` refuses to write into an existing `--out`
directory, verifies the runtime files still match the frozen runtime
baseline commit (`12698ab…`), runs everything serially, and writes every
command, its log, and a SHA256SUMS inventory into a `manifest.txt`. On
Windows run the equivalent `go test` commands listed in the script's `full`
section directly.

Layered entries:

| Target | What runs |
| --- | --- |
| `make bench` | Ordinary 1K-key micro benchmarks once (200ms each) |
| `make bench-full` | Same benchmarks plus 100K/1M scenarios (`-cache-perf-full=true`), 10 repetitions, `-cpu=1,2,4,8` |
| `make bench-expiry` | First Set/Lookup/Size after a full-cache expiry event, one operation per fresh fixture (`-benchtime=1x`), 10 repetitions |
| `make perf-baseline` | Everything above plus the open-loop scenarios, idle/lifecycle runs, and profiles |

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
- Idle runs count `empty_heap_checks` through an instrumented `now()`
  (one atomic increment per call). They honestly report today's ticker
  behavior including that instrumentation overhead; Stage A does not claim
  "no periodic wake-up" — proving that is Stage B2's acceptance work.
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
