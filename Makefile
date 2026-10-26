GO ?= go
GOLANGCI_LINT ?= golangci-lint
FUZZTIME ?= 15s
FUZZWORKERS ?= 4
PERFSUITE ?= full
PERFROLE ?= original

.PHONY: check fmt-check lint lint-fix vet test race coverage fuzz bench bench-full bench-expiry bench-sharded perf-quick perf-baseline perf-compare

check: fmt-check lint vet test race

fmt-check:
	@test -z "$$(gofmt -l *.go)" || { gofmt -l *.go; exit 1; }

lint:
	$(GOLANGCI_LINT) run --config=.golangci.yml ./...

lint-fix:
	$(GOLANGCI_LINT) run --config=.golangci.yml --fix ./...

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
	$(GO) test -run='^$$' -bench='^(BenchmarkCache|BenchmarkCacheWorkload|BenchmarkCachePayload)$$' -benchmem -cpu=1,2,4,8 -benchtime=200ms -count=10 -timeout=45m ./... -args -cache-perf-full=true

# bench-expiry measures one post-expiry operation per fresh fixture; the
# adaptive b.N loop would refill 100K/1M caches, so it requires -benchtime=1x.
bench-expiry:
	$(GO) test -run='^$$' -bench='^BenchmarkCacheExpiry$$' -benchmem -benchtime=1x -count=10 -timeout=10m ./... -args -cache-perf-full=true

# bench-sharded compares one exact LRU with the opt-in sharded read path.
bench-sharded:
	$(GO) test -run='^$$' -bench='^BenchmarkCacheSharded$$' -benchmem -benchtime=200ms -count=1 -cpu=1,2,4,8,24 ./...

# perf-quick measures the working tree with a short latency-only matrix.
perf-quick:
	bash scripts/perf-baseline.sh --suite quick $(if $(PERFOUT),--out "$(PERFOUT)",)

# perf-baseline collects the staged evidence bundle into a fresh directory;
# smoke verifies the pipeline, full freezes the repeated formal baseline.
perf-baseline:
	bash scripts/perf-baseline.sh --suite $(PERFSUITE) $(if $(PERFOUT),--out "$(PERFOUT)",)

# perf-compare measures a candidate commit against a control commit with one
# shared test harness. PERFROLE=original requires the control to be the frozen
# Stage A runtime; PERFROLE=incremental allows B1 as the control for
# attribution only. All three commits and a fresh PERFOUT are required.
perf-compare:
	bash scripts/perf-compare.sh \
		--control "$(PERFCONTROL)" --candidate "$(PERFCANDIDATE)" \
		--harness "$(PERFHARNESS)" --comparison-role "$(PERFROLE)" \
		--suite "$(PERFSUITE)" --out "$(PERFOUT)"
