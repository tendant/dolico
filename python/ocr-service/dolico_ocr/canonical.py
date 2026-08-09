"""Build the canonical envelope this service returns.

The shape is the same `ExtractOutput` the Rust shim emits, and the Go client
parses both with the same code. Keeping one envelope across two very different
engines is what makes the OCR tier a drop-in rather than a special case.

The contract is `schema/canonical-v1.json` at the repository root; this module
is its third mirror, alongside `internal/canonical/document.go` and
`rust/dolico-rs/src/canonical.rs`.
"""

from __future__ import annotations

from . import ENGINE_NAME, SCHEMA_VERSION
from .layout import Paragraph
from .raster import RasteredPage


def block(
    block_id: str,
    text: str,
    confidence: float,
    bbox: dict | None,
    engine_version: str,
) -> dict:
    out = {
        "id": block_id,
        "type": "paragraph",
        "text": text,
        "confidence": round(confidence, 6),
        "provenance": {
            "engine": ENGINE_NAME,
            "engine_version": engine_version,
            # Names the tier as well as the engine: Tier 1 detects text lines
            # and groups them, and produces no headings or tables. A consumer
            # seeing this knows not to expect document structure.
            "method": "paddleocr/text-lines",
        },
    }
    if bbox is not None:
        out["bbox"] = bbox
    return out


def bbox_from_paragraph(paragraph: Paragraph, page: RasteredPage) -> dict | None:
    """Convert a paragraph's raster extent into a PDF-space rectangle.

    A degenerate rectangle is reported as no rectangle, matching the rule the
    rest of the pipeline follows: a zero-area box crops to nothing and a
    consumer cannot tell it from a real one.
    """
    x0_px, y0_px, x1_px, y1_px = paragraph.bbox()
    # The raster y axis points down and PDF's points up, so the *bottom* of the
    # box in PDF space comes from the *largest* raster y.
    x_left, y_bottom = page.to_points(x0_px, y1_px)
    x_right, y_top = page.to_points(x1_px, y0_px)

    width = x_right - x_left
    height = y_top - y_bottom
    if width <= 0 or height <= 0:
        return None
    return {
        "x": round(x_left, 3),
        "y": round(y_bottom, 3),
        "width": round(width, 3),
        "height": round(height, 3),
    }


def page_payload(
    page: RasteredPage, paragraphs: list[Paragraph], engine_version: str
) -> dict:
    blocks = []
    for index, paragraph in enumerate(paragraphs):
        text = paragraph.text
        if not text:
            continue
        blocks.append(
            block(
                block_id=f"p{page.number}-ocr{index}",
                text=text,
                confidence=paragraph.confidence,
                bbox=bbox_from_paragraph(paragraph, page),
                engine_version=engine_version,
            )
        )

    # The page's confidence is what the OCR engine actually reported, averaged
    # over what it read. A page where nothing was found scores zero rather than
    # inheriting a default -- the router needs to be able to see that.
    confidence = (
        sum(b["confidence"] for b in blocks) / len(blocks) if blocks else 0.0
    )
    reasons = ["ocr"] if blocks else ["ocr", "no_text_found"]

    return {
        "number": page.number,
        "kind": "paginated",
        # Unlike the pdf-inspector path, rendering means the real page size is
        # known, so these are genuine rather than omitted.
        "width": round(page.width_pt, 3),
        "height": round(page.height_pt, 3),
        "classification": {
            "type": "scanned",
            "confidence": round(min(1.0, max(0.0, confidence)), 6),
            "reasons": reasons,
        },
        "blocks": blocks,
    }


def extract_output(
    pages: list[dict], engine_version: str, duration_ms: int
) -> dict:
    return {
        "schema_version": SCHEMA_VERSION,
        "engine": ENGINE_NAME,
        "engine_version": engine_version,
        "metadata": {"page_count": len(pages)},
        "pages": pages,
        "duration_ms": duration_ms,
    }


def error_output(kind: str, message: str) -> dict:
    """The same failure envelope the Rust shim writes, so the Go client has one
    way to classify a failure regardless of which engine produced it."""
    return {"schema_version": SCHEMA_VERSION, "kind": kind, "message": message}
