#!/usr/bin/env bash
# Collect the reproducible memcache performance baseline.
#
# Usage: scripts/perf-baseline.sh [--suite smoke|full] [--out PATH]
#
# The output directory must not exist; evidence is never overwritten. Without
# --out a fresh mktemp directory outside the repository is used. --suite smoke
# verifies the pipeline only and is NOT a baseline; --suite full collects the
# repeated measurements (micro benchmarks x10, first-expiry operations x10,
# five load scenarios x three rates x ten repetitions, idle instance counts,
# lifecycle value types, and separate-process profiles) that the formal
# baseline report freezes. Only local collection happens here: nothing is
# downloaded and no data leaves the machine. Windows uses the equivalent
# go-test commands from docs/performance/README.md instead of this script.
set -euo pipefail

RUNTIME_BASELINE=12698ab9a095dcb130b5f60f8296950820f9c94b
RUNTIME_FILES=(memcache.go cache_state.go node.go monitor.go go.mod)
SEED=20261025

suite=full
out=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --suite)
      suite="${2-}"
      shift 2
      ;;
    --out)
      out="${2-}"
      shift 2
      ;;
    *)
      echo "unknown argument: $1" >&2
      exit 2
      ;;
  esac
done
case "$suite" in
  smoke | full) ;;
  *)
    echo "--suite must be smoke or full" >&2
    exit 2
    ;;
esac

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
if [ -z "$out" ]; then
  out="$(mktemp -d "${TMPDIR:-/tmp}/memcache-perf-XXXXXX")"
else
  if [ -e "$out" ]; then
    echo "refusing to use existing --out directory: $out" >&2
    exit 1
  fi
  mkdir -p "$out"
fi
out="$(cd "$out" && pwd)"
echo "results directory: $out (suite=$suite)"

cd "$repo_root"
if ! git diff --quiet "$RUNTIME_BASELINE" -- "${RUNTIME_FILES[@]}"; then
  echo "runtime files differ from baseline $RUNTIME_BASELINE; refusing to measure" >&2
  git diff "$RUNTIME_BASELINE" -- "${RUNTIME_FILES[@]}" >&2 || true
  exit 1
fi

manifest="$out/manifest.txt"
: >"$manifest"
record() { printf '%s\n' "$*" >>"$manifest"; }

record "suite=$suite"
record "started_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
record "git_head=$(git rev-parse HEAD)"
record "runtime_baseline=$RUNTIME_BASELINE"
record "runtime_files_match_baseline=yes"
record "git_dirty=$(if git diff --quiet && git diff --cached --quiet; then echo no; else echo yes; fi)"
record "seed=$SEED"
record "go_version=$(go version)"
# go env never modifies the environment; its output documents the toolchain.
go env GOOS GOARCH GOMAXPROCS GOVERSION CC >>"$manifest" 2>&1 || true
record "uname=$(uname -a)"
case "$(uname -s)" in
  Darwin)
    cpu_model="$(sysctl -n machdep.cpu.brand_string 2>/dev/null || echo unknown)"
    ;;
  Linux)
    cpu_model="$(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ *//' || true)"
    [ -n "$cpu_model" ] || cpu_model=unknown
    ;;
  *)
    cpu_model=unknown
    ;;
esac
record "cpu_model=$cpu_model"
record "scenarios_note=open-loop scenarios run at the machine-default GOMAXPROCS recorded above"

# run_logged <label> <command...>: records the exact command in the manifest,
# runs it, and keeps stdout+stderr in <label>.log inside the result directory.
run_logged() {
  local label="$1"
  shift
  record "command[${label}]=$*"
  "$@" >"$out/${label}.log" 2>&1
  record "exit[${label}]=0"
}

task_perf_dir="$out/perf"
mkdir -p "$task_perf_dir"

