"""HTTP surface of the OCR tier.

One endpoint that matters: POST the document bytes and the page numbers, get
canonical pages back. The service is stateless and holds nothing between
requests except the loaded model.
"""

from __future__ import annotations

import logging
import os
import time
from contextlib import asynccontextmanager

from fastapi import FastAPI, File, Form, UploadFile
from fastapi.concurrency import run_in_threadpool
from fastapi.responses import JSONResponse

from . import ENGINE_NAME, SCHEMA_VERSION, __version__
from . import structure as structure_mod
from . import vision as vision_mod
from .canonical import (
    error_output,
    extract_output,
    layout_page_payload,
    page_payload,
    vision_page_payload,
)
from .engine import OCREngine
from .layout import group_lines
from .raster import DEFAULT_DPI, RasterError, is_pdf, render_pdf_pages, wrap_image
from .structure import StructureEngine
from .vision import VisionEngine, VisionError

logging.basicConfig(
    level=os.environ.get("DOLICO_OCR_LOG_LEVEL", "INFO"),
    format="%(asctime)s %(levelname)s %(name)s %(message)s",
)
log = logging.getLogger("dolico_ocr")

LANG = os.environ.get("DOLICO_OCR_LANG", "en")

# Tier 1: text lines. Always available.
engine = OCREngine(lang=LANG)
# Tier 2: layout analysis. Preferred when its dependencies are installed,
# because a scanned table read as flat text is wrong rather than merely
# uglier, and it costs only about a third more per page.
structure = StructureEngine(lang=LANG)
# Tier 3: MinerU. Never the default for a document -- it is reached per page,
# only for pages the other tiers already lost, because it costs seconds and
# gigabytes rather than milliseconds.
vision = VisionEngine()

# "layout" | "text" | "auto". Auto uses Tier 2 when it is installable.
TIER = os.environ.get("DOLICO_OCR_TIER", "auto").strip().lower()

MAX_UPLOAD_BYTES = int(os.environ.get("DOLICO_OCR_MAX_UPLOAD_BYTES", 256 << 20))

# How many uvicorn worker processes are serving, reported so the Go client can
# match its request concurrency without being told separately.
#
# Processes, not threads: one inference uses about one core and scales with
# neither Paddle's intra-op threading nor Python threads -- Paddle holds the
# GIL throughout, so a thread pool measures at exactly 1.00x. Concurrency
# therefore costs a whole process, and a process costs 2.5-3GB once its arenas
# are warm, which is why this defaults to 1 and is opted into deliberately.
WORKERS = max(1, int(os.environ.get("DOLICO_OCR_WORKERS", "1")))


def _use_structure() -> bool:
    if TIER == "text":
        return False
    if TIER == "layout":
        return True
    return structure_mod.available()


def active_engine():
    """The tier actually serving requests, and the name it reports."""
    if _use_structure():
        return structure, structure_mod.ENGINE_NAME
    return engine, ENGINE_NAME


@asynccontextmanager
async def lifespan(_: FastAPI):
    # Load at startup rather than lazily: a health check that passes before the
    # models exist would let the orchestrator route work to a replica that then
    # spends thirty seconds downloading.
    if os.environ.get("DOLICO_OCR_LAZY_LOAD", "").lower() not in {"1", "true", "yes"}:
        active, name = active_engine()
        try:
            await run_in_threadpool(active.load)
            log.info("serving tier %s", name)
        except Exception as exc:
            if active is engine:
                raise
            # PP-StructureV3 needs the `paddlex[ocr]` extras. Falling back is
            # better than refusing to start -- Tier 1 still reads the page --
            # but it has to be loud, because the difference shows up as tables
            # silently arriving as flat text.
            log.error("PP-StructureV3 unavailable, falling back to text-line OCR: %s", exc)
            log.error("install it with: uv sync --extra structure")
            globals()["TIER"] = "text"
            await run_in_threadpool(engine.load)
    yield


app = FastAPI(title="Dolico OCR", version=__version__, lifespan=lifespan)


@app.get("/healthz")
def healthz() -> JSONResponse:
    active, name = active_engine()
    ready = active.loaded
    return JSONResponse(
        status_code=200 if ready else 503,
        content={
            "status": "ok" if ready else "loading",
            "engine": name,
            "engine_version": active.version,
            "tier": "layout" if active is structure else "text",
            "workers": WORKERS,
            "structure_available": structure_mod.available(),
            # Advertised separately from the serving tier: vision is reached
            # per page by the router, never as this service's default tier.
            "vision_available": vision_mod.available(),
            "vision": vision.describe(),
            "schema_version": SCHEMA_VERSION,
            "service_version": __version__,
            "models": active.describe(),
        },
    )


@app.get("/v1/version")
def version() -> dict:
    active, name = active_engine()
    return {
        "schema_version": SCHEMA_VERSION,
        "service_version": __version__,
        # The engine name follows the tier, so the orchestrator records the
        # right thing in provenance and keys its cache on it: upgrading from
        # text-line OCR to layout analysis must invalidate the pages the old
        # tier produced.
        "engine": name,
        "engine_version": active.version,
        "tier": "layout" if active is structure else "text",
        "workers": WORKERS,
        "vision_available": vision_mod.available(),
        "vision_engine": vision_mod.ENGINE_NAME,
    }


