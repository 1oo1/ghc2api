GO ?= go
BINARY := bin/ghc2api

.PHONY: build test test-race vet run clean

build:
	mkdir -p bin
	$(GO) build -o $(BINARY) ./cmd/ghc2api

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

run:
	$(GO) run ./cmd/ghc2api

clean:
	rm -f $(BINARY)
