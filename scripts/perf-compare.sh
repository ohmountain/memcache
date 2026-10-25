#!/usr/bin/env bash
# Compare a control and a candidate cache runtime from fixed git commits.
#
# Usage:
#   scripts/perf-compare.sh --control <commit> --candidate <commit> \
#     --harness <commit> --comparison-role original|incremental \
#     --suite smoke|full --out <new-dir>
#
# The measurement contract is strategy-neutral and closed:
#   * Runtime source (every root *.go that is not a _test.go file, plus go.mod)
#     comes from the side's own commit. Nothing from the working tree is read,
#     so an uncommitted candidate can never leak into a measurement.
#   * Test/measurement code comes from --harness, using the fixed HARNESS_FILES
#     list below. Files outside the list are not copied, so a test-only helper
#     that only exists in the candidate's commit is a compile error, not a
#     silent extra dependency. history_checker_test.go and
#     history_integration_test.go are in the list because model_state_test.go
#     needs executeRecordedOp from them at the frozen baseline.
#   * --comparison-role original verifies the control runtime is byte-identical
#     to the frozen Stage A runtime, so its numbers can feed a Stage B verdict.
#     --comparison-role incremental accepts B1 as the control for attribution
#     only; the manifest says so and such a run must not be reported as a
#     Stage B acceptance result.
#   * --suite smoke only proves the pipeline. It is not acceptance evidence.
#     --suite full collects the Stage A matrix; repeated measurements run as
#     10 interleaved control/candidate pairs, one subprocess each, never
#     concurrently.
#
# Everything is exported into <out>/scratch (a fresh directory per run). An
# existing --out is refused: evidence is never overwritten. Profiles run in
# their own processes and are excluded from the comparison tables. Load
# accounting is enforced inside the harness by verifyLoadAccounting, so a
# broken run fails instead of producing a plausible-looking JSON file.
set -euo pipefail

ORIGINAL_RUNTIME=12698ab9a095dcb130b5f60f8296950820f9c94b
SEED=20261025
HARNESS_FILES=(
  benchmark_test.go
  benchmark_expiration_test.go
  benchmark_payload_test.go
  workload_test.go
  performance_load_test.go
  performance_test.go
  performance_resources_test.go
  memcache_test.go
  expiration_test.go
  invariants_test.go
  model_snapshot_test.go
  model_state_test.go
  history_checker_test.go
  history_integration_test.go
)
# Repeated measurements use 10 control/candidate pairs with AB/BA rotation.
# Idle and lifecycle scenarios mirror Stage A, which sampled each once.
REPETITIONS=10

die() {
  echo "perf-compare: $*" >&2
  exit 1
}

usage() {
  sed -n '3,7p' "$0" | sed 's/^# \{0,1\}//' >&2
  exit 2
}

control=""
candidate=""
harness=""
role=""
suite=""
out=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --control) control="${2-}"; shift 2 ;;
    --candidate) candidate="${2-}"; shift 2 ;;
    --harness) harness="${2-}"; shift 2 ;;
    --comparison-role) role="${2-}"; shift 2 ;;
    --suite) suite="${2-}"; shift 2 ;;
    --out) out="${2-}"; shift 2 ;;
    -h | --help) usage ;;
    *) die "unknown argument: $1" ;;
  esac
done

[ -n "$control" ] || die "--control is required"
[ -n "$candidate" ] || die "--candidate is required"
[ -n "$harness" ] || die "--harness is required"
[ -n "$role" ] || die "--comparison-role is required"
[ -n "$suite" ] || die "--suite is required"
[ -n "$out" ] || die "--out is required"
case "$role" in
  original | incremental) ;;
  *) die "--comparison-role must be original or incremental, got '$role'" ;;
esac
case "$suite" in
  smoke | full) ;;
  *) die "--suite must be smoke or full, got '$suite'" ;;
esac

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