@app.post("/v1/extract")
async def extract(
    file: UploadFile = File(...),
    pages: str = Form(""),
    dpi: int = Form(DEFAULT_DPI),
    tier: str = Form(""),
) -> JSONResponse:
    """OCR the requested pages of a document.

    `pages` is a comma-separated list of 1-indexed page numbers; empty means
    every page. The response is the canonical extract envelope, identical in
    shape to what the Rust shim produces.

    `tier` selects an engine explicitly. Empty uses whichever tier this service
    is serving; `vision` reaches Tier 3 for the named pages. Vision is
    per-request rather than a service mode because it only makes sense for
    pages the other tiers already lost.
    """
    started = time.monotonic()

    data = await file.read()
    if not data:
        return _error(400, "malformed", "the uploaded document is empty")
    if len(data) > MAX_UPLOAD_BYTES:
        return _error(413, "resource_limit", f"document exceeds {MAX_UPLOAD_BYTES} bytes")

    try:
        wanted = _parse_pages(pages)
    except ValueError as exc:
        return _error(400, "malformed", str(exc))

    dpi = max(72, min(600, dpi))

    if tier.strip().lower() == "vision":
        return await _extract_vision(data, wanted, started)

    try:
        rendered = await run_in_threadpool(_render, data, wanted, dpi)
    except RasterError as exc:
        # Distinguish "I cannot handle this kind of file" from "this file is
        # broken": the router retries neither, but the API reports them
        # differently and so should we.
        kind = "unsupported" if "not a PDF" in str(exc) else "malformed"
        return _error(415 if kind == "unsupported" else 422, kind, str(exc))

    if not rendered:
        return _error(422, "malformed", "no requested page exists in this document")

    active, name = active_engine()
    pages_out = []
    for page in rendered:
        if active is structure:
            blocks, lines = await run_in_threadpool(active.read, page.image)
            pages_out.append(
                layout_page_payload(page, blocks, lines, active.version, name)
            )
            log.info(
                "layout page=%d regions=%d labels=%s",
                page.number,
                len(blocks),
                [b.label for b in blocks],
            )
        else:
            lines = await run_in_threadpool(active.read, page.image)
            paragraphs = group_lines(lines)
            pages_out.append(page_payload(page, paragraphs, active.version))
            log.info(
                "ocr page=%d lines=%d paragraphs=%d",
                page.number,
                len(lines),
                len(paragraphs),
            )

    duration_ms = int((time.monotonic() - started) * 1000)
    return JSONResponse(extract_output(pages_out, active.version, duration_ms, engine=name))


async def _extract_vision(data: bytes, wanted: list[int] | None, started: float) -> JSONResponse:
    """Tier 3: read named pages with MinerU.

    Named pages only. Vision runs when the cheaper tiers have already failed a
    specific page, so a whole-document vision request is a caller mistake
    rather than something to quietly and expensively honor.
    """
    if not vision_mod.available():
        return _error(
            503,
            "unavailable",
            "the vision tier is not installed; run `uv sync --extra vision`",
        )
    if not is_pdf(data):
        return _error(415, "unsupported", "the vision tier reads PDFs only")
    if not wanted:
        return _error(
            400,
            "malformed",
            "the vision tier extracts named pages only; pass `pages`",
        )

    pages_out = []
    for number in wanted:
        try:
            blocks, width, height = await run_in_threadpool(vision.read, data, number)
        except VisionError as exc:
            # One bad page must not fail the batch: the router keeps the OCR
            # result for whatever is missing, which is strictly better than
            # discarding the pages that did work.
            log.error("vision page=%d failed: %s", number, exc)
            continue

        pages_out.append(
            vision_page_payload(number, blocks, width, height, vision.version, vision.backend)
        )
        log.info("vision page=%d blocks=%d", number, len(blocks))

    if not pages_out:
        return _error(422, "malformed", "the vision tier could not read any requested page")

    duration_ms = int((time.monotonic() - started) * 1000)
    return JSONResponse(
        extract_output(pages_out, vision.version, duration_ms, engine=vision_mod.ENGINE_NAME)
    )


def _render(data: bytes, pages: list[int] | None, dpi: int):
    if is_pdf(data):
        return render_pdf_pages(data, pages, dpi)
    # A standalone image is a single page; asking for anything but page 1 is a
    # caller mistake, and returning nothing says so.
    if pages is not None and pages != [1]:
        return []
    return wrap_image(data)


def _parse_pages(raw: str) -> list[int] | None:
    raw = (raw or "").strip()
    if not raw:
        return None
    out: list[int] = []
    for part in raw.split(","):
        part = part.strip()
        if not part:
            continue
        try:
            number = int(part)
        except ValueError as exc:
            raise ValueError(f"page {part!r} is not a number") from exc
        if number < 1:
            raise ValueError(f"page numbers are 1-indexed, got {number}")
        out.append(number)
    return out or None


def _error(status: int, kind: str, message: str) -> JSONResponse:
    log.warning("request failed: %s: %s", kind, message)
    return JSONResponse(status_code=status, content=error_output(kind, message))
