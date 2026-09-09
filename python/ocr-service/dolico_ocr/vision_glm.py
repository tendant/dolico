"""Tier 3, GLM-OCR: the other engine that reads pages the OCR tiers lose.

A 0.9B vision-language model from Z.ai. Its pipeline is two stages --
PP-DocLayoutV3 detects regions on a rendered page, then the VLM reads each one
in parallel with a task-specific prompt.

See `docs/glm-ocr-tier-design.md`. Three things from it are load-bearing here
and are implemented in this file rather than left to configuration:

**We rasterize, not GLM-OCR.** `parse()` accepts PDF bytes and would appear to
work, but its PDF path needs poppler -- a system package this image does not
carry -- and renders at its own DPI, so Tier 3 would be looking at a different
page than Tiers 1 and 2 for no stated reason. Handing it one page from
`raster.render_pdf_pages` fixes both, and hands us the page size in true points
for free. MinerU needed a second output file for that.

**MaaS is forced off.** `glmocr`'s shipped default is `maas.enabled: true`
pointing at open.bigmodel.cn, and a `ZHIPU_API_KEY` anywhere in the environment
flips it there even when the YAML says otherwise. Every engine in this pipeline
is local by deliberate choice; an engine whose default posts customer documents
to a third party would break that by accident. `mode="selfhosted"` is passed
explicitly, which also defeats the environment-variable flip.

**Its JSON is Markdown in disguise.** `content` arrives decorated for
rendering -- `"# Title"`, `"$$...$$"` -- and the canonical model carries a
heading level and a formula, not the Markdown that would render one. The
stripping happens here so that `VisionBlock.text` means the same thing for both
engines.
"""

from __future__ import annotations

import io
import logging
import os
import re
import threading
from urllib.parse import urlparse

from .vision_base import VisionBlock, VisionError, server_url

log = logging.getLogger(__name__)

ENGINE_NAME = "glm-ocr"

# The name the serving backend answers to, which is not the same everywhere:
# `--served-model-name glm-ocr` under vLLM or SGLang, `mlx-community/GLM-OCR-bf16`
# under MLX, `glm-ocr:latest` under Ollama.
DEFAULT_MODEL = "glm-ocr"

# PP-DocLayoutV3 is the cheap half of the pipeline and there is rarely a GPU
# here to give it. Where there is one, it is wanted for the VLM.
DEFAULT_LAYOUT_DEVICE = "cpu"

# Where the request goes, which is not the same on any two backends:
#
#   vLLM, SGLang    /v1/chat/completions   OpenAI-compatible, glmocr's default
#   mlx_vlm.server  /chat/completions      the same API without the /v1 prefix
#   Ollama          /api/generate          its native endpoint; the
#                                          OpenAI-compatible one 502s on vision
#
# Only the last is derivable, because it comes with `api_mode`. The MLX path
# differs from the default by three characters and nothing in the request says
# so, so it is taken from DOLICO_VISION_URL: give that variable a path and it
# is used verbatim.
API_PATHS = {"ollama_generate": "/api/generate", "openai": "/v1/chat/completions"}

# A leading run of `#` on a title region, which `_format_content` adds
# unconditionally. The level comes from the native label, not from counting
# these: reading the level back out of the syntax would make canonical
# structure depend on a formatter setting.
_HEADING_PREFIX = re.compile(r"^#+\s*")

_NATIVE_HEADING_LEVELS = {"doc_title": 1, "paragraph_title": 2}


def available() -> bool:
    """Whether GLM-OCR can actually be used here.

    Two conditions, and the second is the one that matters. Importing proves
    less than it does for MinerU: a `pip install glmocr` with no extras has no
    torch, no layout model and no way to know it cannot serve until it tries --
    it would simply reach for the cloud. Requiring an endpoint makes "installed
    but unconfigured" report unavailable, which is what it is.
    """
    if not server_url():
        return False
    try:
        from glmocr.api import GlmOcr  # noqa: F401,PLC0415

        return True
    except Exception:
        return False


