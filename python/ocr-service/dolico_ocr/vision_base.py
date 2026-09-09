"""The vocabulary Tier 3 engines share.

Tier 3 is a slot, not an engine. What makes a page escalate, how many pages may
escalate, whether the result is trusted over the OCR one -- all of that lives in
the router and is engine-independent, and it should stay that way. What varies
is only how a page of pixels becomes blocks, which is the contract below.

Two engines implement it today: MinerU (`vision_mineru`) and GLM-OCR
(`vision_glm`). `vision` selects between them.

The one thing this module fixes for every engine is the **coordinate space**:
boxes are normalized to 0-1000 with a top-left origin. Both current engines
report exactly that natively, which is luck rather than design, but pinning it
here is what lets `canonical._vision_bbox` own the conversion to PDF points --
including the vertical flip, which is the part that is easy to get subtly and
invisibly wrong -- once instead of once per engine.
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from typing import Protocol, runtime_checkable


class VisionError(Exception):
    """The page could not be read. The caller keeps its OCR result."""


def server_url() -> str | None:
    """The remote engine endpoint, under either name.

    Both engines want the same knob -- run the model somewhere else, so its
    weights are not resident in a service already measured at 6.3GB. MinerU had
    it first under `DOLICO_MINERU_URL`, which is kept as an alias, because
    renaming a variable is not a reason to break a running deployment.
    """
    return (
        os.environ.get("DOLICO_VISION_URL")
        or os.environ.get("DOLICO_MINERU_URL")
        or None
    )


@dataclass(frozen=True)
class VisionBlock:
    """One region as a Tier 3 engine reports it, before canonical mapping."""

    label: str
    """The engine's own name for this region -- `text`, `table`, `doc_title`.

    Deliberately the engine's vocabulary rather than a normalized one: it is
    what the per-engine label map keys on, and it rides into provenance so a
    reader can see what the engine thought it was looking at.
    """

    text: str
    """Plain text, or an HTML fragment when `is_table`.

    Plain means plain. An engine that decorates its output for rendering --
    GLM-OCR returns `"# Title"` and `"$$...$$"` -- strips that in its adapter,
    because the canonical model carries a heading level and a formula, not the
    Markdown that would render one.
    """

    x0: float
    y0: float
    x1: float
    y1: float
    """Extent, normalized to 0-1000 with a top-left origin."""

    text_level: int | None = None
    """Heading depth, when the engine marks one. None for body text."""

    @property
    def is_table(self) -> bool:
        return self.label == "table"


@runtime_checkable
class VisionAdapter(Protocol):
    """What `app` needs from a Tier 3 engine.

    Narrow on purpose. An adapter loads a model, reads one page, and says who
    it is; it does not decide when it runs, on which pages, or whether its
    answer is kept.
    """

    def load(self) -> None:
        """Make the engine ready, or raise `VisionError` saying why not.

        Called on the first read rather than at startup: Tier 3 is optional and
        frequently never reached, and a service that pays for it at boot would
        pay for it in every deployment that never escalates a page.
        """

    def read(self, pdf_bytes: bytes, page_number: int) -> tuple[list[VisionBlock], float, float]:
        """Read one 1-indexed page. Returns its blocks and its size in points.

        One call per page, for every engine. Escalated pages are scattered --
        page 2 and page 7, not 2 through 7 -- so batching would mean reading
        the pages in between, which is precisely the cost Tier 3 exists to
        avoid paying twice.
        """

    @property
    def version(self) -> str:
        """The engine build. Part of the page cache key, so an upgrade
        invalidates exactly the pages this engine produced."""

    @property
    def loaded(self) -> bool: ...

    @property
    def backend(self) -> str:
        """What actually read the page, recorded in provenance.

        For MinerU that is the backend name, which is the difference between a
        real third tier and a second run of Tier 2's model family. For GLM-OCR
        it is the served model, which is the same question asked of a
        deployment that could be pointing anywhere.
        """

    def describe(self) -> dict[str, str]:
        """Configuration worth reporting from `/healthz`."""
