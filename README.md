# Dolico

A document ingestion platform that routes **per page**, not per document.

A two-page PDF whose first page is text and whose second page is a scan costs
one OCR call. Not two, and not zero. Everything else in the architecture exists
to make that decision possible and to record how it was made.

Implements [`document-processing-design.md`](document-processing-design.md).

```
POST /v1/documents ──► blob store (sha256) ──► worker pool
                                                    │
                                              inspect once
                                                    │
                        ┌───────────────────────────┴──────────────────────┐
                   non-PDF                                              PDF
                        │                                                  │
                 anydoc (Rust)                            pdf-inspector classifies
                        │                                        each page
                        │                          ┌───────────────┴───────────────┐
                        │                    text_based                  scanned / image
                        │                          │                              │
                        │              pdf-inspector extract        PP-StructureV3 (Python)
                        │                          │                              │
                        └──────────────────────────┴──────────────────────────────┘
                                                    │
                                    per-page quality score, escalate if poor
                                                    │
                                       Canonical Document (schema v1)
                                                    │
                              ┌─────────────────────┴──────────────────┐
                    GET /v1/documents/{id}                GET /v1/documents/{id}.md
                       JSON — the representation             Markdown — a view
```

## Status

Working end to end, with **no persistence**. Blobs live under a temp directory,
jobs live in a map, and restarting the process loses everything. That is
deliberate — the point was to settle the canonical schema, the `Engine`
interface and the routing contracts while they were still cheap to change.

| Working | Not yet |
| --- | --- |
| Per-page PDF classification and routing | Postgres, MinIO, durable jobs |
| Native extraction for 14 formats + Markdown/text | NATS, distributed workers |
| Real OCR in two tiers, optional and pluggable | Vision-LLM fallback |
| Layout analysis: scanned tables come back as grids | Engine benchmarks over a real corpus |
| Canonical JSON as the primary API | Parallel page processing |
| Markdown generated as a view | HTML input |
| Bounding boxes, provenance, per-page quality scores | |
| Content-hash caching at page and document level | |

The OCR tier is optional: with no OCR service configured the API falls back to
a stub that marks scanned pages as unread rather than silently returning
nothing, so everything builds, runs and tests with no Python installed.

## Quick start

Needs Go 1.25+, Rust 1.97+, and `uv` (only for regenerating fixtures).

```bash
make build      # builds bin/dolico and the Rust shim
make run        # serves on 127.0.0.1:8080
make test       # Go + Rust test suites
make e2e        # every fixture through the HTTP API, validated against the schema
```

With real OCR — two terminals, and `uv` for the Python side:

```bash
make ocr        # OCR service on 127.0.0.1:8181 (first run fetches ~50MB of models)
make run-ocr    # the API server, wired to it
make e2e-ocr    # the sweep again, asserting the real engine read the scans
```

```bash
# Upload and wait for the result
curl -F file=@testdata/mixed.pdf 'localhost:8080/v1/documents?wait=true' | jq

# See which engine handled which page
curl -F file=@testdata/mixed.pdf 'localhost:8080/v1/documents?wait=true' \
  | jq '.pages[] | {page: .number, class: .classification.type,
                    engine: .blocks[0].provenance.engine, score: .quality.score}'
```

```json
{"page": 1, "class": "text_based", "engine": "pdf-inspector",  "score": 0.774}
{"page": 2, "class": "scanned",    "engine": "pp-structurev3", "score": 0.663}
```

Page 2 also comes back with something page 1 lacks — real page dimensions and a
confidence on every block — because the OCR path renders the page and the
native path does not:

```json
{"page": 2, "width": 612, "height": 792,
 "blocks": [{"text": "SIGNED AGREEMENT", "confidence": 0.993,
             "bbox": {"x": 60, "y": 726, "width": 52, "height": 7}}]}
```

And a table that exists only as pixels comes back as a table:

```bash
curl -F file=@testdata/scanned-table.pdf 'localhost:8080/v1/documents?wait=true' \
  | jq -r .id | xargs -I{} curl -s localhost:8080/v1/documents/{}.md
```

```markdown
# QUARTERLY SALES

| Region | Units | Revenue |
| --- | --- | --- |
| North | 120 | 14,400.00 |
| South | 86 | 10,320.00 |
```

Async, if you would rather not block:

