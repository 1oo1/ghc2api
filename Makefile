GO ?= go
BINARY := bin/ghc2api

.PHONY: build web test test-race vet run clean

build:
	mkdir -p bin
	$(GO) build -o $(BINARY) ./cmd/ghc2api

# 构建内嵌的管理控制台（web/dist/index.html，需提交到 git）；build 不依赖 Node。
web:
	cd web && npm ci && npm run build

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
