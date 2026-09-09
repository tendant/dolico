.PHONY: help build build-go build-rust run run-ocr run-vision ocr ocr-text ocr-vision \
        ocr-glm test test-go test-rust test-ocr lint fmt e2e e2e-ocr e2e-vision bench bench-ocr \
        bench-vision bench-glm bench-hard testdata clean clean-ocr \
        image up down logs config verify compose-check push login registry-check

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
# Where `make ocr-glm` finds the GLM-OCR model. Nothing else uses it, and the
# service refuses to start the engine without it.
GLM_URL ?= http://127.0.0.1:8080

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
	@echo "  ocr-glm      Run the OCR service with GLM-OCR as Tier 3 instead"
	@echo "  bench-glm    Score extraction with GLM-OCR in the vision tier"
	@echo ""
	@echo "Deployment (two containers, loopback only -- see deploy/README.md):"
	@echo "  image       Build both images as $(REGISTRY_PREFIX)dolico-{api,ocr}:$(DOLICO_TAG)"
	@echo "              (the tag is the current commit unless DOLICO_TAG says otherwise)"
	@echo "  up          Start them; first run downloads OCR models"
	@echo "  logs        Follow both services"
	@echo "  verify      Run the e2e sweep against the running deployment"
	@echo "  down        Stop and remove them (volumes survive)"
	@echo "  login       Log in to $(or $(DOLICO_REGISTRY),a registry), with credentials from deploy/.env"
	@echo "  push        Push both images to $(or $(DOLICO_REGISTRY),a registry) (needs DOLICO_REGISTRY)"

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

# The same corpus through the other Tier 3 engine. Requires `make ocr-glm`.
#
# Run this against `make bench-vision` on the same corpus: the API server does
# not know or care which engine the service has in its Tier 3 slot, so the two
# runs differ in exactly one thing and the CER columns are comparable.
bench-glm: build
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
# Deployment
#
# Two containers on one host, published on loopback only. dolico has no
# authentication, so `up` is not the whole job -- read deploy/README.md for
# what has to sit in front of it.
# ---------------------------------------------------------------------------

# Local settings -- a crates.io mirror, DOLICO_TAG, a port -- go in deploy/.env,
# which compose reads on its own because it sits beside the compose file. No
# --env-file here: the default is the same file a server would use, so one
# arrangement covers both.
# `override`, so that a COMPOSE in the environment cannot replace this under
# `make -e` and quietly drop the `compose` word.
override COMPOSE := docker compose -f deploy/docker-compose.yml

# What `make image` names the images it builds.
#
# The compose file defaults these to reg.memochat.ai, so that a server holding
# nothing but that file can pull. A build host is the other case: it is
# producing an image for itself, and stamping a registry it will never push to
# onto a local build makes `docker images` read like a lie. So the Makefile --
# the developer's entry point -- names them for this machine instead.
#
# Both are `?=` and make lets the environment win, so the registry naming is
# one variable away:
#
#   DOLICO_REGISTRY=reg.memochat.ai DOLICO_TAG=$(git rev-parse --short=7 HEAD) \
#     make image
#
# Nothing here pushes. `docker push` is the only command that contacts a
# registry and no target runs it -- CI does, from dolico-stack/ci/pipeline.hcl.
# One key out of deploy/.env. Compose reads that file on its own for everything
# it interpolates, but make does not, and these two are needed here as well --
# to name the images in output, and to refuse a push with nowhere to push to.
# Environment first, then the file, then the default: `?=` keeps an environment
# value, and the file only decides what the default would otherwise have been.
dotenv = $(shell sed -n 's/^$(1)=//p' deploy/.env 2>/dev/null | head -1 | tr -d '"'"'"'"')

# No default registry. A build host tags `dolico-api:dev`, with no prefix at
# all -- an image built here and never pushed has no business carrying the name
# of a registry it did not come from. Set DOLICO_REGISTRY when you mean to push
# or pull.
DOLICO_REGISTRY ?= $(call dotenv,DOLICO_REGISTRY)

# The tag defaults to the commit being built. It is the only thing that ties an
# image back to the source it came from, it is what makes a push reproducible,
# and it is the same 7 characters CI tags with, so an image built here and one
# built there have the same name for the same code.
#
# A working tree with changes in it gets `-dirty`. The commit alone would name
# something that was never built, and a registry is exactly where that stops
# being recoverable -- nobody can check out `39d18d6` and get the image they
# pulled. Untracked files count: they are as capable of ending up in the build
# context as modified ones.
#
# Outside a git checkout -- a tarball, a vendored copy -- it falls back to
# `dev`, which claims nothing.
GIT_SHA := $(shell git rev-parse --short=7 HEAD 2>/dev/null)
ifneq ($(GIT_SHA),)
GIT_TAG := $(GIT_SHA)$(shell test -z "$$(git status --porcelain 2>/dev/null)" || echo -dirty)
endif

