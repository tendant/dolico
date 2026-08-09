"""Tier 3: MinerU2.5 — the fallback for pages the OCR tiers lose.

Tiers 1 and 2 fail the same way, because they are the same kind of thing: a
detector trained on documents that look like documents. A page they both lose
needs a different kind of model, not another attempt with the same one.

## The backend choice is the whole point

MinerU ships several backends and they are not interchangeable here:

  * ``pipeline`` is PP-OCR based -- the same model family as Tier 2. Using it
    as Tier 3 would mean handing a failed page to a near-copy of what just
    failed it. Measured on this repository's ``scanned-table.pdf`` it does beat
    Tier 2 on the table, but still merges words the same way ("All figuresare").
  * ``hybrid-engine`` is a 1.2B vision-language model. On the same fixture it
    is the only engine tested that reads the page completely correctly.

So the default here is ``hybrid-engine``, and that default is load-bearing
rather than incidental.

## What this buys over a hosted reasoning model

MinerU *measures* geometry instead of guessing it, so escalating a page to
Tier 3 keeps its bounding boxes -- a hosted language model asked for pixel
coordinates returns plausible fiction, and this pipeline does not fabricate
geometry anywhere. It also costs nothing per page, sends no document anywhere,
and can be tested end to end on a developer machine.

The price is resources: roughly 8GB of model weights and several seconds per
page. Which is why this only ever runs on pages that already failed twice.
"""

from __future__ import annotations

import glob
import json
import logging
import os
import tempfile
import threading
from dataclasses import dataclass

log = logging.getLogger(__name__)

ENGINE_NAME = "mineru"

# See the module docstring: `pipeline` is Tier 2's own model family and is the
# wrong choice for a tier whose job is to succeed where Tier 2 failed.
DEFAULT_BACKEND = "hybrid-engine"


class VisionError(Exception):
    """The page could not be read. The caller keeps its OCR result."""


@dataclass(frozen=True)
class VisionBlock:
    """One block as MinerU reports it, before canonical mapping."""

    label: str
    """MinerU's own type: text, header, footer, table, list, equation, image."""

    text: str
    """Plain text, or an HTML fragment when `is_table`."""

    x0: float
    y0: float
    x1: float
    y1: float
    """Extent, normalized to 0-1000 with a top-left origin, as MinerU reports
    it in content_list.json."""

    text_level: int | None = None
    """Heading depth when MinerU marks one; None for body text."""

    @property
    def is_table(self) -> bool:
        return self.label == "table"


def available() -> bool:
    """Whether MinerU can actually be used here.

    Importing the parse entry point is the honest check: the package pulls in
    torch and a model stack, and a partial install fails at import rather than
    at first use.
    """
    try:
        from mineru.cli.common import do_parse  # noqa: F401,PLC0415

        return True
    except Exception:
        return False


class VisionEngine:
    """MinerU behind a lock, matching the other two tiers.

    Serialized for the same reason: one model, one inference at a time, and
    concurrency comes from processes rather than threads.
    """

    def __init__(self) -> None:
        self.backend = os.environ.get("DOLICO_MINERU_BACKEND", DEFAULT_BACKEND)
        self.effort = os.environ.get("DOLICO_MINERU_EFFORT", "medium")
        self.lang = os.environ.get("DOLICO_OCR_LANG", "en")
        # When set, MinerU runs as its own service and this process only talks
        # to it. That keeps ~8GB of model weights out of a service already
        # measured at ~3GB per worker -- see docs/vision-tier-design.md.
        self.server_url = os.environ.get("DOLICO_MINERU_URL") or None
        if self.server_url:
            self.backend = _remote_backend(self.backend)
        self._lock = threading.Lock()
        self._loaded = False
        self._version = "unknown"

    def load(self) -> None:
        """Import MinerU and record its version.

        Models are fetched lazily by MinerU on first parse, so this is cheap;
        the first page of the first document pays the download.
        """
        if self._loaded:
            return
        if not available():
            raise VisionError(
                "MinerU is not installed; install it with "
                "`uv sync --extra vision` in python/ocr-service"
            )
        try:
            from importlib.metadata import version

            self._version = version("mineru")
        except Exception:  # pragma: no cover - version is cosmetic
            pass
        log.info(
            "vision tier ready (mineru=%s backend=%s effort=%s remote=%s)",
            self._version,
            self.backend,
            self.effort,
            self.server_url or "no",
        )
        self._loaded = True

    @property
    def version(self) -> str:
        return self._version

    @property
    def loaded(self) -> bool:
        return self._loaded

    def describe(self) -> dict[str, str]:
        return {
            "backend": self.backend,
            "effort": self.effort,
            "server_url": self.server_url or "",
        }

    def read(self, pdf_bytes: bytes, page_number: int) -> tuple[list[VisionBlock], float, float]:
        """Read one 1-indexed page. Returns its blocks and size in PDF points.

        One call per page, deliberately. MinerU's page selection is a
        contiguous range, and escalated pages are typically scattered -- page 2
        and page 7, not 2 through 7. Parsing the span between them would cost
        far more than the extra calls.
        """
        if not self._loaded:
            self.load()

        from mineru.cli.common import do_parse  # noqa: PLC0415

        index = page_number - 1
        if index < 0:
            raise VisionError(f"page numbers are 1-indexed, got {page_number}")

        with tempfile.TemporaryDirectory(prefix="dolico-vision-") as out_dir:
            with self._lock:
                try:
                    do_parse(
                        output_dir=out_dir,
                        pdf_file_names=["page"],
                        pdf_bytes_list=[pdf_bytes],
                        p_lang_list=[self.lang],
                        backend=self.backend,
                        start_page_id=index,
                        end_page_id=index,
                        effort=self.effort,
                        server_url=self.server_url,
                        # Only the structured output is wanted. The rendered
                        # debug PDFs and the copy of the original are pure cost
                        # in a service.
                        f_draw_layout_bbox=False,
                        f_draw_span_bbox=False,
                        f_dump_orig_pdf=False,
                        f_dump_model_output=False,
                        f_dump_md=False,
                    )
                except Exception as exc:
                    raise VisionError(f"MinerU failed on page {page_number}: {exc}") from exc

            blocks = _read_content_list(out_dir, page_number)
            width, height = _read_page_size(out_dir)

        return blocks, width, height