# Resolve every SHA to a commit before touching the filesystem, so a refusal
# leaves no output directory behind to block a retry with the same --out.
resolve_commit() {
  local name="$1" value="$2" full
  full="$(git rev-parse --verify --quiet "${value}^{commit}" 2>/dev/null)" ||
    die "$name is not a git commit: '$value'"
  printf '%s\n' "$full"
}
control_sha="$(resolve_commit --control "$control")"
candidate_sha="$(resolve_commit --candidate "$candidate")"
harness_sha="$(resolve_commit --harness "$harness")"

if [ -e "$out" ]; then
  die "refusing to use existing --out path: $out (evidence is never overwritten)"
fi

# hash_*: sha256 with whichever tool this machine has. Reading blobs straight
# out of a commit keeps a dirty working tree from influencing the comparison.
if command -v sha256sum >/dev/null 2>&1; then
  hash_stdin() { sha256sum | awk '{ print $1 }'; }
  hash_file() { sha256sum "$1" | awk '{ print $1 }'; }
else
  hash_stdin() { shasum -a 256 | awk '{ print $1 }'; }
  hash_file() { shasum -a 256 "$1" | awk '{ print $1 }'; }
fi
sha_of() { git show "$1:$2" | hash_stdin; }

# runtime_files prints the root-level runtime Go files and go.mod at a commit.
# The caller verifies that go.mod is present.
runtime_files() {
  git ls-tree -r --name-only "$1" | awk -F/ 'NF == 1 && /\.go$/ && !/_test\.go$/ { print }'
  printf 'go.mod\n'
}

export_runtime() {
  local commit="$1" dir="$2" file list="$3"
  : >"$list"
  mkdir -p "$dir"
  while IFS= read -r file; do
    git cat-file -e "$commit:$file" 2>/dev/null ||
      die "commit $commit is missing runtime file $file"
    git show "$commit:$file" >"$dir/$file"
    printf '%s\n' "$file" >>"$list"
  done < <(runtime_files "$commit")
  grep -qx 'go.mod' "$list" || die "commit $commit has no go.mod"
}

export_harness() {
  local commit="$1" dir="$2" file
  mkdir -p "$dir"
  for file in "${HARNESS_FILES[@]}"; do
    git show "$commit:$file" >"$dir/$file" 2>/dev/null ||
      die "harness commit $commit is missing $file"
  done
}

mkdir -p "$out"
out="$(cd "$out" && pwd)"
scratch="$out/scratch"
results="$out/results"
mkdir -p "$scratch/control" "$scratch/candidate" "$results/control" "$results/candidate"
manifest="$out/manifest.txt"
: >"$manifest"
record() { printf '%s\n' "$*" >>"$manifest"; }

control_src="$scratch/control-src"
candidate_src="$scratch/candidate-src"
harness_src="$scratch/harness-src"
export_runtime "$control_sha" "$control_src" "$scratch/control.runtime-files"
export_runtime "$candidate_sha" "$candidate_src" "$scratch/candidate.runtime-files"
export_harness "$harness_sha" "$harness_src"
cp "$harness_src"/*.go "$scratch/control/"
cp "$harness_src"/*.go "$scratch/candidate/"
cp "$control_src"/*.go "$control_src/go.mod" "$scratch/control/"
cp "$candidate_src"/*.go "$candidate_src/go.mod" "$scratch/candidate/"

record "suite=$suite"
record "comparison_role=$role"
case "$role" in
  original) record "stage_b_conclusion=eligible (control is the frozen Stage A runtime)" ;;
  incremental) record "stage_b_conclusion=attribution-only; not a Stage B acceptance verdict" ;;
esac
record "started_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
record "control_commit=$control_sha"
record "candidate_commit=$candidate_sha"
record "harness_commit=$harness_sha"
record "original_runtime=$ORIGINAL_RUNTIME"
record "seed=$SEED"
record "repetitions=$REPETITIONS"
record "interleave=AB/BA rotation; control and candidate never run concurrently"
record "idle_lifecycle_samples=single pair (Stage A sampled these once; not percentage-verdict inputs)"
record "git_head=$(git rev-parse HEAD)"
record "git_dirty=$(if git diff --quiet && git diff --cached --quiet; then echo no; else echo yes; fi)"
record "go_version=$(go version)"
record "env_goos=$(go env GOOS)"
record "env_goarch=$(go env GOARCH)"
record "env_go_version=$(go env GOVERSION)"
record "gomaxprocs_env=${GOMAXPROCS-<unset: runtime default = cpu_count>}"
case "$(uname -s)" in
  Darwin) cpu_count="$(sysctl -n hw.ncpu 2>/dev/null || echo unknown)" ;;
  Linux) cpu_count="$(nproc 2>/dev/null || echo unknown)" ;;
  *) cpu_count=unknown ;;
esac
record "cpu_count=$cpu_count"
record "uname=$(uname -a)"
case "$(uname -s)" in
  Darwin) cpu_model="$(sysctl -n machdep.cpu.brand_string 2>/dev/null || echo unknown)" ;;
  Linux)
    cpu_model="$(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ *//' || true)"
    [ -n "$cpu_model" ] || cpu_model=unknown
    ;;
  *) cpu_model=unknown ;;
