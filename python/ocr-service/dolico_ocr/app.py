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
from .canonical import error_output, extract_output, page_payload
from .engine import OCREngine
from .layout import group_lines
from .raster import DEFAULT_DPI, RasterError, is_pdf, render_pdf_pages, wrap_image

logging.basicConfig(
    level=os.environ.get("DOLICO_OCR_LOG_LEVEL", "INFO"),
    format="%(asctime)s %(levelname)s %(name)s %(message)s",
)
log = logging.getLogger("dolico_ocr")

engine = OCREngine(lang=os.environ.get("DOLICO_OCR_LANG", "en"))

MAX_UPLOAD_BYTES = int(os.environ.get("DOLICO_OCR_MAX_UPLOAD_BYTES", 256 << 20))


@asynccontextmanager
async def lifespan(_: FastAPI):
    # Load at startup rather than lazily: a health check that passes before the
    # models exist would let the orchestrator route work to a replica that then
    # spends thirty seconds downloading.
    if os.environ.get("DOLICO_OCR_LAZY_LOAD", "").lower() not in {"1", "true", "yes"}:
        await run_in_threadpool(engine.load)
    yield


app = FastAPI(title="Dolico OCR", version=__version__, lifespan=lifespan)


@app.get("/healthz")
def healthz() -> JSONResponse:
    ready = engine.loaded
    return JSONResponse(
        status_code=200 if ready else 503,
        content={
            "status": "ok" if ready else "loading",
            "engine": ENGINE_NAME,
            "engine_version": engine.version,
            "schema_version": SCHEMA_VERSION,
            "service_version": __version__,
            "models": engine.describe(),
        },
    )


@app.get("/v1/version")
def version() -> dict:
    return {
        "schema_version": SCHEMA_VERSION,
        "service_version": __version__,
        "engine": ENGINE_NAME,
        "engine_version": engine.version,
    }


@app.post("/v1/extract")
async def extract(
    file: UploadFile = File(...),
    pages: str = Form(""),
    dpi: int = Form(DEFAULT_DPI),
) -> JSONResponse:
    """OCR the requested pages of a document.

    `pages` is a comma-separated list of 1-indexed page numbers; empty means
    every page. The response is the canonical extract envelope, identical in
    shape to what the Rust shim produces.
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

    pages_out = []
    for page in rendered:
        lines = await run_in_threadpool(engine.read, page.image)
        paragraphs = group_lines(lines)
        pages_out.append(page_payload(page, paragraphs, engine.version))
        log.info(
            "ocr page=%d lines=%d paragraphs=%d",
            page.number,
            len(lines),
            len(paragraphs),
        )

    duration_ms = int((time.monotonic() - started) * 1000)
    return JSONResponse(extract_output(pages_out, engine.version, duration_ms))


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