class GlmEngine:
    """GLM-OCR behind a lock, matching the other tiers.

    The lock is inherited rather than justified. It is right for MinerU and for
    Paddle -- one model, one inference, concurrency from processes -- but a
    remote GLM-OCR does layout locally and then fans region recognition out over
    HTTP, which the library itself parallelizes. Holding a process-wide lock
    across that serializes network wait. Removing it is a change with a number
    attached, not a guess to make while adding the engine.
    """

    def __init__(self) -> None:
        self.model = os.environ.get("DOLICO_GLM_MODEL", DEFAULT_MODEL)
        self.layout_device = os.environ.get(
            "DOLICO_GLM_LAYOUT_DEVICE", DEFAULT_LAYOUT_DEVICE
        )
        # `openai` for vLLM, SGLang and MLX; `ollama_generate` for an Ollama
        # that 502s on vision requests to its OpenAI-compatible path.
        self.api_mode = os.environ.get("DOLICO_GLM_API_MODE", "openai")
        self.server_url = server_url()
        self._lock = threading.Lock()
        self._parser = None
        self._loaded = False
        self._version = "unknown"

    def load(self) -> None:
        """Build the parser, or say why not.

        Loud and early, like PP-StructureV3's failure: an unreachable endpoint
        discovered on the first escalated page of the first real document is
        the same defect found at the worst possible time.
        """
        if self._loaded:
            return
        if not self.server_url:
            raise VisionError(
                "GLM-OCR needs a model endpoint; set DOLICO_VISION_URL to a "
                "vLLM, SGLang, MLX or Ollama server"
            )
        try:
            from glmocr.api import GlmOcr  # noqa: PLC0415
        except Exception as exc:
            raise VisionError(
                "GLM-OCR is not installed; install it with "
                f"`uv sync --extra glm` in python/ocr-service ({exc})"
            ) from exc

        try:
            from importlib.metadata import version

            self._version = version("glmocr")
        except Exception:  # pragma: no cover - version is cosmetic
            pass

        try:
            self._parser = GlmOcr(**self._config())
        except Exception as exc:
            raise VisionError(f"GLM-OCR failed to start: {exc}") from exc

        log.info(
            "vision tier ready (glm-ocr=%s model=%s endpoint=%s layout=%s)",
            self._version,
            self.model,
            self.server_url,
            self.layout_device,
        )
        self._loaded = True

    def _config(self) -> dict:
        """Constructor arguments for `GlmOcr`.

        Split out because it is the part worth testing without a model: the
        MaaS guard, the endpoint, and the one post-processing switch we turn
        off are all decisions rather than plumbing.
        """
        url = urlparse(self.server_url or "")
        if not url.hostname:
            raise VisionError(
                f"DOLICO_VISION_URL is not a usable URL: {self.server_url!r}"
            )
        port = url.port or (443 if url.scheme == "https" else 80)
        # A path on the endpoint wins, because the caller wrote it down. "/" is
        # what a bare host URL parses to and means nothing, so it does not
        # count as one.
        path = url.path if url.path not in ("", "/") else API_PATHS.get(
            self.api_mode, API_PATHS["openai"]
        )

        return {
            # Not a default worth inheriting. See the module docstring.
            "mode": "selfhosted",
            "model": self.model,
            "ocr_api_host": url.hostname,
            "ocr_api_port": port,
            "layout_device": self.layout_device,
            "_dotted": {
                # The scheme is otherwise inferred from the port, which is
                # wrong for TLS on anything but 443.
                "pipeline.ocr_api.api_scheme": url.scheme or "http",
                "pipeline.ocr_api.api_mode": self.api_mode,
                "pipeline.ocr_api.api_path": path,
                # Leave the document's own bullets alone. This switch rewrites
                # a leading `·` or `•` into Markdown's `- `, which would put
                # rendering syntax into a canonical text field -- the two
                # decorations we cannot switch off are stripped below instead.
                "pipeline.result_formatter.enable_format_bullet_points": False,
            },
        }

    @property
    def version(self) -> str:
        return self._version

    @property
    def loaded(self) -> bool:
        return self._loaded

    @property
    def backend(self) -> str:
        """The served model, which is what actually read the page.

        MinerU records its backend here because that is the difference between
        a real third tier and a second run of Tier 2's model family. The
        equivalent question for a deployment that could be pointing at any
        OpenAI-compatible endpoint is which model answered.
        """
        return self.model

    def describe(self) -> dict[str, str]:
        return {
            "engine": ENGINE_NAME,
            "backend": self.model,
            "layout_device": self.layout_device,
            "api_mode": self.api_mode,
            "server_url": self.server_url or "",
        }

    def read(self, pdf_bytes: bytes, page_number: int) -> tuple[list[VisionBlock], float, float]:
        """Read one 1-indexed page. Returns its blocks and its size in points."""
        if not self._loaded:
            self.load()
        if page_number < 1:
            raise VisionError(f"page numbers are 1-indexed, got {page_number}")

        from .raster import DEFAULT_DPI, RasterError, render_pdf_pages  # noqa: PLC0415

        dpi = _dpi(DEFAULT_DPI)
        try:
            rendered = render_pdf_pages(pdf_bytes, [page_number], dpi)
        except RasterError as exc:
            raise VisionError(f"could not render page {page_number}: {exc}") from exc
        if not rendered:
            raise VisionError(f"page {page_number} does not exist in this document")

        page = rendered[0]
        image = _encode_png(page.image)

        with self._lock:
            try:
                result = self._parser.parse(image, save_layout_visualization=False)
            except Exception as exc:
                raise VisionError(f"GLM-OCR failed on page {page_number}: {exc}") from exc

        return _blocks(result.json_result), page.width_pt, page.height_pt