esac
record "cpu_model=$cpu_model"

# The original role's control must be the exact frozen runtime. Compare blob
# hashes, not just file names: a same-named file with different content would
# silently invalidate the comparison.
if [ "$role" = original ]; then
  diff <(runtime_files "$control_sha" | sort) <(runtime_files "$ORIGINAL_RUNTIME" | sort) >/dev/null ||
    die "control runtime file set does not match the frozen Stage A runtime $ORIGINAL_RUNTIME"
  while IFS= read -r file; do
    [ "$(sha_of "$control_sha" "$file")" = "$(sha_of "$ORIGINAL_RUNTIME" "$file")" ] ||
      die "control runtime file $file differs from the frozen Stage A runtime $ORIGINAL_RUNTIME"
  done < <(runtime_files "$ORIGINAL_RUNTIME")
  record "original_runtime_check=control matches $ORIGINAL_RUNTIME"
else
  record "original_runtime_check=skipped (incremental comparison)"
fi

while IFS= read -r file; do
  record "runtime[control/$file]=$(sha_of "$control_sha" "$file")"
done <"$scratch/control.runtime-files"
while IFS= read -r file; do
  record "runtime[candidate/$file]=$(sha_of "$candidate_sha" "$file")"
done <"$scratch/candidate.runtime-files"
for file in "${HARNESS_FILES[@]}"; do
  record "harness[/$file]=$(sha_of "$harness_sha" "$file")"
done

# Both sides must compile against the same harness before anything is timed.
# A harness file outside HARNESS_FILES that a listed test needs shows up here
# as an unresolved symbol; the closed list is the contract, so this is a hard
# failure rather than a silent extra dependency.
for side in control candidate; do
  log="$results/compile-$side.log"
  record "command[compile-$side]=go test -run '^\$' -count=1 . (run in scratch/$side)"
  rc=0
  (cd "$scratch/$side" && go test -run '^$' -count=1 .) >"$log" 2>&1 || rc=$?
  record "exit[compile-$side]=$rc"
  if [ "$rc" -ne 0 ]; then
    cat "$log" >&2
    die "$side does not compile with the fixed harness set (see $log)"
  fi
done

run_logged() {
  local label="$1" dir="$2"
  shift 2
  record "command[${label}]=$* (run in $(basename "$dir"))"
  local rc=0
  (cd "$dir" && "$@") >"$results/${label}.log" 2>&1 || rc=$?
  record "exit[${label}]=$rc"
  if [ "$rc" -ne 0 ]; then
    cat "$results/${label}.log" >&2
    die "$label exited $rc"
  fi
}

# run_pair_once runs one control and one candidate sample.
run_pair_once() {
  local label="$1"
  shift
  local control_cmd=()
  while [ "$1" != "--" ]; do
    control_cmd+=("$1")
    shift
  done
  shift
  run_logged "${label}-control" "$scratch/control" "${control_cmd[@]}"
  run_logged "${label}-candidate" "$scratch/candidate" "$@"
}

