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
instances with a meaningful worker count (idle and lifecycle); always
disambiguate via the `case` field. Non-applicable keys are omitted — a
missing key never means zero.

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
GOBIN="$(mktemp -d)" go install golang.org/x/perf/cmd/benchstat@latest
"$(that-dir)/benchstat" old.log new.log
```

Measurement stability is checked by splitting one 10-repetition file into
two halves and comparing them with benchstat before believing any delta.

## Reports

- `2026-10-25-baseline.md` — the Stage A baseline frozen from an actual full
  collection; the authoritative comparison point for Stage B/C candidates.