```bash
JOB=$(curl -sF file=@testdata/sample.docx localhost:8080/v1/documents | jq -r .job_id)
curl -s localhost:8080/v1/jobs/$JOB | jq
curl -s localhost:8080/v1/documents/$(curl -s localhost:8080/v1/jobs/$JOB | jq -r .document_id).md
```

## API

| Method | Path | |
| --- | --- | --- |
| `POST` | `/v1/documents` | multipart upload → `202`. `?wait=true` blocks and returns the document. |
| `GET` | `/v1/jobs/{id}` | job state, engine timings, error detail |
| `GET` | `/v1/jobs` | all jobs, newest first |
| `GET` | `/v1/documents/{id}` | canonical JSON |
| `GET` | `/v1/documents/{id}.md` | Markdown view |
| `GET` | `/v1/documents/{id}/assets/{asset}` | extracted asset bytes |
| `GET` | `/v1/engines` | engines, versions, cache stats |
| `GET` | `/healthz` | liveness, including that the shim is executable |

Every response carries `X-Trace-Id`. Failures map to status codes by cause:
`415` unsupported format, `422` malformed or encrypted document, `413` too
large, `503` queue full.

## How it is put together

**Go** orchestrates: HTTP, routing, caching, storage, normalization, the
Markdown view. **Rust** parses. **Python** does OCR, which is where the ML
ecosystem is.

The Rust work is done by two Firecrawl libraries, not by us:

