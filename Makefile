.PHONY: help build build-go build-rust run run-ocr run-vision ocr ocr-text ocr-vision \
        test test-go test-rust test-ocr lint fmt e2e e2e-ocr e2e-vision bench bench-ocr \
        bench-vision bench-hard testdata clean clean-ocr

# Caches live inside the repo so a build never depends on, or pollutes, the
# machine's shared Go cache.
GO ?= env GOCACHE=$(CURDIR)/.gocache GOMODCACHE=$(CURDIR)/.gomodcache go
CARGO ?= cargo
SHIM := rust/dolico-rs/target/release/dolico-rs

HOST ?= 127.0.0.1
PORT ?= 8080

OCR_DIR := python/ocr-service
OCR_HOST ?= 127.0.0.1
OCR_PORT ?= 8181
OCR_URL ?= http://$(OCR_HOST):$(OCR_PORT)
# The tier `make ocr` starts. Use EXPECT_OCR=paddleocr with `make ocr-text`.
EXPECT_OCR ?= pp-structurev3
# OCR worker processes. Each costs 2.5-3GB once warm; see the `ocr` target.
OCR_WORKERS ?= 1
UV ?= uv

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
	@echo "  bench       Score extraction against ground truth on a cold cache"
	@echo "  testdata    Regenerate the binary fixtures in testdata/"
	@echo "  clean       Remove build output and caches"
	@echo ""
	@echo "OCR tier (optional -- without it, scanned pages use the stub):"
	@echo "  ocr         Run the OCR service (layout tier) on $(OCR_HOST):$(OCR_PORT)"
	@echo "  ocr-text    Run it with text-line OCR only (no layout analysis)"
	@echo "  run-ocr     Run the API server wired to an OCR service already running"
	@echo "  test-ocr    Python tests, plus the Go tests against a live OCR service"
	@echo "  e2e-ocr     End-to-end sweep with real OCR asserted"
	@echo "  bench-ocr   Score extraction with the OCR tier included"
	@echo ""
	@echo "Vision tier (optional third tier -- only for pages OCR loses):"
	@echo "  ocr-vision   Run the OCR service with MinerU installed as well"
	@echo "  run-vision   Run the API server with the vision escalation enabled"
	@echo "  e2e-vision   End-to-end sweep asserting faded.pdf escalated and was recovered"
	@echo "  bench-vision Score extraction with all three tiers"
	@echo "  bench-hard   Score the real-scan corpus in testdata/corpus-hard"

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

# Asserts that scanned pages were read by the real engine rather than the stub.
# Requires an OCR service at $(OCR_URL); start one with `make ocr`.
e2e-ocr: build
	@DOLICO_OCR_URL=$(OCR_URL) DOLICO_EXPECT_OCR=$(EXPECT_OCR) ./scripts/e2e.sh

# Requires a service started with `make ocr-vision`.
e2e-vision: build
	@DOLICO_OCR_URL=$(OCR_URL) DOLICO_EXPECT_OCR=$(EXPECT_OCR) \
		DOLICO_VISION_ENABLED=1 DOLICO_EXPECT_VISION=1 ./scripts/e2e.sh

# Scores extraction against testdata/ground-truth.json on a cold cache. Set
# DOLICO_OCR_URL to include the OCR tier; without it, scanned pages score as
# total failures because the stub does not read them.
bench: build
	@./scripts/bench.sh $(BENCH_ARGS)

bench-ocr: build
	@DOLICO_OCR_URL=$(OCR_URL) ./scripts/bench.sh $(BENCH_ARGS)

# The pair that gives Tier 3 a number: run this against `make bench-ocr` on the
# same corpus and compare the faded.pdf row.
bench-vision: build
	@DOLICO_OCR_URL=$(OCR_URL) DOLICO_VISION_ENABLED=1 ./scripts/bench.sh $(BENCH_ARGS)

