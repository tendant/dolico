# Vision Tier (Tier 3) — Design

Status: **implemented and measured.** Engine: **MinerU2.5, self-hosted.**
See *Implementation status* and *What the tier is worth, measured* below; the
sections before them are the original design, kept as written except where a
measurement contradicted them, which is noted where it happened.

The design document's third OCR tier: a vision model that reads pages the OCR
tiers cannot. Everything below was measured on this machine rather than
recalled — MinerU was installed and run against the repository's own fixtures
before this design was written.

## Why a third tier

Tier 1 (text-line OCR) and Tier 2 (layout analysis) fail the same way: they are
trained on documents that look like documents, and degrade on handwriting,
noise, low contrast, dense or unusual layouts, and structure implied by
whitespace rather than drawn.

Our benchmark puts Tier 2 at a mean CER of 0.0074, but that is measured on
clean synthetic renderings — it says the pipeline is wired correctly, not that
it survives a photograph of creased paper.

## Engine: MinerU2.5, hybrid backend

Chosen for a reason the rest of this system already reflects: it runs locally,
costs nothing per page, and sends no document anywhere. Every component so far
holds that line, and a hosted API would have broken it.

**The backend choice is the load-bearing part of this design.** MinerU ships
several, and they are not interchangeable here:

| Backend | What it is | Verdict |
| --- | --- | --- |
| `pipeline` | PP-OCR based, CPU | **Wrong choice for Tier 3** — the same model family as our Tier 2, so it fails for the same reasons |
| `hybrid-engine` | 1.2B VLM + native text extraction | **Use this.** Genuinely different from Tier 2; the default |
| `vlm-engine` | Standalone VLM | Viable; higher hardware demand, no measured advantage here |
| `*-http-client` | Points at a MinerU server | The scaling path — see Deployment |

A tier that reruns the same class of model that just failed is not a fallback.
Picking `hybrid-engine` is what makes this a real third tier rather than a
second attempt at the second one.

## Measured on this machine

M-series Mac, no CUDA, against `testdata/scanned-table.pdf` — a ruled table
drawn as pixels with no text operators.

| | `All figures are unaudited.` | Table cell | Result |
| --- | --- | --- | --- |
| Tier 2 (PP-StructureV3, mobile) | `Allfigures are unaudited.` | `24360.00` | cells 0.933 |
| MinerU `pipeline` | `All figuresare unaudited.` | `24,360.00` | table correct, text still merged |
| MinerU `hybrid-engine` | `All figures are unaudited.` ✓ | `24,360.00` ✓ | **fully correct** |

The hybrid backend is the only engine tested that gets the whole page right.

**Cost, in resources rather than money:**

| | |
| --- | --- |
| Install | `uv pip install "mineru[core]"` — 106 packages, ~1.1GB venv |
| Model cache | ~8.3GB on first run, one time |
| Warm latency | 14s (`pipeline`) / 16s (`hybrid-engine`) per page via CLI |
| Actual inference | ~7s for the hybrid VLM; the remaining ~9s is CLI/server startup |
| Apple Silicon | Works — both backends ran, no CUDA |

That ~9s of per-invocation overhead is why the CLI is not the integration
point.

## Integration

**In-process, via `mineru.cli.common.do_parse`.** Signature confirmed against
the installed package:

```python
do_parse(output_dir, pdf_file_names, pdf_bytes_list, p_lang_list,
         backend="hybrid-engine", start_page_id=0, end_page_id=None,
         effort="medium", server_url=None, ...)
```

There is also `aio_do_parse`, which suits the service's async handlers.

It writes results to a directory rather than returning them, so the adapter is:
temp dir → `do_parse` → read `content_list.json` → map to canonical → clean up.
Slightly awkward, and much cheaper than paying CLI startup per page.

`start_page_id`/`end_page_id` take a **contiguous** range, but escalated pages
are typically scattered (page 2 and page 7, not 2–7). The adapter therefore
issues **one call per escalated page**. At ~7s of inference that is acceptable
for a tier that only fires on pages the others already lost.

