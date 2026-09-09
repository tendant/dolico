# GLM-OCR as a Second Tier-3 Engine — Design

Status: **built, not measured.** The engine is implemented and its tests
pass; nothing in the *comparison* below has been run.

That is the difference between this document and
[`vision-tier-design.md`](vision-tier-design.md), which was written *after*
MinerU had been installed and pointed at the repository's own fixtures. Every
claim here about GLM-OCR is read out of its source; every claim about how well
it reads is still an open question with a proposed way to answer it. The
sections are ordered so that the code comes before the benchmark, but the
decision comes after it — and the code shipping is not the decision.

Two things were learned by building it rather than by reading, and both are
marked where they appear: the bounding boxes need no conversion at all, and
the two engines cannot be installed into the same environment.

## What GLM-OCR is

A 0.9B-parameter vision-language model for document parsing, from Z.ai
(`zai-org/GLM-OCR`). Its pipeline is two stages: **PP-DocLayoutV3** detects
regions on a rendered page, then the VLM reads each region in parallel with a
task-specific prompt (`Text Recognition:`, `Table Recognition:`,
`Formula Recognition:`).

Code is Apache-2.0; **the model weights are MIT**. It runs against Zhipu's
hosted API, or self-hosted behind vLLM, SGLang, Ollama or MLX — in every
self-hosted shape the VLM is reached over an OpenAI-compatible
`/v1/chat/completions`, and only the layout stage runs in-process.

## Where it goes, and where it does not

**A second engine at Tier 3, selected at runtime. Not a fourth tier.**

The vision-tier design already settled the general form of this question when
it rejected MinerU's `pipeline` backend: a tier exists to hand a failed page to
a *different kind* of engine, and "a tier that reruns the same class of model
that just failed is not a fallback." MinerU `hybrid-engine` is a 1.2B VLM
document parser. GLM-OCR is a 0.9B VLM document parser. Stacking one behind the
other is another attempt at the third tier wearing a fourth tier's name, and it
would cost a second multi-second inference on exactly the pages that are
already the most expensive in the corpus.

So the shape is a swap, not an addition to the ladder:

```
Tier 3  ──►  DOLICO_VISION_ENGINE=mineru   (default, unchanged)
             DOLICO_VISION_ENGINE=glm-ocr  (new)
```

Everything the router does around Tier 3 is reused untouched: the 0.35 quality
threshold, the five-page cap, the disagreement probe, and the arbitration rule
that the vision result replaces the OCR result only when the call succeeds.
None of that is engine-specific, and none of it should become so.

The one thing a second engine *does* buy beyond a swap is a third opinion for
the disagreement probe, where the two vision engines could be compared against
each other rather than against OCR. That is deliberately out of scope here. The
probe's cost is already a fixed toll on every document with scanned pages, and
doubling it needs its own justification and its own measurement.

## What has to change structurally

One thing, and it is not optional.

**`VisionName` stops being a compile-time constant.**
`internal/engine/paddleocr/vision.go:13` hardcodes `"mineru"`, and `Name()`
feeds both `Provenance.Engine` and the page cache key. Ship a second engine
behind that constant and GLM-OCR's pages get written into the cache under
MinerU's name, so switching engines would silently serve the other one's cached
pages.

The fix is the one the same file already applies to the version, for the same
reason and in the same place. `NewVision` cannot know the name at startup —
Tier 3 loads lazily — so the name is adopted from the first real answer
alongside the version it already adopts (`vision.go:92`). The service already
reports it: `/v1/version` returns `vision_engine` (`app.py:151`).

Everything else is additive.

## Integration

**In-process, via `glmocr.api.GlmOcr.parse()`, one call per page, fed our own
rendered bytes.**

`parse()` accepts raw `bytes` and auto-detects the format from magic bytes
(`api.py:222`), and returns a `PipelineResult` **in memory** with
`.json_result` as a list of pages, each a list of region dicts. There is no
output directory.

That deletes most of the MinerU adapter. Compare what the two engines force:

| | MinerU | GLM-OCR |
| --- | --- | --- |
| Input | PDF bytes; MinerU opens the PDF itself | any image bytes; we hand it one rendered page |
| Output | files under a temp dir | a Python object |
| Finding the result | glob for `*_content_list.json` under a backend-dependent subdirectory | `.json_result` |
| Page size | second glob, for `*_middle.json` | ours already — `RasteredPage.width_pt/height_pt` |
| Page selection | contiguous `start_page_id`/`end_page_id` range | whatever page we chose to render |