- [`anydoc`](https://github.com/firecrawl/anydoc) 0.1.7 — DOCX, XLSX, PPTX,
  ODF, RTF, EPUB, CSV and the legacy binary formats, at roughly 5ms.
- [`pdf-inspector`](https://github.com/firecrawl/pdf-inspector) 0.1.7 —
  classifies each PDF page as text/scanned/image/mixed in 10–50ms by reading
  the PDF's internals, without rendering, then extracts positioned text.

Neither exposes its document model as JSON: `anydoc::to_document` is a Rust-only
API and both CLIs emit Markdown. So `rust/dolico-rs` is a thin shim that calls
both and serializes into our canonical schema — which we want regardless, since
the schema adds page, bounding-box, confidence and provenance fields neither
library carries.

Go reaches the shim by **exec**, not cgo and not HTTP: no ports, no lifecycle,
and a parser crash takes down one subprocess instead of the API. Process spawn
costs a couple of milliseconds against extraction times in the tens. The seam is
deliberate — `internal/engine/rustshim` is the only code that knows how the shim
is invoked, so moving to a long-lived service later touches one file.

The OCR tier is the opposite: a long-lived HTTP service, because it holds a
loaded model and paying to load one per document would dominate everything
else. Go posts the original bytes plus the page numbers; the service rasterizes
with `pypdfium2`, which keeps pdfium's native dependency out of both the Go
binary and the Rust shim. It answers with the same canonical envelope the shim
writes, so one code path in Go parses both.

```
cmd/dolico/                 API server
internal/canonical/         the canonical model — the contract everything meets
internal/engine/            Engine interface + registry
        ├── rustshim/       subprocess transport; native and PDF engines
        ├── paddleocr/      HTTP client for the OCR tier
        ├── ocrstub/        fallback OCR tier when none is configured
        ├── quality/        per-page scoring
        └── router/         the routing policy
internal/blob/              content-addressed filesystem store
internal/cache/             page-level result cache
internal/jobs/              in-memory job store + worker pool
internal/render/            canonical → Markdown
internal/api/               HTTP handlers
rust/dolico-rs/             the shim: anydoc + pdf-inspector → canonical JSON
python/ocr-service/         PaddleOCR over HTTP (see its own README)
schema/canonical-v1.json    cross-language source of truth
```

No third-party Go dependencies. Routing is `net/http.ServeMux`.

## Design decisions worth knowing

**Markdown is a view, not the representation.** The canonical JSON is primary
and `internal/render` is a pure function over it. An API whose main output is
Markdown pushes every consumer into re-parsing prose to recover structure the
pipeline already had.

**Geometry is never fabricated.** `bbox` is absent wherever the source has none
— which is every non-PDF format — rather than filled with a plausible
rectangle, because a consumer cannot tell an invented box from a real one. For
the same reason a degenerate box (zero width or height) is reported as no box
at all.

**Native documents get one page, honestly.** anydoc's model is a flat block
flow with no slide or sheet boundaries; a multi-sheet workbook is just headings
followed by tables. Rather than guess where pages begin, every native document
becomes a single page of kind `section`. `Page.Kind` records what a "page" means
for that source.

**Quality scoring is not engine confidence.** A PDF with a broken font encoding
extracts "text" with complete confidence and produces mojibake. The scorer
combines text density, replacement-character ratio, and how word-like the output
is; engine confidence is the smallest of four weights. A page scoring below
`DOLICO_OCR_THRESHOLD` is re-extracted by the OCR tier.

**Caching is keyed per page**, on document hash + engine + engine version +
pipeline version + configuration. An engine upgrade re-runs only that engine's
pages. Above it, a document-level short-circuit skips work entirely when the
same bytes have already been processed by the current schema and pipeline.

**pdf-inspector mixes 0- and 1-indexed page numbers** across its API — six
different functions, not consistently. Everything is normalized to 1-indexed at
the shim boundary; `rust/dolico-rs/src/pdf.rs` documents the table and has a
regression test, because getting this wrong means OCR runs silently on the
wrong page.

**Glyph widths are often unavailable.** The base-14 PDF fonts declare no
`/Widths`, so pdf-inspector reports zero-width text items. Widths are recovered
from the distance to the next run on the same line, falling back to
pdf-inspector's own character-count estimate. Any page where that happened is
tagged `estimated_glyph_widths` in its classification reasons.

## Configuration

| Variable | Default | |
| --- | --- | --- |
| `DOLICO_ADDR` | `:8080` | listen address |
| `DOLICO_DATA_DIR` | `$TMPDIR/dolico` | blobs and derived artifacts |
| `DOLICO_WORKERS` | CPU count | extraction worker pool size |
| `DOLICO_SHIM_PATH` | auto-detected | the `dolico-rs` binary |
| `DOLICO_SHIM_TIMEOUT` | `120s` | bound on one shim invocation |
| `DOLICO_OCR_THRESHOLD` | `0.60` | page quality below which OCR is tried |
| `DOLICO_MAX_UPLOAD_BYTES` | `256MiB` | per-upload cap |
| `DOLICO_OCR_URL` | unset | OCR service address; unset means the stub tier |
| `DOLICO_OCR_TIMEOUT` | `10m` | bound on one OCR request |

Setting `DOLICO_OCR_URL` to a service that is not reachable is a startup
failure, not a silent fallback: a deployment configured for OCR that quietly
serves stub text would be worse than one that refuses to start. The OCR
service has [its own configuration](python/ocr-service/README.md#configuration).

## Testing

```bash
make test      # 48 Rust tests, ~110 Go tests
make e2e       # HTTP sweep with JSON Schema validation
make test-ocr  # 50 Python tests, plus the Go client against a live OCR service
make e2e-ocr   # the sweep again, asserting real OCR read the scanned pages
```

The Go tests for `rustshim` and `api` drive the **real** shim against the
**real** fixtures — a mock would test nothing that matters about talking to two
third-party libraries. `scripts/e2e_check.py` is the only place a genuine JSON
Schema validator runs over produced documents, which is what catches drift
between `schema/canonical-v1.json` and its Go and Rust mirrors.

Fixtures in `testdata/` are committed and regenerated by
`scripts/gen-testdata.py` (`make testdata`). The three PDFs are the ones that
matter: `text.pdf` (all text), `scanned.pdf` (image only, no text operators),
and `mixed.pdf` (one of each), plus `corrupt.pdf` for the error path.

## Next

**Persistence** is now the largest gap. Postgres for jobs and page metadata,
MinIO behind the existing `blob.Store` interface — nothing outside that package
knows where bytes live. It also unlocks partial reprocessing after an engine
upgrade: the page-level cache key already exists, it just has nowhere durable
to look.

**PP-StructureV3** is the natural second step. Tier 1 gives text lines grouped
into paragraphs; Tier 2 would give headings, tables and reading order, which is
what scanned tables need. It slots in as another engine behind the same
interface.

**Tuning the quality weights** matters more now that escalation costs seconds
of real work. The weights in `internal/engine/quality` are guesses in a named
struct so a benchmark can sweep them, but nothing has measured them against a
real corpus — and the same is true of the OCR model choice, where the default
was picked on a single synthetic fixture.

Smaller: HTML input, and the page cache clears wholesale at its limit rather
than evicting LRU (deliberate, and moot once values live in a real store).
