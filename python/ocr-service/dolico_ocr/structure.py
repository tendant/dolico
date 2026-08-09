"""PP-StructureV3: the layout-analysis tier.

Tier 1 detects text lines and this module detects *what the page is made of* --
headings, paragraphs, figures, and tables with their grid recovered. On a
scanned table that is the difference between eighteen loose fragments and a
5x3 grid, which is a correctness difference rather than a cosmetic one.

It is optional. PP-StructureV3 needs the `paddlex[ocr]` extras, and when they
are absent the service says so plainly and falls back to Tier 1 rather than
failing to start.
"""

from __future__ import annotations

import logging
import os
import threading
from dataclasses import dataclass

import numpy as np

from .engine import DEFAULT_DET_MODEL, DEFAULT_REC_MODEL, _flag
from .layout import Line, lines_from_prediction

log = logging.getLogger(__name__)

ENGINE_NAME = "pp-structurev3"


@dataclass(frozen=True)
class LayoutBlock:
    """One region of the page, as the layout model sees it."""

    label: str
    """The model's own label: text, header, table, figure, doc_title, ..."""

    content: str
    """Plain text, or an HTML fragment when `is_table`."""

    x0: float
    y0: float
    x1: float
    y1: float
    """Extent in raster pixels, top-down."""

    order: int | None = None
    """Reading-order index when the model assigned one."""

    det_score: float | None = None
    """The layout model's confidence in this region. Distinct from OCR
    confidence: it says how sure the model is that a table is a table, not how
    well the characters were read."""

    @property
    def is_table(self) -> bool:
        return self.label == "table"


def available() -> bool:
    """Whether PP-StructureV3 can actually be constructed here.

    Importing `PPStructureV3` succeeds without the extras; it is instantiation
    that fails. Rather than construct one just to find out -- which loads a
    dozen models -- the marker dependency is probed directly.
    """
    try:
        from paddlex.utils.deps import is_extra_available  # noqa: PLC0415

        return bool(is_extra_available("ocr"))
    except Exception:
        # Older or newer paddlex without that helper: fall back to importing
        # something the extras provide.
        try:
            import bs4  # noqa: F401,PLC0415
            import ftfy  # noqa: F401,PLC0415

            return True
        except Exception:
            return False


class StructureEngine:
    """A PP-StructureV3 pipeline behind a lock, as with Tier 1."""

    def __init__(self, lang: str = "en") -> None:
        self.lang = lang
        self.det_model = os.environ.get("DOLICO_OCR_DET_MODEL", DEFAULT_DET_MODEL)
        self.rec_model = os.environ.get("DOLICO_OCR_REC_MODEL", DEFAULT_REC_MODEL)
        # The table orientation classifier is off by default because it is
        # actively wrong on ordinary tables: on the repository's
        # scanned-table.pdf it rotates the recovered grid by 180 degrees, so
        # the header row comes out last and the columns come out reversed.
        # Leaving it off also roughly halves the time per page. Turn it on only
        # for a corpus that genuinely contains rotated tables.
        self.table_orientation = _flag("DOLICO_OCR_TABLE_ORIENTATION", False)
        self._lock = threading.Lock()
        self._pipeline = None
        self._version = "unknown"

    def load(self) -> None:
        if self._pipeline is not None:
            return
        from paddleocr import PPStructureV3

        try:
            import paddleocr

            self._version = getattr(paddleocr, "__version__", "unknown")
        except Exception:  # pragma: no cover - version is cosmetic
            pass

        log.info(
            "loading PP-StructureV3 (det=%s rec=%s table_orientation=%s)",
            self.det_model,
            self.rec_model,
            self.table_orientation,
        )
        # PP-StructureV3 would otherwise pick the *server* OCR models, which
        # cost roughly five times what the mobile ones do; the same models Tier
        # 1 uses keep the two tiers comparable and the page budget sane.
        self._pipeline = PPStructureV3(
            text_detection_model_name=self.det_model,
            text_recognition_model_name=self.rec_model,
            use_doc_orientation_classify=_flag("DOLICO_OCR_ORIENTATION", False),
            use_doc_unwarping=_flag("DOLICO_OCR_UNWARP", False),
            use_textline_orientation=_flag("DOLICO_OCR_TEXTLINE_ORIENTATION", False),
            use_table_recognition=_flag("DOLICO_OCR_TABLES", True),
            use_seal_recognition=_flag("DOLICO_OCR_SEALS", False),
            use_formula_recognition=_flag("DOLICO_OCR_FORMULAS", False),
            use_chart_recognition=_flag("DOLICO_OCR_CHARTS", False),
            use_region_detection=_flag("DOLICO_OCR_REGIONS", False),
        )
        log.info("PP-StructureV3 ready (version=%s)", self._version)

    @property
    def version(self) -> str:
        return self._version

    @property
    def loaded(self) -> bool:
        return self._pipeline is not None

    def describe(self) -> dict[str, str]:
        return {
            "lang": self.lang,
            "det_model": self.det_model,
            "rec_model": self.rec_model,
            "table_orientation_classify": str(self.table_orientation),
        }

    def read(self, image: np.ndarray) -> tuple[list[LayoutBlock], list[Line]]:
        """Analyze one page.

        Returns the layout blocks and, alongside them, the raw OCR lines. The
        lines are what give each block a text confidence: the layout result
        reports how sure the model is that a region is a table, not how well
        its characters were read.
        """
        if self._pipeline is None:
            self.load()
        with self._lock:
            results = self._pipeline.predict(
                image, use_table_orientation_classify=self.table_orientation
            )
        if not results:
            return [], []
        result = results[0]

        blocks = [
            block
            for block in (_to_block(raw, result) for raw in _get(result, "parsing_res_list", []))
            if block is not None
        ]
        blocks = _in_reading_order(blocks)

        overall = _get(result, "overall_ocr_res", None)
        lines = lines_from_prediction(_as_mapping(overall)) if overall is not None else []
        return blocks, lines


