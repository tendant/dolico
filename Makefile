.PHONY: help build build-go build-rust run test test-go test-rust lint fmt e2e testdata clean

# Caches live inside the repo so a build never depends on, or pollutes, the
# machine's shared Go cache.
GO ?= env GOCACHE=$(CURDIR)/.gocache GOMODCACHE=$(CURDIR)/.gomodcache go
CARGO ?= cargo
SHIM := rust/dolico-rs/target/release/dolico-rs

HOST ?= 127.0.0.1
PORT ?= 8080

help:
	@echo "Dolico -- document processing platform"
	@echo ""
	@echo "Targets:"
	@echo "  build       Build the API server and the Rust shim"
	@echo "  run         Build, then run the API server on $(HOST):$(PORT)"
	@echo "  test        Run the Go and Rust test suites"
	@echo "  test-go     Go tests only"
	@echo "  test-rust   Rust tests only"
	@echo "  e2e         End-to-end sweep over testdata/ against a live server"
	@echo "  lint        go vet + cargo clippy"
	@echo "  fmt         gofmt + cargo fmt"
	@echo "  testdata    Regenerate the binary fixtures in testdata/"
	@echo "  clean       Remove build output and caches"

build: build-rust build-go

build-go:
	@$(GO) build -o bin/dolico ./cmd/dolico

# The Go tests exec the shim, so it must exist before they run.
build-rust:
	@$(CARGO) build --release --manifest-path rust/dolico-rs/Cargo.toml

run: build
	@DOLICO_ADDR=$(HOST):$(PORT) ./bin/dolico

test: test-rust test-go

test-go: build-rust
	@$(GO) test ./...

test-rust:
	@$(CARGO) test --release --manifest-path rust/dolico-rs/Cargo.toml

lint:
	@$(GO) vet ./...
	@$(CARGO) clippy --manifest-path rust/dolico-rs/Cargo.toml --all-targets -- -D warnings

fmt:
	@$(GO) fmt ./...
	@$(CARGO) fmt --manifest-path rust/dolico-rs/Cargo.toml

e2e: build
	@./scripts/e2e.sh

testdata:
	@./scripts/gen-testdata.py

clean:
	@rm -rf bin .gocache .gomodcache rust/dolico-rs/target