### Deployment

Two modes, mirroring MinerU's own engine/http-client split and the pattern this
repo already uses for its optional tiers:

- **In-process** (default): the OCR service loads MinerU itself. One
  deployment, and the model's memory adds to the service's own.
- **Remote** (`DOLICO_MINERU_URL` set): MinerU runs as its own service —
  it ships a FastAPI server at `mineru.cli.fast_api` — and the adapter uses the
  `hybrid-http-client` backend against it.

The remote mode matters more than it looks. The OCR service already measures
~3GB per worker, and we found four workers costs 12.3GB; adding an ~8GB model
into that process multiplies badly. Keeping MinerU in its own process, on its
own box or GPU, is the sane production shape.

## Output mapping

MinerU's `content_list.json` maps onto the canonical model almost directly, and
better than the Claude design would have:

| MinerU | Canonical |
| --- | --- |
| `type: text` with `text_level` | `heading` at that level |
| `type: text` | `paragraph` |
| `type: header` / `footer` | `paragraph` (label kept in provenance) |
| `type: table`, `table_body` (HTML) | `table` — **reuses `tables.py`**, already written for PP-Structure |
| `type: list` | `list` |
| `type: equation` | `formula` |
| `type: image` | `image` |
| `bbox` | `bbox`, converted (see below) |

**Bounding boxes are real and are kept.** `content_list.json` gives
`[x0,y0,x1,y1]` normalized to 0–1000 with a top-left origin;
`middle.json` carries `page_size` in true PDF points. Cross-checking the table
on our fixture against Tier 2's output, both land at x≈70–483 with a top edge
at y≈650 — the conversion is confirmed, not assumed.

This reverses the decision the hosted-model design had to make. A reasoning
model asked for pixel coordinates invents them; MinerU measures them. Escalating
a page to Tier 3 therefore **keeps** its geometry.

**Per-block confidence is not available** and is omitted rather than invented —
`content_list.json` reports none. Quality scoring still works; it scores text.

## Escalation

Unchanged from the reviewed design, and reusing machinery that already exists.

- Trigger: page quality below **`DOLICO_VISION_THRESHOLD`, default 0.35** —
  a second, lower bar than the 0.60 that already escalates native → OCR.
- The strongest signal remains a page scoring **0**: OCR returned nothing from
  a page the classifier said has content.
- Bounded per document by **`DOLICO_VISION_MAX_PAGES`, default 5**; when more
  pages qualify, the worst scorers go and the number dropped is logged.
- **Arbitration: the MinerU result replaces the OCR result** when the call
  succeeds. On failure the OCR result stands untouched, with the reason
  recorded. No scoring contest between engines.

## Failure handling

The governing principle is unchanged: **Tier 3 must never make a page worse.**

| Failure | Behavior |
| --- | --- |
| MinerU not installed | Tier reports unavailable; the router never escalates |
| Import or model-load failure | Logged loudly at startup; tier unavailable |
| Parse error or timeout on a page | Keep the OCR result; record `vision_failed` |
| Empty result for a page | Keep the OCR result; record `vision_empty` |
| More pages qualify than the cap | Escalate the worst scorers; log the number dropped |

## Configuration

| Variable | Default | |
| --- | --- | --- |
| `DOLICO_VISION_ENABLED` | off | master switch |
| `DOLICO_VISION_THRESHOLD` | `0.35` | page quality below which OCR escalates |
| `DOLICO_VISION_MAX_PAGES` | `5` | per-document escalation cap |
| `DOLICO_MINERU_BACKEND` | `hybrid-engine` | see the backend table above |
| `DOLICO_MINERU_EFFORT` | `medium` | MinerU's own effort knob (`medium`/`high`) |
| `DOLICO_MINERU_URL` | unset | when set, use the remote MinerU server |

## Verification

**Everything is testable here** — which is the other thing the self-hosted
choice buys. The hosted design would have shipped with its live path exercised
only by a test that skips without credentials; this one has no credentials to
lack. The adapter, the canonical mapping, the coordinate conversion, escalation
arithmetic, thresholds, caps and every failure path can all be tested against
the real engine, and `make bench` can score it.

