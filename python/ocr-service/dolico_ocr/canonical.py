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
from .vision_base import VisionBlock


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
# Tier 3: vision-engine layout blocks
# ---------------------------------------------------------------------------

# Each engine's own block types, mapped onto canonical ones. Anything unlisted
# becomes a paragraph and keeps its original label in provenance, so a new
# upstream type degrades to readable text rather than disappearing -- which is
# also why neither table lists every label the engine can emit.

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

# GLM-OCR reports PP-DocLayoutV3's native labels, which are finer-grained than
# MinerU's: two kinds of title, two kinds of formula, and several kinds of
# marginal text that all mean "a paragraph, somewhere unusual on the page".
# Only the ones that are not paragraphs are listed.
#
# The two title labels map to `heading` and the adapter sets the level to match
# (`vision_glm._NATIVE_HEADING_LEVELS`), so the depth comes from what the layout
# model detected rather than from counting the `#` characters GLM-OCR's
# formatter writes into the text.
GLM_LABEL_TO_TYPE = {
    "doc_title": "heading",
    "paragraph_title": "heading",
    "table": "table",
    "display_formula": "formula",
    "inline_formula": "formula",
    "image": "image",
    "chart": "image",
    "algorithm": "code",
}

# Not to be confused with Tier 2's `LABEL_TO_TYPE` above. The two are separate
# on purpose even though GLM-OCR's vocabulary overlaps it almost exactly --
# both come from the PP-DocLayout family, which is the asymmetry recorded in
# docs/glm-ocr-tier-design.md. Merging them would hide that rather than fix it,
# and would couple a tier's mapping to another tier's model.
VISION_LABEL_TO_TYPE = {
    "mineru": MINERU_LABEL_TO_TYPE,
    "glm-ocr": GLM_LABEL_TO_TYPE,
}


def vision_page_payload(
    page_number: int,
    blocks: list[VisionBlock],
    page_width_pt: float,
    page_height_pt: float,
    engine: str,
    engine_version: str,
    backend: str,
) -> dict:
    """Build a canonical page from a vision engine's output.

    Takes a page number rather than a `RasteredPage` because the two engines
    disagree about who renders: MinerU opens the PDF itself, and the GLM-OCR
    adapter goes through our rasterizer but reports the page size it got from
    there. Either way the geometry is relative to a page size the caller knows
    and this function does not have to rediscover.
    """
    out_blocks: list[dict] = []
    for index, block in enumerate(blocks):
        out_blocks.extend(_vision_block(
            block, index, page_number, page_width_pt, page_height_pt,
            engine, engine_version, backend,
        ))

    reasons = ["ocr", "vision"] if out_blocks else ["ocr", "vision", "no_text_found"]
    return {
        "number": page_number,
        "kind": "paginated",
        "width": round(page_width_pt, 3),
        "height": round(page_height_pt, 3),
        "classification": {
            "type": "scanned",
            # Neither vision engine reports per-block confidence, so none is
            # invented. A page that produced blocks is taken at face value; the
            # quality scorer is what second-guesses it, as with every tier.
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
    engine: str,
    engine_version: str,
    backend: str,
) -> list[dict]:
    """Convert one vision block. Usually one canonical block, sometimes several.

    Several because a single-column "table" is not a table -- see below.
    """
    block_type = VISION_LABEL_TO_TYPE.get(engine, {}).get(block.label, "paragraph")
    block_id = f"p{page_number}-vis{index}"
    provenance = {
        "engine": engine,
        "engine_version": engine_version,
        # The backend is recorded because it is the difference between a real
        # third tier and a second run of Tier 2's model family -- and, for an
        # engine reached over HTTP, the only record of which model answered.
        "method": f"{engine}/{backend}:{block.label}",
    }

    out: dict = {"id": block_id, "type": block_type, "provenance": provenance}

    bbox = _vision_bbox(block, width_pt, height_pt)
    if bbox is not None:
        out["bbox"] = bbox

    if block_type == "table":
        grid, header_rows = parse_table_html(block.text)
        if not grid:
            return []
        if max((len(row) for row in grid), default=0) <= 1:
            # A one-column table is a stack of paragraphs wearing a grid.
            #
            # MinerU does this to ordinary text set in a narrow column: the
            # repository's faded receipt comes back as 8x1 and the 1922
            # newspaper column as 9x1, while the fixture that really is a table
            # comes back as 5x3. Emitting them as tables would put structure
            # into the canonical model that is not in the document, which is
            # the one thing this pipeline refuses to do -- and it would do it
            # on exactly the pages the vision tier exists to rescue.
            return _flatten_single_column(grid, block_id, provenance)
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
            return []
        out["text"] = text
        if block_type == "heading":
            out["level"] = block.text_level or 2
        elif block.text_level:
            # MinerU marks headings with text_level on an ordinary text block.
            out["type"] = "heading"
            out["level"] = min(max(block.text_level, 1), 6)
    return [out]


def _flatten_single_column(grid: list, block_id: str, provenance: dict) -> list[dict]:
    """One paragraph per row of a table that has only one column.

    No bounding box on any of them. The engine measured the region, not the
    rows inside it, and giving every paragraph the region's rectangle would
    hand a consumer several identical overlapping boxes that nothing ever
    measured. Absent geometry is recoverable; invented geometry is not.
    """
    out = []
    for r, row in enumerate(grid):
        slot = row[0] if row else {}
        text = " ".join(str(slot.get("text", "")).split())
        if not text or "covered_by" in slot:
            continue
        out.append({
            "id": f"{block_id}-r{r}",
            "type": "paragraph",
            "text": text,
            "provenance": provenance,
        })
    return out


def _vision_bbox(block: VisionBlock, width_pt: float, height_pt: float) -> dict | None:
    """Convert a 0-1000 top-left box into PDF points, bottom-left.

    Two conversions at once: the 0-1000 normalization back to points, and the
    vertical flip. Verified against Tier 2's output on the repository's table
    fixture -- both place that table at x 70-483 with its top edge at y 650.

    Both engines report this space natively (MinerU in `content_list.json`,
    GLM-OCR from its layout detector), which is why `vision_base` pins it as
    the contract: this arithmetic is easy to get subtly and invisibly wrong,
    and it is worth having exactly one copy of it under exactly one test.
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