def _in_reading_order(blocks: list[LayoutBlock]) -> list[LayoutBlock]:
    """Put the regions in reading order.

    PP-StructureV3 populates `order_index` on only some regions -- on the
    repository's table fixture the two body paragraphs get 1 and 2 while the
    header and the table get nothing. Sorting on it regardless pushes every
    unnumbered region to the end, which moved the page's own title below the
    table.

    So it is used only when every region has one. Otherwise the model's own
    list order stands, which is already reading order and handles multi-column
    pages better than sorting by vertical position could.
    """
    if blocks and all(b.order is not None for b in blocks):
        return sorted(blocks, key=lambda b: b.order)
    return blocks


def _to_block(raw: object, result: object) -> LayoutBlock | None:
    label = str(getattr(raw, "label", "") or "")
    bbox = getattr(raw, "bbox", None)
    if not label or bbox is None or len(bbox) < 4:
        return None
    x0, y0, x1, y1 = (float(v) for v in list(bbox)[:4])

    content = getattr(raw, "content", "")
    if content is None:
        content = ""

    order = getattr(raw, "order_index", None)
    if order is not None:
        try:
            order = int(order)
        except (TypeError, ValueError):
            order = None

    return LayoutBlock(
        label=label,
        content=str(content),
        x0=min(x0, x1),
        y0=min(y0, y1),
        x1=max(x0, x1),
        y1=max(y0, y1),
        order=order,
        det_score=_detection_score(result, label, (x0, y0, x1, y1)),
    )


def _detection_score(result: object, label: str, bbox: tuple) -> float | None:
    """Find the layout detection score for a region, matched by label and
    position. The parsing list and the detection list are parallel views of the
    same regions but do not carry a shared id."""
    det = _get(result, "layout_det_res", None)
    if det is None:
        return None
    boxes = _get(det, "boxes", []) or []
    best, best_distance = None, float("inf")
    for box in boxes:
        if str(box.get("label", "")) != label:
            continue
        coords = box.get("coordinate")
        if coords is None or len(coords) < 4:
            continue
        distance = sum(abs(float(a) - float(b)) for a, b in zip(coords[:4], bbox))
        if distance < best_distance:
            best, best_distance = box, distance
    if best is None:
        return None
    try:
        return max(0.0, min(1.0, float(best.get("score"))))
    except (TypeError, ValueError):
        return None


def _get(obj: object, key: str, default=None):
    """PaddleOCR results behave like mappings but are not dicts, and nested
    values are sometimes plain objects."""
    try:
        return obj[key]
    except Exception:
        return getattr(obj, key, default)


def _as_mapping(result: object) -> dict:
    if isinstance(result, dict):
        return result
    if hasattr(result, "keys"):
        return {key: result[key] for key in result.keys()}
    return {}