# run_pair runs N interleaved pairs; AB on odd pairs, BA on even pairs.
run_pair() {
  local label="$1"
  shift
  local control_cmd=()
  while [ "$1" != "--" ]; do
    control_cmd+=("$1")
    shift
  done
  shift
  local candidate_cmd=("$@")
  local pair rep
  for pair in $(seq 1 "$REPETITIONS"); do
    rep="$(printf '%02d' "$pair")"
    if [ $((pair % 2)) -eq 1 ]; then
      run_logged "${label}-rep${rep}-control" "$scratch/control" "${control_cmd[@]}"
      run_logged "${label}-rep${rep}-candidate" "$scratch/candidate" "${candidate_cmd[@]}"
    else
      run_logged "${label}-rep${rep}-candidate" "$scratch/candidate" "${candidate_cmd[@]}"
      run_logged "${label}-rep${rep}-control" "$scratch/control" "${control_cmd[@]}"
    fi
  done
}

perf_json() { printf '%s/%s/%s.json\n' "$results" "$1" "$2"; }

run_perf_case() {
  local side="$1" case_name="$2" rate="$3" label="$4" rep="$5"
  local json
  json="$(perf_json "$side" "${label}-rep${rep}")"
  run_logged "${label}-rep${rep}-${side}" "$scratch/$side" \
    go test -run '^TestCachePerformance$' -count=1 -timeout=10m . -args \
    -cache-perf-case="$case_name" -cache-perf-rate="$rate" -cache-perf-duration=3s \
    -cache-perf-workers=4 -cache-perf-queue=1024 -cache-perf-out="$json"
}