**Rasterizing it ourselves is the load-bearing choice**, and it is worth being
explicit about why, because handing `parse()` the PDF bytes would appear to
work:

- GLM-OCR's own PDF path needs **poppler** (`pdftoppm`), a system package the
  OCR image does not carry and should not start carrying for one engine.
- It would render at its own `pdf_dpi: 200` rather than the DPI the rest of
  this service is configured with, so Tier 3 would disagree with Tiers 1 and 2
  about what the page looks like for no stated reason.
- `render_pdf_pages()` already gives us the page size in true points. MinerU
  needed a second output file for that. Taking GLM-OCR's PDF path would put us
  back to inferring it.

So the adapter is: `render_pdf_pages(data, [n], dpi)` → encode the page's
`np.ndarray` to PNG bytes → `parse(bytes)` → map regions to `VisionBlock`.
`raster.py` already owns the first step for Tiers 1 and 2, and the encode is
the only genuinely new line — `glmocr` brings Pillow, so nothing new is
installed for it.

### Coordinates need no conversion at all

**`bbox_2d` is already normalized to 0–1000 with a top-left origin** — the same
space MinerU's `content_list.json` uses. The layout detector normalizes at
`layout_detector.py:390-393`, and the MaaS path normalizes to match at
`api.py:363`.

This is the single luckiest fact in the integration, and it corrects an earlier
reading of this code that had the adapter normalizing pixels. `VisionBlock`
keeps one
meaning, and `_vision_bbox` (`canonical.py:430`) — the 0–1000-to-points
conversion *and* the vertical flip that was verified against Tier 2's
independent measurement of the table fixture — is reused with no change and no
second verification of the same arithmetic. The space is now pinned as the
tier's contract in `vision_base`, so a third engine that reports pixels
converts in its own adapter rather than here.

The boxes are also **measured, not generated**: they come from the layout
detector, not from the language model. The rule that this pipeline never
fabricates geometry holds, and escalating a page to a GLM-OCR Tier 3 keeps its
geometry exactly as escalating to MinerU does.

### The real friction: the JSON is Markdown in disguise

`json_result`'s `content` field is not plain text. `_format_content()`
(`result_formatter.py:287`) decorates it for rendering:

| region | what `content` actually holds |
| --- | --- |
| `native_label: doc_title` | `"# The Title"` |
| `native_label: paragraph_title` | `"## A Heading"` |
| `label: formula` | `"$$\n...\n$$"` |
| bullets | `"- item"`, rewritten from `·` / `•` / `*` |
| `label: text` | single newlines doubled into paragraph breaks |

The canonical model carries a heading *level* and plain text; Markdown is a
view generated from it, which is the whole thesis of this repository stated
backwards. So the adapter has to undo the rendering.

It should undo it from the **`native_label`, not from the Markdown**: take the
level from `doc_title` → 1 and `paragraph_title` → 2 and strip `^#+\s*`, rather
than counting `#` characters. Parsing the syntax back out would make the
canonical structure depend on a formatter setting, and two of the three
decorations here are unconditional — only `enable_format_bullet_points` can be
switched off in config.

`raw_json_result` holds the model's output before post-processing and is
tempting for this reason, but it also predates the merges that are worth
keeping — hyphenated text blocks rejoined, formula numbers folded into their
formulas. Take the formatted output and strip it.

### Output mapping

`_map_label` (`result_formatter.py:365`) collapses the detector's ~24 native
labels into `text` / `table` / `formula` / `image`, passing anything unmapped
through under its own name. So the adapter sees both, and both matter: `label`
decides the canonical type, `native_label` decides the detail.

**Except on the cloud path, which sets no `native_label` at all.**
`_maas_response_to_pipeline_result` (`api.py:414`) builds each region as
`{index, label, content, bbox_2d}` and nothing else, and does not say which
vocabulary its `label` is drawn from — the mapped four or the detector's own.
So the adapter derives both rather than trusting either: a `label` that is one
of the four is taken as mapped, anything else is read as native and mapped
here. Getting this wrong is not subtle in effect — an HTML table would arrive
as a paragraph of angle brackets, and a formula would keep its `$$` fences —
but it is entirely silent, because every one of those is a valid canonical
paragraph.

