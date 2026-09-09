"""Tier 3: which engine reads the pages the OCR tiers lose.

The tier is a slot with two engines in it. Everything that decides *when* it
runs -- the quality threshold, the per-document cap, the disagreement probe,
the rule that a vision result replaces an OCR one only when the call succeeds
-- lives in the Go router and is engine-independent. This module is the only
place that knows which engine is in the slot.

    DOLICO_VISION_ENGINE=mineru    the default, and the only one with
                                   measurements behind it
    DOLICO_VISION_ENGINE=glm-ocr   see docs/glm-ocr-tier-design.md

The default is deliberate and should stay until the benchmark says otherwise.
`docs/vision-tier-design.md` has MinerU's numbers on this repository's corpus:
mean CER 0.011, and one missed letter on a real 1922 microfilm scan the OCR
tier gets 54% of the words wrong on. Swapping the default engine for one nobody
has scored would trade a measurement for a hope.

An unknown value raises rather than falling back. A deployment that thinks it
is running GLM-OCR and is silently running MinerU would attribute one engine's
output to the other in provenance and in the page cache, which is a worse
failure than not starting.
"""

from __future__ import annotations

import os

from . import vision_glm, vision_mineru
from .vision_base import VisionBlock, VisionError, server_url  # noqa: F401

# Name -> the module implementing it. Each exposes ENGINE_NAME, available()
# and an engine class; see `vision_base.VisionAdapter` for the contract.
ENGINES = {
    vision_mineru.ENGINE_NAME: (vision_mineru, vision_mineru.MineruEngine),
    vision_glm.ENGINE_NAME: (vision_glm, vision_glm.GlmEngine),
}

DEFAULT_ENGINE = vision_mineru.ENGINE_NAME


def _selected() -> str:
    name = (os.environ.get("DOLICO_VISION_ENGINE") or DEFAULT_ENGINE).strip().lower()
    if name not in ENGINES:
        raise VisionError(
            f"DOLICO_VISION_ENGINE={name!r} is not an engine; "
            f"expected one of {', '.join(sorted(ENGINES))}"
        )
    return name


ENGINE_NAME = _selected()

_MODULE, _ENGINE_CLASS = ENGINES[ENGINE_NAME]


def available() -> bool:
    """Whether the selected engine can actually be used here.

    Asked of the engine rather than answered here: what makes an engine usable
    differs between them. MinerU is usable when it imports; GLM-OCR needs an
    endpoint, because installed-but-unconfigured would otherwise reach for a
    cloud API this pipeline does not use.
    """
    return _MODULE.available()


def new_engine():
    """The Tier 3 engine this process serves.

    A live instance either way: `describe()` and `available()` have to answer
    from `/healthz` whether or not the tier is installed, and an engine that
    cannot load says so when a page is actually escalated to it.
    """
    return _ENGINE_CLASS()