DOLICO_TAG ?= $(or $(call dotenv,DOLICO_TAG),$(GIT_TAG),dev)
export DOLICO_REGISTRY DOLICO_TAG

# Optional build-time mirrors, exported so that both
# `CARGO_REGISTRY_MIRROR=... make image` and `make image CARGO_REGISTRY_MIRROR=...`
# reach the build. Unset here on purpose: no mirror URL is committed, because
# the right one depends on where you are, and a stale one in the repository is
# worse than none.
#
# Only when it has a value. `export` on an undefined variable still puts an
# empty one in the environment, and compose gives the environment precedence
# over --env-file -- so exporting unconditionally would mean a .env whose
# mirror is silently ignored.
ifneq ($(CARGO_REGISTRY_MIRROR),)
export CARGO_REGISTRY_MIRROR
endif
ifneq ($(PYPI_FILES_MIRROR),)
export PYPI_FILES_MIRROR
endif
ifneq ($(PIP_INDEX_URL),)
export PIP_INDEX_URL
endif
ifneq ($(DEBIAN_MIRROR),)
export DEBIAN_MIRROR
endif

REGISTRY_PREFIX := $(if $(DOLICO_REGISTRY),$(DOLICO_REGISTRY)/,)

# Which tiers the OCR image carries. Empty here rather than `structure`, which
# is the Dockerfile's own default: exporting a value unconditionally would
# override a deploy/.env that sets it, for the same reason the mirrors below
# are exported only when they have one.
OCR_EXTRAS ?=

# The engine goes in the tag, because two images built from the same commit
# with different Tier 3 engines are different images. Sharing a tag would mean
# the second build silently replacing the first, with nothing in `docker
# images` to say which one is there. Tier-2-only builds keep the bare tag they
# have always had.
# `filter` and not `findstring`: findstring is a substring test, so an extra
# named `visionary` would tag the image `-mineru`. filter matches whole words,
# which is what an extras list is.
OCR_VARIANT := $(if $(filter vision,$(OCR_EXTRAS)),-mineru,$(if $(filter glm,$(OCR_EXTRAS)),-glm,))

ifneq ($(OCR_EXTRAS),)
export OCR_EXTRAS
endif
ifneq ($(OCR_VARIANT),)
export OCR_VARIANT
endif

IMAGE_API := $(REGISTRY_PREFIX)dolico-api:$(DOLICO_TAG)
IMAGE_OCR := $(REGISTRY_PREFIX)dolico-ocr:$(DOLICO_TAG)$(OCR_VARIANT)

# --pull by default: a base image left in the local store goes stale, and a
# stale Debian base fails `apt-get update` with NO_PUBKEY once the archive is
# signed with keys its keyring predates -- a build failure with no cause in
# this repository and no fix inside it. `make image PULL=` skips the check when
# you are offline or deliberately pinning what you already have.
PULL ?= --pull

# BuildKit attaches a provenance attestation to every build by default, and an
# attestation is a second manifest -- so what would have been one image becomes
# an OCI index holding the image and its attestation. Registries that speak
# only Docker's schema 2 reject that index with
#
#   error from registry: manifest invalid
#
# after uploading every layer, which is a slow way to be told the bytes were
# fine and the description of them was not. Unset this to get attestations back
# on a registry that understands them; `docker compose build` has no flag for
# it, so it is the environment or nothing.
export BUILDX_NO_DEFAULT_ATTESTATIONS ?= 1

# One at a time, not the parallel build compose does by default. The api stage
# is a full Rust release compile that will take every core it is given, and the
# ocr stage unpacks several gigabytes of PaddlePaddle wheels; run together on a
# Docker VM with 8GB they lose to the OOM killer, which surfaces as a step
# dying with exit code 137 rather than as anything about memory. The CI
# pipeline in dolico-stack is sequential for the same reason.
# A docker CLI without the Compose v2 plugin does not say so. `compose` is not
# a subcommand it knows, so everything after it is parsed as docker's own
# flags and you get
#
#   unknown shorthand flag: 'f' in -f
#
# which names neither compose nor the plugin, on a machine where the fix is one
# package. Every target that shells out to compose checks first.
.PHONY: compose-check
compose-check:
	@docker compose version >/dev/null 2>&1 || { \
		echo "docker compose (the v2 plugin) is not installed."; \
		echo "  Debian/Ubuntu:  apt-get install docker-compose-plugin"; \
		echo "  RHEL/Fedora:    dnf install docker-compose-plugin"; \
		echo "  otherwise:      https://docs.docker.com/compose/install/linux/"; \
		echo; \
		echo "This repo needs v2: the compose file uses top-level \`name:\`,"; \
		echo "which the standalone docker-compose v1 does not support."; \
		exit 1; }