| GLM-OCR `label` | `native_label` | Canonical |
| --- | --- | --- |
| `text` | `doc_title` | `heading`, level 1 |
| `text` | `paragraph_title` | `heading`, level 2 |
| `text` | `algorithm` | `code` |
| `text` | anything else | `paragraph` |
| `table` | | `table` — **reuses `tables.parse_table_html`**, as MinerU does |
| `formula` | | `formula`, `$$` fences stripped |
| `image` | | `image` |
| `header` `footer` `footnote` `number` `aside_text` `reference` | | `paragraph` (label kept in provenance) |

The last row matches what the MinerU adapter already does with `header` and
`footer`, and the `MINERU_LABEL_TO_TYPE` table at `canonical.py:286` becomes
one of two, keyed by engine.

Two behaviors carry over unchanged and deliberately:

- **The one-column-table flattening** (`canonical.py:372`). That rule exists
  because MinerU returned the faded receipt as an 8×1 grid and the 1922
  newspaper column as a 9×1 one — plain text in a narrow column, read as
  structure the document does not have. Nothing about that failure was specific
  to MinerU, and the rule costs nothing when it does not fire.
- **No invented confidence.** GLM-OCR reports none per region either. The page
  gets `1.0` if it produced blocks and `0.0` if it did not, exactly as
  `vision_page_payload` already does, and the quality scorer remains the thing
  that second-guesses it.

## Deployment

Three shapes, and they are not equally acceptable here.

**Self-hosted vLLM or SGLang — the production shape.** The VLM runs as its own
service and the OCR service talks to it over `/v1/chat/completions`. This is
the same split as `DOLICO_MINERU_URL`, so that variable generalizes to
`DOLICO_VISION_URL` and keeps a third model's resident memory out of a service
already measured at 6.3GB steady. It is a third container, not something to
bake into `dolico-ocr`. The README names minimum vLLM and SGLang versions;
pin whatever it says at integration time.

**MLX or Ollama — the development shape.** On Apple Silicon, where all of this
repository's measurements were taken, MLX is what makes a local GLM-OCR
testable at all. `api_mode: ollama_generate` exists for Ollama's native
endpoint because its OpenAI-compatible path 502s on some vision requests.

**Zhipu MaaS — supported, and never reached by accident.** *This is the
section the implementation changed most, because the deployment that wanted
GLM-OCR wanted the cloud API.*

`glmocr/config.yaml` ships `maas.enabled: true` pointing at
`open.bigmodel.cn`, and a `ZHIPU_API_KEY` anywhere in the environment flips it
there regardless of the YAML. The original objection was to that *default*, not
to the cloud as such: an engine that posts documents to a third party because
nobody configured it otherwise is a different thing from one that does so
because an operator decided to. So the shape is opt-in by a variable this
repository owns:

    DOLICO_GLM_API_KEY   the only thing that turns the cloud on
    ZHIPU_API_KEY        does not, though the library reads it
    (neither, no URL)    the tier reports itself unavailable

A self-hosted endpoint wins over a leftover key, so standing up your own server
redirects the pages rather than racing a stale credential. Pages read in the
cloud carry `glm-ocr/maas:<label>` in provenance, not the model name: "where
was this page read" should not require reading deployment config.

The cost of this mode is worth stating plainly rather than burying. It is the
only configuration in this pipeline where a document leaves the host, and in at
least one deployment those documents are patient records held in a service with
no authentication of its own, on a 30-day TTL chosen partly to bound exposure.
That is a decision for whoever runs it — but it should be a decision, and it
should be visible in `/healthz`, in provenance, and in `deploy/README.md`
alongside everything else that egresses.

Note that even in the fully remote shape, **PP-DocLayoutV3 still runs
in-process**. GLM-OCR is not a pure client; `layout_device: cpu` is supported
and is probably right here, since the layout pass is the cheap half.

### Concurrency

`VisionEngine._lock` (`vision.py:113`) serializes inference because that is
true of Paddle and of a local MinerU: one model, one inference, and concurrency
comes from processes. **It is probably wrong for a remote GLM-OCR.** Local work
is layout detection; region recognition is an HTTP fan-out that glmocr itself
parallelizes at `max_workers: 32`. Holding a process-wide lock across that
serializes network wait.

Probably wrong is not measured wrong. Keep the lock for the first version —
matching the other tiers is worth more than a guess — and treat removing it as
its own change with its own number attached.

## Configuration