case "$suite" in
  smoke)
    run_logged bench-smoke go test -run='^$' -bench='^(BenchmarkCache|BenchmarkCacheWorkload)$' -benchmem -benchtime=200ms -count=1 -timeout=10m ./...
    run_logged perf-smoke go test -run '^TestCachePerformance$' -count=1 -timeout=5m . -args -cache-perf-case=noop -cache-perf-rate=1000 -cache-perf-duration=1s -cache-perf-out="$task_perf_dir/smoke-noop-001.json"
    ;;
  full)
    # 1. Micro benchmarks: 10 repetitions across CPU counts, large scenarios on.
    run_logged bench-full go test -run='^$' -bench='^(BenchmarkCache|BenchmarkCacheWorkload|BenchmarkCachePayload)$' -benchmem -cpu=1,2,4,8 -benchtime=200ms -count=10 -timeout=30m ./... -args -cache-perf-full=true
    # 2. First post-expiry operation per fresh fixture (one iteration each).
    run_logged bench-expiry go test -run='^$' -bench='^BenchmarkCacheExpiry$' -benchmem -benchtime=1x -count=10 -timeout=10m ./... -args -cache-perf-full=true
    # 3. Open-loop scenarios: five cases x three rates x ten repetitions.
    for case_name in noop steady expiry-burst expiry-spread size-burst; do
      for rate in 1000 10000 50000; do
        for rep in 1 2 3 4 5 6 7 8 9 10; do
          label="perf-${case_name}-rate${rate}-rep$(printf '%02d' "$rep")"
          run_logged "$label" go test -run '^TestCachePerformance$' -count=1 -timeout=10m . -args \
            -cache-perf-case="$case_name" -cache-perf-rate="$rate" -cache-perf-duration=3s \
            -cache-perf-workers=4 -cache-perf-queue=1024 -cache-perf-out="$task_perf_dir/${label}.json"
        done
      done
    done
    # 4. Idle instance scenarios: empty heap and one future deadline each.
    for case_name in idle-empty idle-future; do
      for n in 1 100 1000; do
        label="perf-${case_name}-n${n}"
        run_logged "$label" go test -run '^TestCachePerformance$' -count=1 -timeout=10m . -args \
          -cache-perf-case="$case_name" -cache-perf-instances="$n" -cache-perf-out="$task_perf_dir/${label}.json"
      done
    done
    # 5. Lifecycle: scalar values and independent 4KiB payloads.
    for value in int bytes4k; do
      label="perf-lifecycle-${value}"
      run_logged "$label" go test -run '^TestCachePerformance$' -count=1 -timeout=15m . -args \
        -cache-perf-case=lifecycle -cache-perf-value="$value" -cache-perf-out="$task_perf_dir/${label}.json"
    done
    # 6. Profiles in separate processes; the kept test binaries stay in the
    #    result directory so no .test files appear in the repository root.
    run_logged profile-cpu go test -run '^TestCachePerformance$' -count=1 -timeout=10m \
      -o "$out/memcache-cpu.test" -cpuprofile="$out/cpu.pprof" . -args \
      -cache-perf-case=steady -cache-perf-rate=10000 -cache-perf-out="$task_perf_dir/profile-cpu.json"
    run_logged profile-mutex go test -run '^TestCachePerformance$' -count=1 -timeout=10m \
      -o "$out/memcache-mutex.test" -mutexprofile="$out/mutex.pprof" . -args \
      -cache-perf-case=expiry-burst -cache-perf-rate=10000 -cache-perf-out="$task_perf_dir/profile-mutex.json"
    run_logged profile-heap go test -run '^TestCachePerformance$' -count=1 -timeout=15m \
      -o "$out/memcache-heap.test" -memprofile="$out/heap.pprof" . -args \
      -cache-perf-case=lifecycle -cache-perf-value=int -cache-perf-out="$task_perf_dir/profile-heap.json"
    ;;
esac

record "finished_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# SHA256 inventory of every produced file (checksums use the first available tool).
hash_tool=sha256sum
command -v sha256sum >/dev/null 2>&1 || hash_tool="shasum -a 256"
(
  cd "$out"
  find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 $hash_tool >SHA256SUMS
)
record "files_hashed=$(find "$out" -type f ! -name SHA256SUMS | wc -l | tr -d ' ')"
echo "done: $out"