case "$suite" in
  smoke)
    record "smoke_note=pipeline only; never use these numbers for percentage acceptance"
    run_pair_once bench-smoke \
      go test -run='^$' -bench='^BenchmarkCache$' -benchmem -benchtime=10ms -count=1 -timeout=10m ./... -- \
      go test -run='^$' -bench='^BenchmarkCache$' -benchmem -benchtime=10ms -count=1 -timeout=10m ./...
    for side in control candidate; do
      json="$(perf_json "$side" smoke-noop)"
      run_logged "perf-smoke-noop-$side" "$scratch/$side" \
        go test -run '^TestCachePerformance$' -count=1 -timeout=5m . -args \
        -cache-perf-case=noop -cache-perf-rate=1000 -cache-perf-duration=1s -cache-perf-out="$json"
    done
    ;;
  full)
    # 1. Micro benchmarks: the Stage A matrix, ten interleaved pairs per CPU count.
    for cpu in 1 2 4 8; do
      run_pair "bench-cpu${cpu}" \
        go test -run='^$' -bench='^(BenchmarkCache|BenchmarkCacheWorkload|BenchmarkCachePayload)$' -benchmem -cpu="$cpu" -benchtime=200ms -count=1 -timeout=45m ./... -args -cache-perf-full=true -- \
        go test -run='^$' -bench='^(BenchmarkCache|BenchmarkCacheWorkload|BenchmarkCachePayload)$' -benchmem -cpu="$cpu" -benchtime=200ms -count=1 -timeout=45m ./... -args -cache-perf-full=true
    done
    # 2. First operation after a whole-cache expiry event.
    run_pair bench-expiry \
      go test -run='^$' -bench='^BenchmarkCacheExpiry$' -benchmem -benchtime=1x -count=1 -timeout=20m ./... -args -cache-perf-full=true -- \
      go test -run='^$' -bench='^BenchmarkCacheExpiry$' -benchmem -benchtime=1x -count=1 -timeout=20m ./... -args -cache-perf-full=true
    # 3. Five open-loop scenarios at three rates, ten interleaved pairs each.
    for case_name in noop steady expiry-burst expiry-spread size-burst; do
      for rate in 1000 10000 50000; do
        label="perf-${case_name}-rate${rate}"
        for pair in $(seq 1 "$REPETITIONS"); do
          rep="$(printf '%02d' "$pair")"
          if [ $((pair % 2)) -eq 1 ]; then
            run_perf_case control "$case_name" "$rate" "$label" "$rep"
            run_perf_case candidate "$case_name" "$rate" "$label" "$rep"
          else
            run_perf_case candidate "$case_name" "$rate" "$label" "$rep"
            run_perf_case control "$case_name" "$rate" "$label" "$rep"
          fi
        done
      done
    done
    # 4. Idle instances: empty heap and one future deadline, 1/100/1000.
    for case_name in idle-empty idle-future; do
      for n in 1 100 1000; do
        label="perf-${case_name}-n${n}"
        run_pair_once "$label" \
          go test -run '^TestCachePerformance$' -count=1 -timeout=10m . -args \
          -cache-perf-case="$case_name" -cache-perf-instances="$n" -cache-perf-out="$(perf_json control "$label")" -- \
          go test -run '^TestCachePerformance$' -count=1 -timeout=10m . -args \
          -cache-perf-case="$case_name" -cache-perf-instances="$n" -cache-perf-out="$(perf_json candidate "$label")"
      done
    done
    # 5. Lifecycle: scalar and independent 4KiB payloads, verbose so the
    #    per-round heap trail survives in the label log.
    for value in int bytes4k; do
      label="perf-lifecycle-${value}"
      run_pair_once "$label" \
        go test -run '^TestCachePerformance$' -count=1 -v -timeout=15m . -args \
        -cache-perf-case=lifecycle -cache-perf-value="$value" -cache-perf-out="$(perf_json control "$label")" -- \
        go test -run '^TestCachePerformance$' -count=1 -v -timeout=15m . -args \
        -cache-perf-case=lifecycle -cache-perf-value="$value" -cache-perf-out="$(perf_json candidate "$label")"
    done
    # 6. Profiles: separate processes, kept out of every comparison table.
    record "profiles=separate processes; excluded from comparison statistics"
    for side in control candidate; do
      run_logged "profile-cpu-$side" "$scratch/$side" go test -run '^TestCachePerformance$' -count=1 -timeout=10m \
        -o "$out/${side}-cpu.test" -cpuprofile="$out/${side}-cpu.pprof" . -args \
        -cache-perf-case=steady -cache-perf-rate=10000 -cache-perf-out="$(perf_json "$side" profile-cpu)"
      run_logged "profile-mutex-$side" "$scratch/$side" go test -run '^TestCachePerformance$' -count=1 -timeout=10m \
        -o "$out/${side}-mutex.test" -mutexprofile="$out/${side}-mutex.pprof" . -args \
        -cache-perf-case=expiry-burst -cache-perf-rate=10000 -cache-perf-out="$(perf_json "$side" profile-mutex)"
      run_logged "profile-heap-$side" "$scratch/$side" go test -run '^TestCachePerformance$' -count=1 -timeout=15m \
        -o "$out/${side}-heap.test" -memprofile="$out/${side}-heap.pprof" . -args \
        -cache-perf-case=lifecycle -cache-perf-value=int -cache-perf-out="$(perf_json "$side" profile-heap)"
    done
    ;;
esac

record "finished_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# Inventory: every produced file except the manifest and the checksum list is
# hashed; each scenario JSON also contributes its workload (action) SHA256 so a
# reader can confirm both sides replayed the same actions.
for side in control candidate; do
  for json in "$results/$side"/*.json; do
    [ -e "$json" ] || continue
    rel="${json#"$out"/}"
    record "json[$rel]=$(hash_file "$json")"
    workload="$(sed -n 's/.*"workload_sha256": "\([0-9a-f]*\)".*/\1/p' "$json" | head -n 1)"
    # noop and lifecycle have no replayable action sequence; record that
    # explicitly instead of silently omitting the row.
    [ -n "$workload" ] || workload="none (scenario replays no action sequence)"
    record "actions[$rel]=$workload"
  done
done

(
  cd "$out"
  find . -type f ! -name SHA256SUMS ! -name manifest.txt -print0 | sort -z | xargs -0 -n 1 sh -c 'if command -v sha256sum >/dev/null 2>&1; then sha256sum "$0"; else shasum -a 256 "$0"; fi' >SHA256SUMS
)
record "files_hashed=$(find "$out" -type f ! -name SHA256SUMS ! -name manifest.txt | wc -l | tr -d ' ')"
echo "done: $out"