| Variable | Default | |
| --- | --- | --- |
| `DOLICO_VISION_ENGINE` | `mineru` | `mineru` or `glm-ocr`. The default keeps every existing deployment on the engine it was measured with. |
| `DOLICO_VISION_URL` | unset | the remote VLM endpoint; generalizes `DOLICO_MINERU_URL`, which stays as a deprecated alias |
| `DOLICO_GLM_API_KEY` | unset | with no `DOLICO_VISION_URL` set, pages go to Zhipu's cloud API. The only thing that enables it |
| `DOLICO_GLM_MODEL` | `glm-ocr` | the served model name, which differs per backend (`mlx-community/GLM-OCR-bf16` for MLX, `glm-ocr:latest` for Ollama) |
| `DOLICO_GLM_LAYOUT_DEVICE` | `cpu` | where PP-DocLayoutV3 runs |
| `DOLICO_GLM_API_MODE` | `openai` | `ollama_generate` for Ollama, whose OpenAI-compatible path 502s on vision requests |
| — | — | the endpoint path comes from `DOLICO_VISION_URL` when it carries one, else from the mode. MLX serves the OpenAI API without the `/v1` prefix, so it needs the explicit form. |
| `DOLICO_GLM_DPI` | the service's own | render DPI for pages handed to GLM-OCR |

`DOLICO_VISION_ENABLED`, `DOLICO_VISION_THRESHOLD`, `DOLICO_VISION_MAX_PAGES`,
`DOLICO_VISION_PROBE` and `DOLICO_VISION_DISAGREEMENT` are unchanged and
engine-independent. That is the point of putting GLM-OCR at Tier 3 rather than
beside it.

Packaging: a `glm` extra, plus `make ocr-glm` and `make bench-glm` mirroring
the existing vision targets. The extra is `glmocr[layout]` rather than
`glmocr[selfhosted]`: the two differ only by `pypdfium2`, which this service
already depends on, and `layout` names the half that actually runs in this
process. It is not as light as the model size suggests — the layout stage is
PP-DocLayoutV3 under torch, so the extra costs roughly what MinerU's does.
GLM-OCR being the smaller model is an argument about the box serving the VLM,
not about this one.

### The two engines cannot share an environment

Found by the resolver while implementing this, and it is the one thing here
that changes a decision rather than describing one.

MinerU pins `transformers>=4.57.3,<5.0.0`. `glmocr[layout]` requires
`transformers>=5.3.0`. No version satisfies both, so resolving those two
extras together fails outright.

*Narrowed after the fact:* this is only true of a **self-hosted** GLM-OCR. The
cloud path needs none of it — Zhipu runs the layout model too, so base
`glmocr` brings no torch and no transformers and conflicts with nothing.
Verified rather than reasoned: a venv with `glmocr` alone has neither, and
`GlmOcr(mode="maas")` constructs and reports MaaS mode. Hence two extras,
`glm` and `glm-selfhosted`, and only the second is in the conflict pair. The
first also keeps the OCR image roughly the size it is with no Tier 3 at all,
which matters rather more than the tidiness: the deployment that wanted this
runs one OCR worker on a host where the MinerU extra would have tripled the
image for a model it never loads.

Upgrading MinerU does not fix it. Every release carries that pin, including the
4.0 pre-release, so this is a standing disagreement between the two projects
rather than a stale requirement waiting on a bump.

That is not a problem to work around. It is the deployment shape, stated by the
dependency graph: **a service is built with one Tier 3 engine, and
`DOLICO_VISION_ENGINE` selects which one it was built with — not which one it
switches to at runtime.** Nothing above assumed otherwise; `ocr-vision` and
`ocr-glm` were always separate targets, and the router never cared which engine
answered.

It is declared rather than left to be discovered:

```toml
[tool.uv]
conflicts = [[{ extra = "vision" }, { extra = "glm" }]]
```

which lets one lockfile carry both, resolved separately. Without it the
lockfile itself is unsatisfiable, and the failure surfaces as an install error
in whichever deployment tries it next rather than here.

### Where the separation is enforced

Three places, each catching what the one below it cannot:

| | |
| --- | --- |
| `pyproject.toml` | `[tool.uv] conflicts` — the extras resolve separately, and one lockfile serves both |
| `deploy/Dockerfile.ocr` | `OCR_EXTRAS` derives `DOLICO_VISION_ENGINE` and refuses a build naming both, so an image cannot disagree with itself about which engine it holds |
| `Makefile` | the engine goes in the image tag (`-mineru`, `-glm`), so two builds of one commit cannot silently replace each other |

