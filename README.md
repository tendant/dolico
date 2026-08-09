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
                        │              pdf-inspector extract              OCR engine
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

This is the first milestone: a complete end-to-end slice with **no
persistence**. Blobs live under a temp directory, jobs live in a map, and
restarting the process loses everything. That is deliberate — the point is to
settle the canonical schema, the `Engine` interface and the routing contracts
while they are still cheap to change.

| Working | Not yet |
| --- | --- |
| Per-page PDF classification and routing | Real OCR (the OCR tier is a stub) |
| Native extraction for 14 formats + Markdown/text | Postgres, MinIO, durable jobs |
| Canonical JSON as the primary API | NATS, distributed workers |
| Markdown generated as a view | Vision-LLM fallback, PP-Structure |
| Bounding boxes, provenance, per-page quality scores | Page rasterization, engine benchmarks |
| Content-hash caching at page and document level | HTML input |

## Quick start

Needs Go 1.25+, Rust 1.97+, and `uv` (only for regenerating fixtures).

```bash
make build      # builds bin/dolico and the Rust shim
make run        # serves on 127.0.0.1:8080
make test       # Go + Rust test suites
make e2e        # every fixture through the HTTP API, validated against the schema
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
{"page": 1, "class": "text_based", "engine": "pdf-inspector", "score": 0.774}
{"page": 2, "class": "scanned",    "engine": "ocr-stub",      "score": 0.656}
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
Markdown view. **Rust** parses. There is no Python yet — that is where real OCR
will go.

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

```
cmd/dolico/                 API server
internal/canonical/         the canonical model — the contract everything meets
internal/engine/            Engine interface + registry
        ├── rustshim/       subprocess transport; native and PDF engines
        ├── ocrstub/        placeholder OCR tier
        ├── quality/        per-page scoring
        └── router/         the routing policy
internal/blob/              content-addressed filesystem store
internal/cache/             page-level result cache
internal/jobs/              in-memory job store + worker pool
internal/render/            canonical → Markdown
internal/api/               HTTP handlers
rust/dolico-rs/             the shim: anydoc + pdf-inspector → canonical JSON
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

## Testing

```bash
make test    # 47 Rust tests, ~90 Go tests
make e2e     # HTTP sweep with JSON Schema validation
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

The OCR tier is the gap. `internal/engine/ocrstub` implements the real `Engine`
interface and is wired into the real router, so the whole escalation path is
exercised — it just returns a synthetic block per page instead of reading
pixels. Replacing it with a PaddleOCR service means implementing three methods
over HTTP; the router does not change.

Two things that service will need which the stub does not: **page
rasterization** — neither Rust library renders, so it should rasterize with
`pypdfium2` from the original PDF bytes plus the page numbers, keeping pdfium's
native dependency out of both the shim and the Go binary — and real per-block
confidences.
