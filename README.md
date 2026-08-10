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
| Real OCR in two tiers, optional and pluggable | A benchmark corpus larger than one real scan |
| A vision-model third tier for pages OCR loses | Remote MinerU (`DOLICO_MINERU_URL` is written but untested) |
| Cross-engine disagreement to catch confident misreads | Authentication, rate limiting, tenancy |
| Layout analysis: scanned tables come back as grids | Blob retention — the store grows forever |
| Canonical JSON as the primary API | HTML input |
| Markdown generated as a view | |
| Parallel page OCR across worker processes | |
| Bounding boxes, provenance, per-page quality scores | |
| Content-hash caching at page and document level | |
| Docker Compose deployment for a single host | |

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

With the vision tier as well — heavier, and only worth starting if you have
pages OCR loses:

```bash
make ocr-vision  # the same service, plus MinerU (torch; ~3.2GB of weights, ~7GB RAM)
make run-vision  # the API server, with escalation to Tier 3 enabled
```

As two containers on one host, for an internal deployment:

```bash
make deploy-up   # API on 127.0.0.1:8080, OCR service alongside it
```

That publishes on loopback only, because **dolico has no authentication of any
kind** — [`deploy/README.md`](deploy/README.md) covers what has to sit in front
of it, how to size the OCR workers, and what does and does not survive a
restart.

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

The vision tier lives behind that same service and the same port, reached by a
`tier=vision` field on the same endpoint. It shares the OCR client's
concurrency budget rather than getting its own, because it occupies a service
worker exactly as an OCR request does — a separate budget would oversubscribe
the service by however many workers it was allowed.

