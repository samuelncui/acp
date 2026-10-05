GO ?= go
PYTHON ?= python3

.PHONY: help test race vet check cross cross-tests bench bench-rewrite test-release-tools release-check release-history performance-collect performance-check

help:
	@sed -n 's/^\([a-z-]*\):.*/\1/p' Makefile | sort

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

cross:
	GOOS=linux GOARCH=amd64 $(GO) build ./...
	GOOS=windows GOARCH=amd64 $(GO) build ./...
	GOOS=darwin GOARCH=arm64 $(GO) build ./...
	GOOS=freebsd GOARCH=amd64 $(GO) build ./...

cross-tests:
	@set -eu; output="$$(mktemp -d)"; trap 'rm -rf "$$output"' EXIT; \
	for target in linux/amd64 windows/amd64 darwin/arm64 freebsd/amd64; do \
		export GOOS="$${target%/*}" GOARCH="$${target#*/}" CGO_ENABLED=0; \
		packages="$$($(GO) list ./...)"; \
		for package in $$packages; do \
			mkdir -p "$$output/$$target/$$package"; \
			$(GO) test -c -o "$$output/$$target/$$package/test" "$$package"; \
		done; \
	done

bench:
	$(GO) test -run '^$$' -bench '^Benchmark(ReadBuffered|ReadMapped|RefreshSignatureUnchanged|RefreshSignatureChanged|CopyWorkload)$$' -benchtime=1s -count=1 -benchmem .

bench-rewrite:
	$(GO) test -run '^$$' -bench '^BenchmarkRewriteCheckpointSnapshots$$' -benchtime=1s -count=1 -benchmem ./cmd/acp-rewrite

test-release-tools:
	PYTHONDONTWRITEBYTECODE=1 $(PYTHON) -m unittest discover -s dev -p 'test_*.py'

check: test vet cross test-release-tools
	@files="$$(gofmt -l .)"; if [ -n "$$files" ]; then echo "gofmt needed:"; echo "$$files"; exit 1; fi
	@echo "check ok"

release-check:
	@PYTHONDONTWRITEBYTECODE=1 $(PYTHON) dev/release.py check

release-history:
	@PYTHONDONTWRITEBYTECODE=1 $(PYTHON) dev/release.py history $(RELEASE_ARGS)

performance-collect:
	@PYTHONDONTWRITEBYTECODE=1 $(PYTHON) dev/release.py collect $(PERF_ARGS)

performance-check:
	@PYTHONDONTWRITEBYTECODE=1 $(PYTHON) dev/release.py performance $(PERF_ARGS)