Splitting `dolico-ocr` into separate *distributions* would add release
machinery for isolation the extras already provide — the venvs are already
disjoint, and nothing outside this repository installs the package.

What would actually dissolve the conflict is making Tier 3 a pure HTTP client,
so the OCR image carries neither engine's weights or dependencies. Neither
engine supports that today: MinerU's `hybrid-*` backends check for a local
torch and refuse without one (`ensure_backend_dependencies`), because the
hybrid backend does its native text extraction here and sends only the VLM half
away; GLM-OCR runs PP-DocLayoutV3 locally and there is no layout-free mode that
keeps bounding boxes. Worth revisiting if either grows a thin client — it is
the shape that makes the question moot rather than managed.

It also closes an option this document had left open. A single service cannot
hold both engines, so using GLM-OCR as a *third* opinion for the disagreement
probe — parked as out of scope above — would need two OCR services rather than
one process. That is a larger change than it looked, and the parking is firmer
than it was.

## Failure handling

Unchanged, because the rule is about the tier and not the engine: **Tier 3 must
never make a page worse.** The table in `vision-tier-design.md` applies as
written — engine not installed means the tier reports unavailable and the
router never escalates; a parse error keeps the OCR result and records
`vision_failed`; an empty result keeps the OCR result and records
`vision_empty`.

Two failure modes are new in kind, both from the remote shape, and both fold
into the existing `vision_failed` path rather than needing their own:

- **The VLM endpoint is down or overloaded.** glmocr retries 429 and 5xx
  itself; past that it raises, the adapter turns it into `VisionError`, and the
  OCR result stands. Worth logging distinctly from a parse failure, because one
  is an operational problem and the other is a document.
- **The layout stage loads but the endpoint was never configured.** This should
  fail at `load()`, loudly, the way a missing PP-StructureV3 does — not on the
  first escalated page of the first customer document.

## Verification

`available()` for MinerU imports `do_parse` because a partial install of a
torch-based package fails at import rather than at first use. The equivalent
honest check for GLM-OCR is **importing `glmocr.api` and confirming the
endpoint is reachable**, because a working import proves much less here: a
MaaS-only install has no torch, no layout model, and no way to know it cannot
serve until it tries.

Everything else is testable the way the existing tier is:

- `tests/test_vision.py` becomes engine-parameterized, so the mapping, the
  coordinate conversion, the Markdown-stripping, the one-column flattening and
  every failure path are exercised against both adapters.
- The live test skips unless an endpoint is up, matching the existing one.
- `internal/engine/paddleocr` gains a test that a page produced by a
  differently-named vision engine does not collide in the cache with the same
  page produced by the other one. That is the regression the `VisionName`
  change exists to prevent, and it is invisible without a test.

## What would decide this

The code above is written and its tests pass. **That is not what makes GLM-OCR
shippable, and it should not be confused with a recommendation.**

This repository settles engine questions with numbers. The
"should MinerU be Tier 2?" question got a five-page CER/WER table, a wall-time
column and a peak-RSS column, and still came out *no* — because "reads better"
turned out not to answer the question that was asked. The equivalent gate here
is the same table, run against `make bench-vision` on both engines:

| | `mineru` | `glm-ocr` |
| --- | --- | --- |
| `scanned.pdf` CER / WER | 0.014 / 0.182 | ? |
| `scanned-table.pdf` | 0.000 / 0.000 | ? |
| `mixed.pdf` p2 | 0.017 / 0.182 | ? |
| `faded.pdf` | 0.019 / 0.087 | ? |
| `corpus-hard/radio-1922.pdf` | 0.005 / 0.016 | ? |
| table cell accuracy | 1.000 | ? |
| wall time, warm | | ? |
| peak service RSS | 7.6GB first call, then 6.3GB | ? |

The MinerU column is measured and is in `vision-tier-design.md`. The other
column is the deliverable.

Note what that table means for the bar: MinerU's mean CER on this corpus is
0.011, and on the one genuinely hard page — real microfilm, hand-transcribed —
it misses a single letter. GLM-OCR does not have much room to win, and "as good
as" would not by itself justify a second engine to maintain. The honest
outcomes are *better on the hard pages*, *materially cheaper for the same
quality*, or *no*.