## Implementation status

Built and green. `python/ocr-service/dolico_ocr/vision.py` plus the `tier=vision`
path on `/v1/extract`; `internal/engine/paddleocr/vision.go` and the second
escalation stage in `internal/engine/router`. 141 Python tests, ~170 Go tests,
including a live test against a real MinerU that runs whenever
`make ocr-vision` is up and skips otherwise.

What the live path actually returns for `scanned-table.pdf`, through the HTTP
API: engine `mineru` 3.4.4, four blocks in reading order, a perfect 5×3 grid
including the `6,480.00` cell the mobile OCR models drop a separator from, and
a table bbox of x=70 y=433 412×217 — matching Tier 2's independent measurement
of the same table, which is what confirms the 0–1000-to-points conversion and
the vertical flip. 16s cold, ~5s warm.

Two things changed during implementation:

- **The vision tier's version is not the OCR tier's.** It starts unknown and is
  adopted from the first real answer. It is part of the page cache key, so
  labelling MinerU output with PaddleOCR's version would make a MinerU upgrade
  invisible to the cache.
- **A page escalated to OCR was keeping its pre-OCR quality score.** Harmless
  until Tier 3 read that number to choose candidates — it would have escalated
  pages OCR had rescued and skipped pages OCR had ruined. Found by reading a
  forced-escalation log where a page reported the same score before and after
  three different engines touched it. Fixed, with a regression test.

## Recorded concerns

**1. My "same class of tool" objection, and what happened to it.** I argued
against MinerU on the grounds that a document parser will fail the pages
another document parser failed. Measurement partly refuted that: the hybrid
backend read our fixture *correctly* where Tier 2 did not. The objection does
hold against the `pipeline` backend, which is PP-OCR based — which is why this
design specifies `hybrid-engine` and treats that as load-bearing rather than a
default worth accepting silently.

**2. MinerU may be a better Tier 2 than PP-StructureV3.** Settled: it reads
better than PP-StructureV3 on every page of this corpus, and warm it is not
slower. See the section below — the conclusion is not the one this concern
anticipated, because "reads better" turned out not to settle "should be Tier 2".

**3. The license is not plain Apache.** It is the "MinerU Open Source License,
based on Apache 2.0 with additional conditions." Worth reading those conditions
before this ships.

## What the tier is worth, measured

Previously parked for want of a fixture. There are two now, and they give the
tier its first numbers in this repository.

**`testdata/faded.pdf`** — generated, so its ground truth is exact. A shipping
receipt rendered as an exhausted photocopy: legible to a person, and
PP-StructureV3 returns a single character from it. Scored by `make bench-ocr`
against `make bench-vision`:

| | two tiers | three tiers |
| --- | --- | --- |
| `faded.pdf` CER | 1.000 | **0.019** |
| corpus mean CER | 0.1315 | **0.0088** |
| total wall time | 16.8s | 24.9s |

Tier 3 fires on one page of eight documents and costs 8.1s to do it.

**`testdata/corpus-hard/radio-1922.pdf`** — a real 1922 newspaper column off
Library of Congress microfilm, transcribed by hand, kept out of the default
corpus for that reason. It answers whether the generated fixture was only hard
in a way we invented:

| | CER | WER |
| --- | --- | --- |
| Tier 2, `pp-structurev3` | 0.091 | 0.540 |
| Tier 3, `mineru` | **0.005** | **0.016** |

More than half the words wrong: `11:13` for `11:15`, `4t0` for `4 to`,
`Ho8nasne` for `Hog flash`. The vision tier misses one letter on the page.

## The escalation trigger is weaker than the tier

The same measurements found the limit, and it is worth stating plainly because
it is not what this design assumed.