def _dpi(default: int) -> int:
    try:
        return max(72, min(600, int(os.environ.get("DOLICO_GLM_DPI", default))))
    except ValueError:
        return default


def _encode_png(image) -> bytes:
    """Encode a rendered page for the model.

    PNG rather than JPEG, which is what `glmocr` would have used had it done
    the rendering. This tier only ever sees pages that two engines already
    failed to read, and adding compression artifacts to marginal legibility is
    a strange thing to do to save a few hundred kilobytes over a loopback or a
    LAN.
    """
    from PIL import Image  # noqa: PLC0415

    buf = io.BytesIO()
    Image.fromarray(image).save(buf, format="PNG")
    return buf.getvalue()


def _blocks(json_result) -> list[VisionBlock]:
    """Map one page of GLM-OCR regions onto `VisionBlock`s.

    Reading order is trusted here, unlike in the MinerU adapter. GLM-OCR's
    formatter sorts regions by `index` and renumbers them, so the list order is
    the order it decided to read them in; MinerU has no such index, which is
    why that adapter sorts by position instead.
    """
    pages = json_result if isinstance(json_result, list) else []
    if not pages:
        return []
    # One image in, so one page out. A list of regions rather than a list of
    # pages means an older or unexpected shape; take it as the single page it
    # would have to be rather than returning nothing.
    regions = pages[0] if isinstance(pages[0], list) else pages

    blocks: list[VisionBlock] = []
    for item in regions:
        if not isinstance(item, dict):
            continue
        label = str(item.get("label") or "")
        native = str(item.get("native_label") or label)
        bbox = item.get("bbox_2d")
        if not native or not bbox or len(bbox) < 4:
            continue

        text, level = _plain(str(item.get("content") or ""), label, native)
        if not text.strip():
            # Includes figure regions, which carry no text and whose cropped
            # pixels this tier has nowhere to put -- the vision path writes no
            # assets. The MinerU adapter drops them for the same reason.
            continue

        try:
            x0, y0, x1, y1 = (float(v) for v in bbox[:4])
        except (TypeError, ValueError):
            continue

        blocks.append(
            VisionBlock(
                # The native label, not the four-way mapped one: it is strictly
                # more informative, it determines the mapped label anyway, and
                # it is what rides into provenance.
                label=native,
                text=text,
                x0=min(x0, x1),
                y0=min(y0, y1),
                x1=max(x0, x1),
                y1=max(y0, y1),
                text_level=level,
            )
        )
    return blocks


def _plain(content: str, label: str, native_label: str) -> tuple[str, int | None]:
    """Undo the Markdown that `_format_content` adds, and return the level.

    Only the decorations that cannot be switched off in configuration are
    handled here; bullets are turned off at the source in `_config`.
    """
    if label == "table":
        # HTML, and `canonical` hands it to the same table parser Tier 2 uses.
        return content.strip(), None

    if label == "formula":
        body = content.strip()
        if body.startswith("$$"):
            body = body[2:]
        if body.endswith("$$"):
            body = body[:-2]
        return body.strip(), None

    level = _NATIVE_HEADING_LEVELS.get(native_label)
    if level is not None:
        return _HEADING_PREFIX.sub("", content.strip()), level
    return content, None