# Real scans, whose ground truth is transcribed rather than generated. Kept out
# of the default corpus for exactly that reason.
#
# This used to force the thresholds, because the real scan did not trip the
# real ones: PaddleOCR misreads that page and reports 0.938 confidence, so it
# scores about 0.61 and no legal threshold selected it. The disagreement probe
# catches it on the production defaults, so the forcing is gone and this now
# measures what the pipeline actually does.
bench-hard: build
	@DOLICO_OCR_URL=$(OCR_URL) DOLICO_VISION_ENABLED=1 \
		./scripts/bench.sh --corpus testdata/corpus-hard $(BENCH_ARGS)

testdata:
	@./scripts/gen-testdata.py

clean:
	@rm -rf bin .gocache .gomodcache rust/dolico-rs/target

# ---------------------------------------------------------------------------
# OCR tier
#
# Optional by design: with no OCR service configured the API falls back to the
# stub tier, so everything above works with no Python installed. The first
# `make ocr` downloads the PaddleOCR models (~50MB) into ~/.paddlex.
# ---------------------------------------------------------------------------

# Includes the layout-analysis tier. Use `make ocr-text` for a Tier-1-only run.
#
# WORKERS is how many pages can be OCR'd at once. One inference uses about one
# core and does not thread, so throughput scales with processes -- but each
# process costs 2.5-3GB once warm, so budget roughly WORKERS x 3GB before
# raising it. The Go client reads this number from the service and matches its
# request concurrency to it automatically.
ocr:
	@DOLICO_OCR_WORKERS=$(OCR_WORKERS) $(UV) run --project $(OCR_DIR) --extra structure \
		uvicorn dolico_ocr.app:app --host $(OCR_HOST) --port $(OCR_PORT) \
		--workers $(OCR_WORKERS)

ocr-text:
	@DOLICO_OCR_TIER=text DOLICO_OCR_WORKERS=$(OCR_WORKERS) \
		$(UV) run --project $(OCR_DIR) uvicorn dolico_ocr.app:app \
		--host $(OCR_HOST) --port $(OCR_PORT) --workers $(OCR_WORKERS)

run-ocr: build
	@DOLICO_ADDR=$(HOST):$(PORT) DOLICO_OCR_URL=$(OCR_URL) ./bin/dolico

# ---------------------------------------------------------------------------
# Vision tier (Tier 3)
#
# The same service on the same port, with MinerU installed alongside PaddleOCR.
# It is a separate target because the extra is heavy: the install pulls torch,
# and the first request downloads ~3.2GB of MinerU weights into the Hugging
# Face cache. Budget roughly 7GB of RAM per worker, measured -- 6.3GB steady
# with both model sets resident and 7.6GB peak on the first vision call,
# against ~3GB for the OCR tiers alone.
#
# Nothing else changes for the OCR tiers -- `ocr-vision` is a superset of
# `ocr`. The API server still only escalates to Tier 3 when asked to, but with
# the disagreement probe on that is every document with a scanned page, so
# budget for MinerU being resident rather than occasional.
# ---------------------------------------------------------------------------

ocr-vision:
	@DOLICO_OCR_WORKERS=$(OCR_WORKERS) $(UV) run --project $(OCR_DIR) \
		--extra structure --extra vision \
		uvicorn dolico_ocr.app:app --host $(OCR_HOST) --port $(OCR_PORT) \
		--workers $(OCR_WORKERS)

# Requires a service started with `make ocr-vision`; against a plain `make ocr`
# the server logs that MinerU is absent and runs with two tiers.
run-vision: build
	@DOLICO_ADDR=$(HOST):$(PORT) DOLICO_OCR_URL=$(OCR_URL) \
		DOLICO_VISION_ENABLED=1 ./bin/dolico

test-ocr:
	@$(UV) run --project $(OCR_DIR) --extra dev pytest -q $(OCR_DIR)
	@echo "--- Go client against the live OCR service at $(OCR_URL) ---"
	@DOLICO_OCR_URL=$(OCR_URL) $(GO) test -count=1 ./internal/engine/paddleocr/...

clean-ocr:
	@rm -rf $(OCR_DIR)/.venv
