# Dolico OCR service

The OCR tier. Rasterizes the pages the router could not extract natively and
reads them.

It is deliberately optional: with no OCR service configured the Go API falls
back to a stub tier, so the rest of the system builds, runs and tests with no
Python installed.

## Two tiers

| | Tier 1 — `paddleocr` | Tier 2 — `pp-structurev3` |
| --- | --- | --- |
| Detects | text lines | layout regions: headings, paragraphs, figures, **tables** |
| A scanned table | 18 loose fragments | a 5×3 grid |
| Dependencies | base install | `+ paddlex[ocr]` (~150MB) |

Scored by `make bench-ocr` over the fixture corpus:

| | Tier 1 | Tier 2 |
| --- | --- | --- |
| mean CER | 0.0409 | **0.0074** |
| mean cell accuracy | 0.667 | **0.978** |
| `scanned-table.pdf` cells | 0.000 — no table found | **0.933** |
| total wall time | **5.8s** | 8.5s |

Tier 2 is the default whenever its dependencies are installed: it is better on
both accuracy axes for about 1.5× the time, and a table read as flat text is
*wrong* rather than merely uglier. Tier 1 remains the fallback, and the service
says loudly at startup when it has fallen back.

## Running it

```bash
make ocr                 # from the repository root; layout tier, on 127.0.0.1:8181
make ocr OCR_WORKERS=4   # four pages at once -- see Concurrency for the memory cost
make ocr-text            # text-line tier only
make run-ocr             # the API server, wired to whichever is running
```

The first start downloads models into `~/.paddlex` — about 50MB for Tier 1 and
a few hundred for Tier 2. Models load at startup, and `/healthz` returns 503
until they are ready, so an orchestrator will not route work to a replica that
is still warming up.

## API

| Method | Path | |
| --- | --- | --- |
| `POST` | `/v1/extract` | multipart: `file`, `pages` (comma-separated, 1-indexed, empty = all), `dpi` |
| `GET` | `/v1/version` | schema, service and engine versions, and the active tier |
| `GET` | `/healthz` | readiness, the active tier, and which models are loaded |

`/v1/version` reports the engine name for the tier actually serving —
`paddleocr` or `pp-structurev3`. The Go client adopts it as its engine name, so
provenance and cache keys follow the tier: switching tiers invalidates the
pages the other one produced rather than silently mixing them.

`/v1/extract` returns the same canonical extract envelope the Rust shim
produces, so the Go client parses both with one code path. Failures return the
same error envelope too: `{schema_version, kind, message}` with `kind` one of
`unsupported`, `malformed`, `resource_limit`.

```bash
curl -F file=@../../testdata/scanned.pdf -F pages=1 \
     localhost:8181/v1/extract | jq '.pages[0].blocks[]|{text,confidence,bbox}'
```

## What each tier produces

**Tier 1** is text lines grouped into paragraphs. PaddleOCR detects lines; this
service groups them by vertical gap and horizontal overlap, which is also what
keeps two columns apart — the grouper compares each line against every open
paragraph, not just the previous line, because in reading order the lines of two
columns interleave.

It produces no headings and no tables. Inferring those from font size would put
invented structure into the canonical model where a consumer could not
distinguish it from the real thing.

**Tier 2** gets that structure from a model rather than a guess. Each layout
region becomes a canonical block, and its original label rides along in
`provenance.method` — so a heading that came from `doc_title` is
distinguishable from one that came from `table_title`, and a label this service
has never heard of degrades to a paragraph without losing its text.

Recognized tables arrive as HTML and are converted into the canonical grid,
with merged cells becoming an origin plus `covered_by` shadow slots. Header
rows are reported only when the HTML says so: PP-Structure emits `<td>`
throughout, so a recognized table almost always reports `header_rows: 0`. The
Markdown view separately promotes the first row into the header position,
because GFM cannot express a headerless table — that is a rendering
concession, and the JSON stays exact.

Two things both OCR tiers provide that no other engine in the pipeline can:

- **Real per-block confidence.** A native parser reading DOCX XML is not "95%
  confident"; it is reading a data structure. OCR genuinely is.
- **Real page geometry.** `pdf-inspector` does not expose page dimensions, so
  natively-extracted PDF pages carry none. This service renders, so it knows.

## Configuration