def _remote_backend(backend: str) -> str:
    """Map a local backend onto its http-client equivalent."""
    if backend.endswith("-http-client"):
        return backend
    if backend.startswith("hybrid"):
        return "hybrid-http-client"
    if backend.startswith("vlm"):
        return "vlm-http-client"
    # `pipeline` has no remote form; MinerU would reject it, so say so here
    # rather than letting it fail deep inside the parse.
    raise VisionError(f"backend {backend!r} cannot run against a remote MinerU server")


def _find(out_dir: str, suffix: str) -> str:
    """Locate an output file.

    Globbed rather than constructed: MinerU nests results under a
    backend-dependent directory (`auto` for pipeline, `hybrid_auto` for
    hybrid), and hardcoding that would break on a backend change.
    """
    matches = glob.glob(os.path.join(out_dir, "**", f"*{suffix}"), recursive=True)
    if not matches:
        raise VisionError(f"MinerU produced no {suffix}")
    return matches[0]


def _read_content_list(out_dir: str, page_number: int) -> list[VisionBlock]:
    with open(_find(out_dir, "_content_list.json"), encoding="utf-8") as fh:
        raw = json.load(fh)

    blocks: list[VisionBlock] = []
    for entry in raw:
        label = str(entry.get("type") or "")
        bbox = entry.get("bbox")
        if not label or not bbox or len(bbox) < 4:
            continue

        if label == "table":
            content = entry.get("table_body") or ""
        else:
            content = entry.get("text") or ""
        if not str(content).strip():
            continue

        level = entry.get("text_level")
        try:
            level = int(level) if level is not None else None
        except (TypeError, ValueError):
            level = None

        x0, y0, x1, y1 = (float(v) for v in bbox[:4])
        blocks.append(
            VisionBlock(
                label=label,
                text=str(content),
                x0=min(x0, x1),
                y0=min(y0, y1),
                x1=max(x0, x1),
                y1=max(y0, y1),
                text_level=level,
            )
        )

    # MinerU's list order is not always reading order -- on this repository's
    # table fixture the page header comes last. Sorting by position is a better
    # default than trusting it, and unlike PP-StructureV3 there is no partial
    # order index to prefer.
    blocks.sort(key=lambda b: (round(b.y0), b.x0))
    return blocks


def _read_page_size(out_dir: str) -> tuple[float, float]:
    """Page dimensions in true PDF points, from middle.json.

    content_list.json normalizes its boxes to 0-1000, so the real page size is
    what makes them convertible back to points.
    """
    with open(_find(out_dir, "_middle.json"), encoding="utf-8") as fh:
        middle = json.load(fh)

    pages = middle.get("pdf_info") or []
    if not pages:
        raise VisionError("MinerU produced no page info")
    size = pages[0].get("page_size")
    if not size or len(size) < 2:
        raise VisionError("MinerU reported no page size")
    return float(size[0]), float(size[1])