```
cmd/dolico/                 API server
internal/canonical/         the canonical model — the contract everything meets
internal/engine/            Engine interface + registry
        ├── rustshim/       subprocess transport; native and PDF engines
        ├── paddleocr/      HTTP client for the OCR and vision tiers
        ├── ocrstub/        fallback OCR tier when none is configured
        ├── quality/        per-page scoring
        └── router/         the routing policy
internal/blob/              content-addressed filesystem store
internal/cache/             page-level result cache
internal/jobs/              in-memory job store + worker pool
internal/render/            canonical → Markdown
internal/api/               HTTP handlers
rust/dolico-rs/             the shim: anydoc + pdf-inspector → canonical JSON
python/ocr-service/         PaddleOCR and MinerU over HTTP (see its own README)
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

**Asserted confidence is ignored; measured confidence is decisive.** A PDF with
a broken font encoding extracts "text" with complete confidence and produces
mojibake — a parser reading a data structure is not 95% sure of anything, and
it reports no per-block confidence at all. For those pages the score combines
text density, replacement-character ratio and how word-like the output is, and
the engine's self-report is the smallest of four weights. A page scoring below
`DOLICO_OCR_THRESHOLD` is re-extracted by the OCR tier.

OCR is the opposite case: it genuinely measures, per block, and the canonical
model already records that. So for a page whose text came from blocks reporting
a confidence, the three text signals become a ceiling that the measurement
scales down. They have to: OCR emits no U+FFFD, and "Rcglon Unlts" is exactly
as word-like as "Region Units" to any language-agnostic test, so as a weighted
term confidence could not pull a page with text below 0.55 however unsure the
engine was — and every vision threshold is below that.

**The vision tier is the same mechanism, one notch lower.** A page the OCR tier
produced and that still scores below `DOLICO_VISION_THRESHOLD` (0.35, and
validated to be strictly below the OCR threshold) is read again by MinerU. Three
things keep it from becoming the default tier: it is off unless asked for, it
sees only pages OCR already handled, and at most `DOLICO_VISION_MAX_PAGES` of
them per document — worst-scoring first, and the router logs what the cap
dropped. On success the page is replaced outright and re-scored; on failure or
an empty read the OCR result stands and the page is tagged `vision_failed` or
`vision_empty`, because a page that scored 0.2 is still worth more than nothing.
[`docs/vision-tier-design.md`](docs/vision-tier-design.md) records why MinerU
rather than a hosted vision LLM, and what that choice costs.

**A second engine is what catches OCR that is wrong and sure of itself.** On a
real 1922 newspaper scan the OCR tier gets 54% of the words wrong — `11:13` for
`11:15`, `Ho8nasne` for `Hog flash` — and reports **0.938 confidence** while
doing it. That page scores 0.61, so no threshold below the 0.60 OCR bar could
ever select it, and no signal computed from the page disagrees with the engine
that produced it.

So one page of every document that used OCR is read again by the vision tier
and the two results are compared. Agree, and the probe is discarded and the
document keeps one engine throughout. Disagree past
`DOLICO_VISION_DISAGREEMENT`, and the OCR tier is distrusted for the *whole*
document — a scan bad enough to fool OCR on one page is rarely fine on the
others. The 0.05 default sits in a measured gap: the worst page where the two
tiers agreed scored 0.034, the best page where they genuinely disagreed scored
0.092.

It is a fixed toll, and the README should say so plainly: about 4–5s per
document with scanned pages, +36% wall time on a corpus where nothing needed
it, in exchange for `radio-1922.pdf` going from 0.540 word error to 0.016.
`DOLICO_VISION_PROBE=0` turns it off.

**OCR parallelism is processes, not threads.** One inference uses about one
core and scales with neither Paddle's intra-op threading nor Python threads —
a four-thread pool measures at exactly 1.00×, because Paddle holds the GIL
throughout. So the Go client shards a document's OCR pages across concurrent
requests, bounded by the worker count the service reports. On a 6-page scan,
four workers take 5.8s against 14.1s — at 12.3GB against 3.1GB, which is why
it is opt-in.

**Caching is keyed per page**, on document hash + engine + engine version +
pipeline version + configuration. An engine upgrade re-runs only that engine's
pages. Above it, a document-level short-circuit skips work entirely when the
same bytes have already been processed by the current schema and pipeline.

**A document stored during an outage does not count as processed.** When a tier
is down its pages come back empty and marked `ocr_failed`, and the document is
still stored — returning a partial document beats failing the request. But the
short-circuit asks whether the document is *finished*, not merely present, so
those bytes are reprocessed on the next upload instead of being served the
outage forever. The line it draws is whether a page is missing content an engine
was supposed to produce: a page the OCR tier read and found blank is done, a
page it never managed to read is not.

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
| `DOLICO_OCR_CONCURRENCY` | service's worker count | OCR requests in flight at once |
| `DOLICO_VISION_ENABLED` | off | escalate bad OCR pages to the vision tier |
| `DOLICO_VISION_THRESHOLD` | `0.35` | page quality below which vision is tried |
| `DOLICO_VISION_MAX_PAGES` | `5` | vision escalations per document |
| `DOLICO_VISION_PROBE` | on | read one page of every OCR'd document with the vision tier and compare |
| `DOLICO_VISION_DISAGREEMENT` | `0.05` | how far the tiers must differ before OCR is distrusted document-wide |

Setting `DOLICO_OCR_URL` to a service that is not reachable is a startup
failure, not a silent fallback: a deployment configured for OCR that quietly
serves stub text would be worse than one that refuses to start. The OCR
service has [its own configuration](python/ocr-service/README.md#configuration).

`DOLICO_VISION_ENABLED` is the opposite: asking for a tier the service does not
have is a warning and a two-tier run, not a startup failure. Tier 3 is an
optional escalation, and refusing to serve documents because an optional
recovery path is missing would trade a small loss for a total one.

## Testing

```bash
make test       # 48 Rust tests, ~193 Go tests
make e2e        # HTTP sweep with JSON Schema validation
make test-ocr   # 148 Python tests, plus the Go client against a live OCR service
make e2e-ocr    # the sweep again, asserting real OCR read the scanned pages
make e2e-vision # ...and that faded.pdf escalated to the vision tier and was recovered
make bench-ocr  # score extraction against ground truth
```

## Benchmarking

`make bench` scores extraction against `testdata/ground-truth.json`: character
and word error rate for text, cell accuracy for tables, wall time per document.
The fixtures are generated, so the expectation is exact rather than
transcribed — `scripts/gen-testdata.py` writes the ground truth from the same
constants it draws from.

```
document               pages     CER     WER   cells      ms  engines
faded.pdf                  1   1.000   1.000    -       2974  pp-structurev3
scanned-table.pdf          1   0.013   0.125   0.933    7092  pp-structurev3
scanned.pdf                1   0.014   0.182    -       3339  pp-structurev3
text.pdf                   2   0.000   0.000    -          24  pdf-inspector
mean CER               0.1315   mean cell accuracy 0.9778
```

It runs against a fresh data directory each time, because the document-level
cache would otherwise short-circuit the second run and measure nothing.

`faded.pdf` is supposed to look like that. It is the fixture the vision tier
exists for, and `make bench-vision` is the same run with Tier 3 enabled:

| | `make bench-ocr` | `make bench-vision` |
| --- | --- | --- |
| `faded.pdf` CER | 1.000 | **0.019** |
| mean CER | 0.1315 | **0.0088** |
| total wall time | 16.8s | 24.9s |

**These numbers say the pipeline is wired correctly, not that OCR is accurate.**
Most of the fixtures are clean synthetic renderings; photographs of creased
paper score far worse. `make bench-hard` runs the same harness over
[`testdata/corpus-hard`](testdata/corpus-hard/PROVENANCE.md) — one real 1922
newspaper column off Library of Congress microfilm, with hand-transcribed
ground truth, kept separate precisely because its expectation is transcribed
rather than generated:

| | CER | WER |
| --- | --- | --- |
| Tier 2, `pp-structurev3` | 0.091 | 0.540 |
| Tier 3, `mineru` | **0.005** | **0.016** |

`--corpus /path/to/real/documents` points the harness at any directory holding
documents and a `ground-truth.json`, which is what would actually settle the
model and threshold defaults.

The Go tests for `rustshim` and `api` drive the **real** shim against the
**real** fixtures — a mock would test nothing that matters about talking to two
third-party libraries. `scripts/e2e_check.py` is the only place a genuine JSON
Schema validator runs over produced documents, which is what catches drift
between `schema/canonical-v1.json` and its Go and Rust mirrors.

Fixtures in `testdata/` are committed and regenerated by
`scripts/gen-testdata.py` (`make testdata`). The PDFs are the ones that matter:
`text.pdf` (all text), `scanned.pdf` (image only, no text operators),
`mixed.pdf` (one of each), `scanned-table.pdf` (a table drawn as pixels),
`faded.pdf` (a scan OCR cannot read), plus `corrupt.pdf` for the error path.
Regenerating the PDFs produces no diff unless something actually changed —
`invariant=1` keeps reportlab from stamping a timestamp — but the DOCX, XLSX
and PPTX fixtures still differ on every run, because their libraries write
times into the package.

## Licensing of what this depends on

Checked against the installed versions rather than the project pages, because
the wheel is what actually runs here.

| | License | |
| --- | --- | --- |
| `anydoc`, `pdf-inspector` (Rust) | MIT | |
| PaddleOCR / PaddleX (Tier 1–2) | Apache-2.0 | |
| **MinerU 3.4.4** (Tier 3 code) | **Apache-2.0 + additional terms** | see below |
| MinerU2.5-Pro-2605-1.2B (Tier 3 weights) | Apache-2.0 | plain, no additional terms |
| everything else in the Python venv | MIT / BSD / Apache / MPL / PSF | 4 LGPL, no GPL, AGPL or SSPL |

MinerU is the only dependency that is not a stock permissive license. It
declares `LicenseRef-MinerU-Open-Source-License`: Apache-2.0 plus three terms,
which are short enough to state in full rather than paraphrase away.

1. **Commercial thresholds.** Commercial use needs no separate license until
   you and your affiliates, consolidated, exceed **100M monthly active users**
   *or* **USD 20M monthly revenue**. Past either, you must obtain a commercial
   license from the MinerU team before continuing.
2. **Attribution for online services.** If you provide an online service to
   third parties based on MinerU, you must indicate clearly and prominently —
   in the service interface or in public documentation — that MinerU is used.
3. **Automatic termination** if you breach either, with no notice required.

Nothing here restricts field of use, requires reciprocal licensing, or blocks
commercial deployment at any plausible size. It is a "get big, then call us"
license.

Term 2 is the one that applies to this repository, and it did not hold when it
was checked: the vision tier is not in the engine registry, so `/v1/engines`
listed every engine *except* the one that had produced the page you were
looking at. It is listed now. If you deploy this as a service, that endpoint
and this README are what carry the attribution — keep them.

**This repository is MIT** — see [`LICENSE`](LICENSE). That covers the code
here and nothing else: MinerU is an optional extra the operator installs, not
something this repository vendors or relicenses, so its additional terms attach
to whoever runs the service rather than to this code.

One loose end: the four LGPL packages (`cssutils`, `encutils`, `crc32c`,
`python-bidi`) are fine as unmodified, dynamically-imported libraries, but would
need a look before anyone vendors or patches them.

## Next

**Persistence** is now the largest gap. Postgres for jobs and page metadata,
MinIO behind the existing `blob.Store` interface — nothing outside that package
knows where bytes live. It also unlocks partial reprocessing after an engine
upgrade: the page-level cache key already exists, it just has nowhere durable
to look.

**A real corpus** is what the benchmark now needs. The harness has earned its
keep twice: it showed the server OCR models recover a scanned table perfectly
where the mobile default drops a cell, and it settled whether MinerU should be
the default OCR tier (it reads twenty times better and is not slower — and it
stays Tier 3 anyway, for reasons in
[the design doc](docs/vision-tier-design.md#should-mineru-be-tier-2)). But it
is scoring eight generated documents and one real scan. Point `--corpus` at a
real collection and the model choice, the escalation threshold, and the four
weights in `internal/engine/quality` become measurable rather than argued.

**Tuning the probe** is what the disagreement check needs next. It works — the
real scan recovers on production defaults — but its threshold, and the decision
to escalate a whole document from one page, rest on five pages of evidence. The
open questions are all measurable with a bigger corpus: how often it fires on
documents that were fine, whether one probe page is enough for a long document,
and whether the toll should scale with page count rather than being paid once.

**Cheaper triage** would take the toll off documents that obviously do not need
it. The service renders every OCR page already, so contrast, ink coverage and
noise are free to measure — enough to skip the probe on a clean page, or to
force it on a visibly degraded one before OCR has said anything at all.

Smaller: HTML input, and the page cache clears wholesale at its limit rather
than evicting LRU (deliberate, and moot once values live in a real store).