**PaddleOCR reported 0.938 confidence on the page it got 54% of the words wrong
on.** With the production thresholds that page scores about 0.61 and Tier 3 is
never called; `make bench-hard` has to force escalation to measure anything.
The generated fixture escalates on the real defaults only because OCR failed
*visibly* there — one character at 0.49 confidence.

So the two failure modes are not equally reachable:

| OCR failure | Detectable? | Why |
| --- | --- | --- |
| returned nothing | yes | zero characters scores 0 |
| returned little, unsure | yes | the measured confidence carries the score down |
| returned plenty, wrong, sure | **no** | every available signal says the page is fine |

The scorer was reworked as part of this work to make the middle row reachable
at all — before it, no OCR page with text on it could score below 0.55, so no
threshold under the 0.60 OCR bar could select one. That fix is real and it is
what makes `faded.pdf` escalate. It does not touch the third row.

Closing that would take a second opinion rather than a better signal: a cheap
disagreement check between two engines on the same page, or a lexicon, which
this pipeline has deliberately avoided because it is language-specific. Both
are larger than a threshold change, and neither is in this design.

## Should MinerU be Tier 2?

Recorded concern #2, settled by measurement. Every OCR page in the corpus was
posted to the same service twice, at the default tier and at `tier=vision`, and
scored with the benchmark's own scoring code. Second run, both models warm:

| page | `pp-structurev3` CER / WER | `mineru` CER / WER |
| --- | --- | --- |
| `scanned.pdf` | 0.014 / 0.182 | 0.014 / 0.182 |
| `scanned-table.pdf` | 0.013 / 0.125 | **0.000 / 0.000** |
| `mixed.pdf` p2 | 0.050 / 0.545 | **0.017 / 0.182** |
| `faded.pdf` | 1.000 / 1.000 | **0.019 / 0.087** |
| `radio-1922.pdf` | 0.091 / 0.540 | **0.005 / 0.016** |
| **mean** | 0.234 / 0.478 | **0.011 / 0.093** |
| table cell accuracy | 0.933 | **1.000** |
| total wall time | 24.3s | **23.0s** |
| peak service RSS | 6.3GB | 7.6GB on the first call, then 6.3GB |

MinerU is better or equal on every page, twenty times better on mean CER, and
it recovers the one table perfectly where PP-StructureV3 drops a thousands
separator.

**It is also not slower**, which contradicts what the rest of this document
says. The "seconds to a minute per page" figure came from cold calls: the first
vision request in a process pays about 6s of warm-up, and after that MinerU was
faster than PP-StructureV3 on four of five pages. On CPU, on Apple Silicon. The
cost argument for keeping it in reserve is weaker than it looked.

### And yet: no.

Not on this evidence, because "reads better" is not the same question as
"should be the default tier", and the two things that make it a good Tier 3 are
exactly what disqualify it as Tier 2:

- **It reports no confidence.** Promoting MinerU would delete the only signal
  that can catch a bad read — the measured-confidence path the scorer was just
  rebuilt around. Every OCR page would fall back to the additive formula, whose
  floor is 0.55 for any page with text, and nothing could ever escalate again.
- **There would be nothing to escalate *to*.** The tier structure exists to give
  a failed page a second chance from a different kind of engine. Spend the best
  engine first and a failure is final.
- **It invents tables.** `faded.pdf` and `radio-1922.pdf` are columns of plain
  text, and MinerU returns both as `table` blocks. The CER numbers above hide
  this, because the text scorer walks into table cells by design. As Tier 3 on a
  page that was otherwise lost, a spurious grid is a small price. As the default
  for every scanned page, it puts structure into the canonical model that is not
  in the document — which is the thing this pipeline refuses to do elsewhere.

So the finding is real and the tiering stays. What it actually argues for is
raising how often Tier 3 runs, not promoting it: on this corpus it would have
improved four pages out of five and cost nothing in wall time. That is the same
conclusion the escalation-trigger section reaches from the other direction, and
it is one more reason the next piece of work is the trigger rather than the
tier.

Sample size: five pages, four of them synthetic, one machine, CPU only. The
direction is unambiguous and the margin is large, but this is not a corpus.
