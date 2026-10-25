GO ?= go
FUZZTIME ?= 15s
FUZZWORKERS ?= 4

.PHONY: check fmt-check vet test race coverage fuzz bench

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

bench:
	$(GO) test -run='^$$' -bench=. -benchmem -benchtime=200ms ./...
