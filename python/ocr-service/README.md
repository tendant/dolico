# Dolico OCR service

The OCR tier. Rasterizes the pages the router could not extract natively and
reads them.

It is deliberately optional: with no OCR service configured the Go API falls
back to a stub tier, so the rest of the system builds, runs and tests with no
Python installed.

## Three tiers

| | Tier 1 — `paddleocr` | Tier 2 — `pp-structurev3` | Tier 3 — `mineru` |
| --- | --- | --- | --- |
| Detects | text lines | layout regions: headings, paragraphs, figures, **tables** | the whole page, by a 1.2B vision model |
| A scanned table | 18 loose fragments | a 5×3 grid | a 5×3 grid |
| Dependencies | base install | `+ paddlex[ocr]` (~150MB) | `+ mineru[core]` (torch, ~2.5GB of weights) |
| Selected by | the router, per page | the router, per page | the **router's escalation**, per page, after Tier 1/2 produced a bad read |
| Cost | ~2.5s/page | ~3-8s/page | ~2-9s/page warm, plus ~6s of warm-up on the first call in a process |

Tiers 1 and 2 are alternatives: one of them serves every OCR request, and which
one depends only on what is installed. Tier 3 is not an alternative — it is a
second chance for individual pages the chosen OCR tier scored badly on, and it
is off unless the API server is started with `DOLICO_VISION_ENABLED=1`. See
`docs/vision-tier-design.md` for why it exists and when it fires.

Tier 3 reads *better* than Tier 2 on every page of the benchmark corpus — mean
CER 0.011 against 0.234 — and warm it is not slower. It stays a fallback anyway,
for reasons that are about the pipeline rather than the model: it reports no
per-block confidence, so promoting it would delete the only signal that can
catch a bad read; it would leave nothing to escalate *to*; and it returns
columns of plain text as tables, which is structure the document does not have.
That argument is made properly in the design doc under *Should MinerU be
Tier 2?*.

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
make ocr-vision          # layout tier plus MinerU, so Tier 3 is reachable
make run-ocr             # the API server, wired to whichever is running
make run-vision          # the same, with the vision escalation enabled
```

The first start downloads models into `~/.paddlex` — about 50MB for Tier 1 and
a few hundred for Tier 2. Models load at startup, and `/healthz` returns 503
until they are ready, so an orchestrator will not route work to a replica that
is still warming up.

## API

| Method | Path | |
| --- | --- | --- |
| `POST` | `/v1/extract` | multipart: `file`, `pages` (comma-separated, 1-indexed, empty = all), `dpi`, `tier` |
| `GET` | `/v1/version` | schema, service and engine versions, the active tier, and whether vision is installed |
| `GET` | `/healthz` | readiness, the active tier, and which models are loaded |

`/v1/version` reports the engine name for the tier actually serving —
`paddleocr` or `pp-structurev3`. The Go client adopts it as its engine name, so
provenance and cache keys follow the tier: switching tiers invalidates the
pages the other one produced rather than silently mixing them.

`tier=vision` routes the request to Tier 3 instead, which answers as engine
`mineru` with its own version. It requires explicit page numbers — Tier 3 is a
per-page escalation, and a whole-document vision request is almost always a
mistake, so it is refused with a 400 rather than served. A page the vision
model fails on is skipped and the rest are returned; a request where *every*
page failed is a 422. `vision_available` is advertised separately from the OCR
tier so a client can decide whether Tier 3 exists without attempting it.

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

**Tier 3** reads the page image directly with MinerU2.5-Pro-2605-1.2B, a 1.2B vision model,
and returns the same block types — except that a table with one column comes
back as paragraphs. MinerU labels narrow columns of ordinary text as tables:
the faded receipt arrives as 8×1 and the 1922 newspaper column as 9×1, while
the fixture that really is a table arrives as 5×3. A one-column grid carries no
structure a stack of paragraphs does not, so it is flattened rather than
passed on as structure the document never had. Its geometry arrives normalized to a
0–1000 box measured from the top-left, so this service scales it to the page's
point size and flips the vertical axis — the same conversion the OCR tiers do
from pixels, and wrong in the same visually-plausible way if skipped.

Two differences from the OCR tiers are worth knowing:

- **No confidence.** MinerU does not report one, and this service does not
  invent one. Tier 3 blocks carry no `confidence` field rather than a fabricated
  `1.0` that would outrank a genuinely-measured OCR score.
- **Reading order is recovered, not reported.** MinerU's output order is not
  reading order, so blocks are sorted top-to-bottom then left-to-right here.

Two things both OCR tiers provide that no other engine in the pipeline can:

- **Real per-block confidence.** A native parser reading DOCX XML is not "95%
  confident"; it is reading a data structure. OCR genuinely is — and the router
  scores an OCR page by that number, because none of the signals computable
  from the text can tell a good read from a confident misread.

  It is worth knowing how far that goes. On `testdata/corpus-hard/radio-1922.pdf`,
  a real microfilm scan, Tier 2 gets 54% of the words wrong and reports **0.938
  confidence**. Low confidence means the page is bad; high confidence does not
  mean it is good. That asymmetry is why the router also asks Tier 3 to read one
  page of every OCR'd document and compares the two — the only way to catch a
  confident misread is another engine.
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
| `DOLICO_MINERU_BACKEND` | `hybrid-engine` | Tier 3 backend — see below |
| `DOLICO_MINERU_EFFORT` | `medium` | MinerU inference effort |
| `DOLICO_MINERU_URL` | unset | run MinerU as its own service and only talk to it |

### On the MinerU backend

`hybrid-engine` is the default and is load-bearing, not a preference. It is the
only backend measured here that reads `scanned-table.pdf` completely correctly;
`pipeline` is a different arrangement of the same model families Tier 2 already
uses, so it tends to fail the same pages Tier 3 was called in to rescue.

Setting `DOLICO_MINERU_URL` switches to the matching HTTP-client backend
(`hybrid-engine` → `hybrid-http-client`) and keeps ~8GB of weights out of a
service already measured at ~3GB per worker. `pipeline` has no remote form and
is rejected when a URL is set.

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

The Python tests replace all three engines with fakes, so they run in a couple
of seconds without loading a model. What they cover is the contract: the
canonical envelope, the error envelope, page filtering, table HTML conversion
including merged cells, reading order, and the pixel-to-point coordinate flip —
raster space is top-down and PDF space is bottom-up, and getting that wrong
yields boxes that are vertically mirrored and look almost right.

`make e2e-ocr` from the repository root exercises the real models end to end,
asserting that the scanned table comes back as a 5×3 grid with its header row
first and its columns in order.
