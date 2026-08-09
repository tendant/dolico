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
from .layout import Line, Paragraph
from .raster import RasteredPage
from .structure import LayoutBlock
from .tables import parse_table_html, table_text
from .vision import ENGINE_NAME as VISION_ENGINE
from .vision import VisionBlock


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
    pages: list[dict], engine_version: str, duration_ms: int, engine: str = ENGINE_NAME
) -> dict:
    return {
        "schema_version": SCHEMA_VERSION,
        "engine": engine,
        "engine_version": engine_version,
        "metadata": {"page_count": len(pages)},
        "pages": pages,
        "duration_ms": duration_ms,
    }


# ---------------------------------------------------------------------------
# Tier 2: layout blocks
# ---------------------------------------------------------------------------

# PP-StructureV3's layout labels, mapped onto canonical block types.
#
# Anything unlisted becomes a paragraph, which is the safe default: the text is
# preserved either way, and the original label always survives in
# provenance.method, so nothing is lost by not having a mapping for it.
LABEL_TO_TYPE = {
    "doc_title": "heading",
    "title": "heading",
    "paragraph_title": "heading",
    "chart_title": "heading",
    "figure_title": "heading",
    "table_title": "heading",
    "table": "table",
    "figure": "image",
    "image": "image",
    "chart": "image",
    "seal": "image",
    "formula": "formula",
    "algorithm": "code",
}

# Headings get a depth from how prominent the label is. Only the labels that
# genuinely mean "document title" get level 1.
LABEL_TO_LEVEL = {"doc_title": 1, "title": 1}


def layout_page_payload(
    page: RasteredPage,
    blocks: list[LayoutBlock],
    lines: list[Line],
    engine_version: str,
    engine: str,
) -> dict:
    """Build a canonical page from layout analysis."""
    out_blocks: list[dict] = []
    for index, layout in enumerate(blocks):
        built = _layout_block(layout, index, page, lines, engine_version, engine)
        if built is not None:
            out_blocks.append(built)

    scored = [b["confidence"] for b in out_blocks if b.get("confidence") is not None]
    confidence = sum(scored) / len(scored) if scored else 0.0
    reasons = ["ocr", "layout_analysis"] if out_blocks else ["ocr", "no_text_found"]

    return {
        "number": page.number,
        "kind": "paginated",
        "width": round(page.width_pt, 3),
        "height": round(page.height_pt, 3),
        "classification": {
            "type": "scanned",
            "confidence": round(min(1.0, max(0.0, confidence)), 6),
            "reasons": reasons,
        },
        "blocks": out_blocks,
    }


def _layout_block(
    layout: LayoutBlock,
    index: int,
    page: RasteredPage,
    lines: list[Line],
    engine_version: str,
    engine: str,
) -> dict | None:
    block_type = LABEL_TO_TYPE.get(layout.label, "paragraph")
    block_id = f"p{page.number}-ly{index}"
    # The model's own label rides along in provenance, so a consumer can tell a
    # heading that came from `doc_title` from one that came from `table_title`,
    # and nothing is lost to the mapping above.
    provenance = {
        "engine": engine,
        "engine_version": engine_version,
        "method": f"pp-structurev3/layout:{layout.label}",
    }

    out: dict = {"id": block_id, "type": block_type, "provenance": provenance}

    bbox = _bbox(layout.x0, layout.y0, layout.x1, layout.y1, page)
    if bbox is not None:
        out["bbox"] = bbox

    if block_type == "table":
        grid, header_rows = parse_table_html(layout.content)
        if not grid:
            return None
        out["table"] = {
            "header_rows": header_rows,
            "kind": "data",
            "grid": [[_cell(slot, block_id, r, c, provenance) for c, slot in enumerate(row)]
                     for r, row in enumerate(grid)],
        }
        text_for_confidence = table_text(grid)
    elif block_type == "image":
        # No crop is extracted, so the block records that a figure is here and
        # where, without pretending to have its bytes.
        out["alt"] = layout.label
        text_for_confidence = ""
    else:
        text = " ".join(layout.content.split())
        if not text:
            return None
        out["text"] = text
        if block_type == "heading":
            out["level"] = LABEL_TO_LEVEL.get(layout.label, 2)
        text_for_confidence = text

    confidence = _confidence(layout, lines, text_for_confidence)
    if confidence is not None:
        out["confidence"] = round(confidence, 6)
    return out


def _cell(slot: dict, block_id: str, row: int, col: int, provenance: dict) -> dict:
    if "covered_by" in slot:
        return {"covered_by": slot["covered_by"]}
    cell: dict = {
        "row_span": slot.get("row_span", 1),
        "col_span": slot.get("col_span", 1),
    }
    text = slot.get("text", "")
    if text:
        cell["blocks"] = [{
            "id": f"{block_id}-r{row}c{col}",
            "type": "paragraph",
            "text": text,
            "provenance": provenance,
        }]
    return cell


def _bbox(x0: float, y0: float, x1: float, y1: float, page: RasteredPage) -> dict | None:
    left, bottom = page.to_points(x0, y1)
    right, top = page.to_points(x1, y0)
    width, height = right - left, top - bottom
    if width <= 0 or height <= 0:
        return None
    return {
        "x": round(left, 3),
        "y": round(bottom, 3),
        "width": round(width, 3),
        "height": round(height, 3),
    }


