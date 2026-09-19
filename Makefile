GO ?= go

.PHONY: help test race vet check cross bench

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

bench:
	$(GO) test -run XXX -bench . -benchtime=1x .

check: test vet cross
	@files="$$(gofmt -l .)"; if [ -n "$$files" ]; then echo "gofmt needed:"; echo "$$files"; exit 1; fi
	@echo "check ok"
