GO ?= go
FUZZTIME ?= 15s
FUZZWORKERS ?= 4
PERFSUITE ?= full

.PHONY: check fmt-check vet test race coverage fuzz bench bench-full bench-expiry perf-baseline

check: fmt-check vet test race

fmt-check:
	@test -z "$$(gofmt -l *.go)" || { gofmt -l *.go; exit 1; }

vet:
	$(GO) vet ./...

test:
	$(GO) test -count=1 -timeout=60s ./...

race:
	$(GO) test -race -shuffle=on -count=3 -timeout=120s ./...

coverage:
	$(GO) test -count=1 -timeout=60s -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

fuzz:
	$(GO) test -run='^$$' -fuzz='^FuzzCacheModel$$' -fuzztime=$(FUZZTIME) -parallel=$(FUZZWORKERS) -timeout=5m .

# bench runs the ordinary 1K-key workloads only; heavy scenarios are opt-in.
bench:
	$(GO) test -run='^$$' -bench='^(BenchmarkCache|BenchmarkCacheWorkload|BenchmarkCachePayload)$$' -benchmem -benchtime=200ms -count=1 ./...

# bench-full adds the 100K/1M-capacity scenarios behind -cache-perf-full and
# repeats every micro benchmark 10 times across CPU counts for benchstat.
bench-full:
	$(GO) test -run='^$$' -bench='^(BenchmarkCache|BenchmarkCacheWorkload|BenchmarkCachePayload)$$' -benchmem -cpu=1,2,4,8 -benchtime=200ms -count=10 -timeout=30m ./... -args -cache-perf-full=true

# bench-expiry measures one post-expiry operation per fresh fixture; the
# adaptive b.N loop would refill 100K/1M caches, so it requires -benchtime=1x.
bench-expiry:
	$(GO) test -run='^$$' -bench='^BenchmarkCacheExpiry$$' -benchmem -benchtime=1x -count=10 -timeout=10m ./... -args -cache-perf-full=true

# perf-baseline collects the staged evidence bundle into a fresh directory;
# smoke verifies the pipeline, full freezes the repeated formal baseline.
perf-baseline:
	bash scripts/perf-baseline.sh --suite $(PERFSUITE) $(if $(PERFOUT),--out "$(PERFOUT)",)