| Variable | Default | |
| --- | --- | --- |
| `DOLICO_OCR_TIER` | `auto` | `auto`, `layout` or `text` |
| `DOLICO_OCR_WORKERS` | `1` | worker count reported to the client; set it to match `uvicorn --workers` |
| `DOLICO_OCR_LANG` | `en` | recognition language |
| `DOLICO_OCR_DET_MODEL` | `PP-OCRv5_mobile_det` | text detection model |
| `DOLICO_OCR_REC_MODEL` | `PP-OCRv5_mobile_rec` | text recognition model |
| `DOLICO_OCR_TABLE_ORIENTATION` | **off** | table orientation classification — see below |
| `DOLICO_OCR_TABLES` | on | table recognition (Tier 2) |
| `DOLICO_OCR_FORMULAS` | off | formula recognition (Tier 2) |
| `DOLICO_OCR_CHARTS` | off | chart recognition (Tier 2) |
| `DOLICO_OCR_SEALS` | off | seal recognition (Tier 2) |
| `DOLICO_OCR_REGIONS` | off | region detection (Tier 2) |
| `DOLICO_OCR_ORIENTATION` | off | document orientation classification |
| `DOLICO_OCR_UNWARP` | off | document unwarping |
| `DOLICO_OCR_TEXTLINE_ORIENTATION` | off | per-line orientation |
| `DOLICO_OCR_MAX_UPLOAD_BYTES` | 256MiB | per-request cap |
| `DOLICO_OCR_LAZY_LOAD` | off | skip loading models at startup |

### On table orientation classification

It is off by default because it is **actively wrong** on ordinary tables. With
it enabled, the repository's `scanned-table.pdf` comes back rotated 180°:

```
enabled:   ['6,480.00', '54', 'West']      <- last row first, columns reversed
disabled:  ['Region', 'Units', 'Revenue']  <- correct
```

Disabling it also roughly halves the time per page. Enable it only for a corpus
that genuinely contains rotated tables, and check the output when you do.

### On the default models

PaddleOCR would otherwise choose the `medium` variants, which take ~17.4s for a
letter page against the mobile ones' ~2.5s. Between mobile and *server*, scored
by `make bench-ocr` over the fixture corpus:

| | `PP-OCRv5_mobile` (default) | `PP-OCRv5_server` |
| --- | --- | --- |
| mean CER | 0.0074 | **0.0065** |
| mean cell accuracy | 0.978 | **1.000** |
| `scanned-table.pdf` CER / cells | 0.013 / 0.933 | **0.000 / 1.000** |
| `scanned.pdf` CER | **0.014** | 0.029 |
| total wall time | **8.5s** | 39.2s |

So the honest summary is narrower than "they are the same". The server models
read the scanned **table** perfectly where mobile drops a thousands separator
and one cell; mobile is marginally better on the simple invoice; and mobile is
**4.6× faster**. Mobile stays the default on that speed, but a deployment whose
documents are mostly tables should measure its own corpus and set
`DOLICO_OCR_DET_MODEL` / `DOLICO_OCR_REC_MODEL` to the server variants.

Both columns are three OCR documents of clean synthetic rendering. They
separate the models on this corpus; they do not predict accuracy on
photographs of real paper.

The document-level preprocessors are off because each is another model, and the
pages arriving here have already been identified as scans of ordinary documents
by `pdf-inspector`. Turn them on for photographed or skewed sources.

## Concurrency

One inference uses **about one core** and does not get faster with more.
Measured on a 16-core M-series machine, a letter page at 200 DPI:

| | Result |
| --- | --- |
| CPU during inference | ~105% of 1600% available |
| Paddle intra-op threads (1 / 4 / 8) | 1.76s / 1.76s / 1.76s — no scaling |
| 4 engine instances across 4 Python threads | **1.00×** — Paddle holds the GIL |

So threads are useless here and throughput comes from **processes**:

```bash
make ocr OCR_WORKERS=4
```

The service reports its worker count at `/v1/version` and the Go client sets
its request concurrency to match, so there is no second setting to keep in
sync. It splits a document's OCR pages into at most that many contiguous chunks
and sends them concurrently — chunks rather than one request per page, because
each request re-uploads the document.

Measured end to end on a 6-page scan:

| Workers | Wall time | Memory |
| --- | --- | --- |
| 1 | 14.1s | 3.1 GB |
| 4 | 5.8s (**2.4×**) | 12.3 GB |

It falls short of 4× only because six pages over four shards is [2,2,1,1], so
the critical path is a two-page chunk; the ratio approaches the worker count as
documents get longer.

**Budget roughly 3GB per worker.** That is the real constraint: the models are
about 1.5GB and the allocator arenas grow to ~3GB after the first inference,
then plateau. `OCR_WORKERS` defaults to 1 for that reason — raising it trades
memory for latency deliberately — and startup costs one model load per worker.

## Tests

```bash
make test-ocr    # pytest, plus the Go client against a live service
```

The Python tests replace both OCR engines with fakes, so they run in a couple
of seconds without loading a model. What they cover is the contract: the
canonical envelope, the error envelope, page filtering, table HTML conversion
including merged cells, reading order, and the pixel-to-point coordinate flip —
raster space is top-down and PDF space is bottom-up, and getting that wrong
yields boxes that are vertically mirrored and look almost right.

`make e2e-ocr` from the repository root exercises the real models end to end,
asserting that the scanned table comes back as a 5×3 grid with its header row
first and its columns in order.