# ---------------------------------------------------------------------------
# Tier 3: MinerU layout blocks
# ---------------------------------------------------------------------------

# MinerU's own block types, mapped onto canonical ones. Anything unlisted
# becomes a paragraph and keeps its original label in provenance, so a new
# upstream type degrades to readable text rather than disappearing.
MINERU_LABEL_TO_TYPE = {
    "text": "paragraph",
    "header": "paragraph",
    "footer": "paragraph",
    "table": "table",
    "list": "list",
    "equation": "formula",
    "interline_equation": "formula",
    "image": "image",
    "figure": "image",
    "code": "code",
    "algorithm": "code",
}


def vision_page_payload(
    page_number: int,
    blocks: list[VisionBlock],
    page_width_pt: float,
    page_height_pt: float,
    engine_version: str,
    backend: str,
) -> dict:
    """Build a canonical page from MinerU's output.

    Takes a page number rather than a `RasteredPage`: MinerU opens the PDF
    itself, so this tier never goes through our rasterizer, and its geometry is
    relative to the page size MinerU saw.
    """
    out_blocks: list[dict] = []
    for index, block in enumerate(blocks):
        built = _vision_block(
            block, index, page_number, page_width_pt, page_height_pt, engine_version, backend
        )
        if built is not None:
            out_blocks.append(built)

    reasons = ["ocr", "vision"] if out_blocks else ["ocr", "vision", "no_text_found"]
    return {
        "number": page_number,
        "kind": "paginated",
        "width": round(page_width_pt, 3),
        "height": round(page_height_pt, 3),
        "classification": {
            "type": "scanned",
            # MinerU reports no per-block confidence, so none is invented. A
            # page that produced blocks is taken at face value; the quality
            # scorer is what second-guesses it, as with every other tier.
            "confidence": 1.0 if out_blocks else 0.0,
            "reasons": reasons,
        },
        "blocks": out_blocks,
    }


def _vision_block(
    block: VisionBlock,
    index: int,
    page_number: int,
    width_pt: float,
    height_pt: float,
    engine_version: str,
    backend: str,
) -> dict | None:
    block_type = MINERU_LABEL_TO_TYPE.get(block.label, "paragraph")
    block_id = f"p{page_number}-vis{index}"
    provenance = {
        "engine": VISION_ENGINE,
        "engine_version": engine_version,
        # The backend is recorded because it is the difference between a real
        # third tier and a second run of Tier 2's model family.
        "method": f"mineru/{backend}:{block.label}",
    }

    out: dict = {"id": block_id, "type": block_type, "provenance": provenance}

    bbox = _vision_bbox(block, width_pt, height_pt)
    if bbox is not None:
        out["bbox"] = bbox

    if block_type == "table":
        grid, header_rows = parse_table_html(block.text)
        if not grid:
            return None
        out["table"] = {
            "header_rows": header_rows,
            "kind": "data",
            "grid": [
                [_cell(slot, block_id, r, c, provenance) for c, slot in enumerate(row)]
                for r, row in enumerate(grid)
            ],
        }
    elif block_type == "image":
        out["alt"] = block.label
    else:
        text = " ".join(block.text.split())
        if not text:
            return None
        out["text"] = text
        if block_type == "heading":
            out["level"] = block.text_level or 2
        elif block.text_level:
            # MinerU marks headings with text_level on an ordinary text block.
            out["type"] = "heading"
            out["level"] = min(max(block.text_level, 1), 6)
    return out


def _vision_bbox(block: VisionBlock, width_pt: float, height_pt: float) -> dict | None:
    """Convert MinerU's 0-1000 top-left box into PDF points, bottom-left.

    Two conversions at once: the 0-1000 normalization back to points, and the
    vertical flip. Verified against Tier 2's output on the repository's table
    fixture -- both place that table at x 70-483 with its top edge at y 650.
    """
    left = block.x0 / 1000.0 * width_pt
    right = block.x1 / 1000.0 * width_pt
    top = height_pt - (block.y0 / 1000.0 * height_pt)
    bottom = height_pt - (block.y1 / 1000.0 * height_pt)

    width, height = right - left, top - bottom
    if width <= 0 or height <= 0:
        return None
    return {
        "x": round(left, 3),
        "y": round(bottom, 3),
        "width": round(width, 3),
        "height": round(height, 3),
    }


def _confidence(layout: LayoutBlock, lines: list[Line], text: str) -> float | None:
    """How well this region's characters were read.

    Computed from the OCR lines whose centres fall inside the region, weighted
    by length. The layout model's own score is a different quantity -- how sure
    it is that a table is a table -- and is only used when no line matched,
    which is the case for a figure.
    """
    inside = [
        line
        for line in lines
        if layout.x0 <= (line.x0 + line.x1) / 2 <= layout.x1
        and layout.y0 <= (line.y0 + line.y1) / 2 <= layout.y1
    ]
    total = sum(len(line.text) for line in inside)
    if total > 0:
        return sum(line.confidence * len(line.text) for line in inside) / total
    if not text:
        return layout.det_score
    return layout.det_score



def error_output(kind: str, message: str) -> dict:
    """The same failure envelope the Rust shim writes, so the Go client has one
    way to classify a failure regardless of which engine produced it."""
    return {"schema_version": SCHEMA_VERSION, "kind": kind, "message": message}