.PHONY: registry-check
registry-check:
	@if [ -z "$(DOLICO_REGISTRY)" ]; then \
		echo "DOLICO_REGISTRY is not set, so these images have no registry in"; \
		echo "their names and nowhere to go. Set it in deploy/.env:"; \
		echo; \
		echo "  DOLICO_REGISTRY=reg.example.com"; \
		exit 1; \
	fi

image: compose-check
	@$(COMPOSE) build $(PULL) api
	@$(COMPOSE) build $(PULL) ocr
	@echo "built $(IMAGE_API)"
	@echo "built $(IMAGE_OCR)"

up: compose-check
	@$(COMPOSE) up -d
	@echo "API on 127.0.0.1:$${DOLICO_PORT:-8080} (loopback only), from $(IMAGE_API)."
	@echo "First start downloads OCR models and is unhealthy meanwhile:"
	@echo "  make logs"

down: compose-check
	@$(COMPOSE) down

logs: compose-check
	@$(COMPOSE) logs -f

# Run the full e2e sweep against a deployment that is already running, rather
# than against a server the script starts for itself. Same checker, same schema
# validation -- the difference is that this one is talking to the containers you
# are about to put behind a gateway.
verify:
	@DOLICO_EXPECT_OCR=$(EXPECT_OCR) \
		./scripts/e2e_check.py http://$(HOST):$${DOLICO_PORT:-8080}

config: compose-check
	@$(COMPOSE) config

# Pushes what is already built, and nothing else. A push that builds is a push
# that can change what it is sending -- a base image moved under `--pull`, a
# file edited since the build -- and the whole value of pushing a tagged image
# is that it is the one you tested.
#
# It does check first, because compose's own failure is misleading here: it
# reports `tag does not exist` naming the image it wanted, which reads like the
# build broke rather than like the name it looked for was never produced.
push: registry-check compose-check
	@missing=""; \
	for img in $(IMAGE_API) $(IMAGE_OCR); do \
		docker image inspect "$$img" >/dev/null 2>&1 || missing="$$missing $$img"; \
	done; \
	if [ -n "$$missing" ]; then \
		echo "not built:$$missing"; \
		if docker image inspect dolico-api:$(DOLICO_TAG) >/dev/null 2>&1; then \
			echo; \
			echo "dolico-api:$(DOLICO_TAG) does exist without the registry prefix,"; \
			echo "so it was built before DOLICO_REGISTRY was set. The registry is"; \
			echo "part of the image name, so that is a different image to docker."; \
		fi; \
		echo; \
		echo "Build them under this name first:"; \
		echo "  make image"; \
		exit 1; \
	fi
	@$(COMPOSE) push || { \
		echo; \
		echo "If that was an authorization failure, this host has no credentials"; \
		echo "for $(DOLICO_REGISTRY):"; \
		echo "  make login"; \
		exit 1; \
	}
	@echo "pushed $(IMAGE_API)"
	@echo "pushed $(IMAGE_OCR)"

# Credentials never pass through make -- see the script.
login: registry-check
	@./scripts/registry-login

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

# The other Tier 3 engine. Same service, same port, same escalation rules --
# only the model that reads an escalated page differs.
#
# It needs a GLM-OCR endpoint to talk to and will not start without one:
# DOLICO_VISION_URL points at a vLLM, SGLang, MLX or Ollama server serving the
# 0.9B VLM. Only the layout stage runs in this process. Unconfigured, the tier
# reports itself unavailable rather than falling back to the vendor's cloud
# API, which is what the library would do on its own.
#
# Unmeasured on this repository's corpus. `make bench-glm` against
# `make bench-vision` is the comparison that would settle whether it belongs
# here -- see docs/glm-ocr-tier-design.md.
ocr-glm:
	@DOLICO_OCR_WORKERS=$(OCR_WORKERS) DOLICO_VISION_ENGINE=glm-ocr \
		DOLICO_VISION_URL=$(GLM_URL) $(UV) run --project $(OCR_DIR) \
		--extra structure --extra glm \
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
