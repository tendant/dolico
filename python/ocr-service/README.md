# Dolico OCR service

The OCR tier. Rasterizes the pages the router could not extract natively and
reads them with PaddleOCR.

It is deliberately optional: with no OCR service configured the Go API falls
back to a stub tier, so the rest of the system builds, runs and tests with no
Python installed.

## Running it

```bash
make ocr        # from the repository root; serves on 127.0.0.1:8181
make run-ocr    # the API server, wired to it
```

The first start downloads the PaddleOCR models (~50MB) into `~/.paddlex`.
Models load at startup, and `/healthz` returns 503 until they are ready — so an
orchestrator will not route work to a replica that is still warming up.

## API

| Method | Path | |
| --- | --- | --- |
| `POST` | `/v1/extract` | multipart: `file`, `pages` (comma-separated, 1-indexed, empty = all), `dpi` |
| `GET` | `/v1/version` | schema, service and engine versions |
| `GET` | `/healthz` | readiness, plus which models are actually loaded |

`/v1/extract` returns the same canonical extract envelope the Rust shim
produces, so the Go client parses both with one code path. Failures return the
same error envelope too: `{schema_version, kind, message}` with `kind` one of
`unsupported`, `malformed`, `resource_limit`.

```bash
curl -F file=@../../testdata/scanned.pdf -F pages=1 \
     localhost:8181/v1/extract | jq '.pages[0].blocks[]|{text,confidence,bbox}'
```

## What it does, and does not, produce

Tier 1 is **text lines grouped into paragraphs**. PaddleOCR detects lines; this
service groups them by vertical gap and horizontal overlap, which is also what
keeps two columns apart — the grouper compares each line against every open
paragraph, not just the previous line, because in reading order the lines of two
columns interleave.

It produces no headings, no tables and no reading-order analysis. Inferring
those from font size would put invented structure into the canonical model
where a consumer could not distinguish it from the real thing. That is layout
analysis, and it belongs to PP-StructureV3 in the next tier.

Two things this tier provides that no other engine in the pipeline can:

- **Real per-block confidence.** A native parser reading DOCX XML is not "95%
  confident"; it is reading a data structure. OCR genuinely is.
- **Real page geometry.** `pdf-inspector` does not expose page dimensions, so
  natively-extracted PDF pages carry none. This service renders, so it knows.

## Configuration

| Variable | Default | |
| --- | --- | --- |
| `DOLICO_OCR_LANG` | `en` | recognition language |
| `DOLICO_OCR_DET_MODEL` | `PP-OCRv5_mobile_det` | text detection model |
| `DOLICO_OCR_REC_MODEL` | `PP-OCRv5_mobile_rec` | text recognition model |
| `DOLICO_OCR_ORIENTATION` | off | document orientation classification |
| `DOLICO_OCR_UNWARP` | off | document unwarping |
| `DOLICO_OCR_TEXTLINE_ORIENTATION` | off | per-line orientation |
| `DOLICO_OCR_MAX_UPLOAD_BYTES` | 256MiB | per-request cap |
| `DOLICO_OCR_LAZY_LOAD` | off | skip loading models at startup |

### On the default models

PaddleOCR would otherwise choose the `medium` variants. Measured on this
machine (M-series CPU, letter page at 200 DPI):

| Models | Time per page |
| --- | --- |
| `PP-OCRv6_medium` | ~17.4s |
| `PP-OCRv5_mobile` | ~2.5s |

On the repository's `scanned.pdf` fixture the two return character-identical
text at comparable confidence, which is why `mobile` is the default. That is
**one clean synthetic page, not a benchmark** — a corpus of real scans may well
separate them. A deployment that cares more about accuracy than latency should
measure its own documents and set `DOLICO_OCR_DET_MODEL` / `DOLICO_OCR_REC_MODEL`.

The document-level preprocessors are off because each is another model, and the
pages arriving here have already been identified as scans of ordinary documents
by `pdf-inspector`. Turn them on for photographed or skewed sources.

## Concurrency

PaddleOCR's predictor is not safe to call from several threads, so every
prediction is serialized behind a lock. This service is one-inference-at-a-time
by construction; scale it with replicas rather than threads, which is also what
a GPU deployment wants.

## Tests

```bash
make test-ocr    # pytest, plus the Go client against a live service
```

The Python tests replace the OCR engine with a fake, so they run in under a
second without loading a model. What they cover is the contract: the canonical
envelope, the error envelope, page filtering, and the pixel-to-point coordinate
flip — raster space is top-down and PDF space is bottom-up, and getting that
wrong yields boxes that are vertically mirrored and look almost right.