## First measurements, from production

Not the benchmark — that column is still empty — but the tier ran against real
documents on the estate, and one result is decisive enough to record here.

**It works on ordinary scanned pages.** Three pages of the deploy sweep came
back from the cloud API with blocks (`blocks=2`, `blocks=3`, `blocks=2`), the
mapping held, and the e2e sweep passed.

**It reads nothing at all from `faded.pdf`.** Zero regions. Not zero *text* —
zero regions, confirmed by calling the API directly with the rendered page and
dumping its raw response, after checking the image we send is a real 2200×1700
raster with 11M non-white pixels. The page comes back `vision_empty`, the OCR
result stands, and that result is the single character `b`.

That fixture is the reason the vision tier exists. MinerU recovers it at CER
0.019 from the same pixels.

**This is recorded concern #1, confirmed.** That concern said GLM-OCR's layout
stage is PP-DocLayoutV3, the same family as Tier 2's PP-StructureV3, so a page
whose regions Tier 2 fails to carve up will fail again — and only the *reading*
of each region improves. It named the test: "compare detected regions, not just
text." The regions are zero. There is nothing for the 0.9B model to read,
however good it is at reading, because the stage in front of it found no
document on the page.

So the two engines are not interchangeable in the way "both are VLM document
parsers" suggests. MinerU's hybrid backend does not depend on a separate
detector agreeing there is something there; GLM-OCR's pipeline does. On clean
scans that difference is invisible. On the degraded page — which is the only
kind of page Tier 3 is called for — it is the whole difference.

**What that means for the deployment as configured.** With the disagreement
probe on, every document with a scanned page pays at least one cloud call, and
each of those uploads a page off the host. The pages where the tier demonstrably
helps are the ones OCR already read acceptably; the page where OCR failed, it
does not rescue. That is a cost with no measured benefit yet, and the honest
options are to measure it properly (`make bench-glm`), to turn the probe off
and let only the threshold escalate, or to run MinerU for this tier and
GLM-OCR not at all.

## Recorded concerns

**1. Its layout stage is Tier 2's model family.** GLM-OCR detects regions with
PP-DocLayoutV3 — PaddleX, the same lineage as PP-StructureV3. The *recognition*
is genuinely different, and that is where MinerU's win on `faded.pdf` and
`radio-1922.pdf` came from, so this is not the `pipeline`-backend objection all
over again. But it is a real asymmetry: a page whose regions Tier 2 carves up
wrongly will be carved up wrongly again, and only the reading of each region
improves. MinerU's hybrid backend does not share that weakness. If GLM-OCR
loses on any fixture, this is the first hypothesis to test — compare detected
regions, not just text.

**2. The hosted default.** Covered under Deployment, repeated here because it
is the kind of thing that gets lost in a config refactor: this dependency's
out-of-the-box behavior is to send documents to a third party, and the guard
against that lives in our adapter, not in theirs.

**3. It cannot be Tier 2 either, for MinerU's reason.** No per-region
confidence. Promoting either VLM deletes the measured-confidence signal the
quality scorer was rebuilt around, whose additive fallback floors any page with
text at 0.55 — above every threshold that could escalate it. The tiering
argument in `vision-tier-design.md` is unchanged by this document.

**4. The escalation trigger is still the weaker half.** Both the vision-tier
design and the disagreement-probe work reached the same conclusion from
opposite directions: Tier 3 is better than the rule that decides when to call
it, and PaddleOCR reported 0.938 confidence on the page it got 54% of the words
wrong on. Adding a second Tier-3 engine improves nothing about that. It is a
sideways move, and it should be argued for on cost, licensing or hard-page
accuracy — not as progress on the thing that is actually limiting this
pipeline.

**5. Two engines is a maintenance surface.** Every adapter is a second place
for the label mapping, the flattening rule and the coordinate conversion to
drift. The parameterized tests are what hold that line, and they are not
optional garnish on this change.

**6. The licensing is genuinely better, and that is a real argument.**
Apache-2.0 code with **MIT weights**, against MinerU's Apache-2.0 plus a
commercial-use threshold, plus an attribution obligation for online services,
plus automatic termination for breaching either — an obligation this repository
has already been caught not meeting once, when the vision tier was absent from
`/v1/engines`. A permissive engine of comparable quality removes a standing
compliance requirement rather than adding one. It does not, on its own, justify
replacing a measured engine with an unmeasured one.
