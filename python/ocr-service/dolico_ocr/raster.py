"""Rasterize document pages for OCR.

OCR needs pixels; the pipeline upstream of here deals only in PDF internals.
This is the one place in the whole system that renders a page, which is why
pdfium lives here and not in the Go binary or the Rust shim.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np
import pypdfium2 as pdfium

# 200 DPI is the usual sweet spot for OCR: enough resolution for 8pt body text,
# without the memory and latency of 300 DPI on a full page.
DEFAULT_DPI = 200

# A PDF point is 1/72 inch, which is what makes DPI and scale interchangeable.
POINTS_PER_INCH = 72.0

# The pixel budget for a standalone image, matched to what this service already
# rasterizes without trouble: a letter page at DEFAULT_DPI is 1700x2200, or
# 3.7MP. Above this an image is downscaled, which costs nothing in accuracy --
# PaddleOCR's detector resizes internally anyway -- and bounds the memory a
# single upload can demand.
MAX_OCR_PIXELS = 4_000_000

# Beyond this, refuse rather than decode. Pillow reads the dimensions from the
# header before allocating anything, so a 400MP image is rejected for the cost
# of parsing its header instead of 1.2GB of RGB. Nothing anyone wants read is
# this large; a decompression bomb is.
MAX_DECODE_PIXELS = 80_000_000


class RasterError(Exception):
    """The document could not be opened or rendered."""


@dataclass(frozen=True)
class RasteredPage:
    """One rendered page, with everything needed to map pixels back to points."""

    number: int
    """1-indexed page number, matching the canonical model."""

    image: np.ndarray
    """RGB pixels, shape (height, width, 3)."""

    width_pt: float
    height_pt: float
    """Page size in PDF points. Unlike the pdf-inspector path, the OCR path
    genuinely knows the page dimensions, so canonical pages from here carry
    real geometry."""

    scale: float
    """Pixels per point. `pixel / scale` is points."""

    def to_points(self, x_px: float, y_px: float) -> tuple[float, float]:
        """Convert a raster coordinate to PDF user space.

        Raster coordinates are top-down with the origin at the top-left;
        PDF user space is bottom-up with the origin at the bottom-left. Getting
        this flip wrong produces bounding boxes that are mirrored vertically
        and look almost right, which is worse than looking obviously wrong.
        """
        return x_px / self.scale, self.height_pt - (y_px / self.scale)


def is_pdf(data: bytes) -> bool:
    return data[:5] == b"%PDF-"


def render_pdf_pages(
    data: bytes, pages: list[int] | None = None, dpi: int = DEFAULT_DPI
) -> list[RasteredPage]:
    """Render the requested 1-indexed pages. `pages=None` renders all of them.

    Page numbers outside the document are skipped rather than raising: the
    router works from a classification that may disagree with what this
    document actually contains, and losing the pages that do exist because one
    number was wrong would be the wrong trade.
    """
    if not is_pdf(data):
        raise RasterError("not a PDF")
    try:
        doc = pdfium.PdfDocument(data)
    except Exception as exc:  # pdfium raises a variety of types
        raise RasterError(f"cannot open PDF: {exc}") from exc

    scale = dpi / POINTS_PER_INCH
    try:
        count = len(doc)
        wanted = list(range(1, count + 1)) if pages is None else sorted(set(pages))

        out: list[RasteredPage] = []
        for number in wanted:
            if number < 1 or number > count:
                continue
            page = doc[number - 1]
            try:
                width_pt, height_pt = page.get_size()
                image = page.render(scale=scale).to_numpy()
                # pypdfium2 may hand back BGRA or RGB depending on the page;
                # PaddleOCR wants three channels.
                if image.ndim == 3 and image.shape[2] == 4:
                    image = image[:, :, :3]
                out.append(
                    RasteredPage(
                        number=number,
                        image=np.ascontiguousarray(image),
                        width_pt=float(width_pt),
                        height_pt=float(height_pt),
                        scale=scale,
                    )
                )
            finally:
                page.close()
        return out
    finally:
        doc.close()


def wrap_image(data: bytes) -> list[RasteredPage]:
    """Treat a standalone image as a single page.

    An image has no intrinsic physical size, so it is reported at 72 DPI --
    one pixel to one point. The bounding boxes are then in pixels-as-points,
    which is the only honest reading when the source carries no page geometry.

    Unlike a PDF, whose raster size this service chooses via DPI, an image
    arrives at whatever resolution a camera happened to use. A 48MP phone
    photo decodes to roughly 140MB of RGB before PaddleOCR allocates its own
    working copies, which is how a single upload OOM-kills a service that
    handles 200 DPI pages all day -- and it takes every concurrent request
    down with it, not just the one that caused it. So the pixels handed to OCR
    are bounded to what the PDF path already produces.
    """
    try:
        import io

        from PIL import Image  # Pillow arrives as a PaddleOCR dependency.

        with Image.open(io.BytesIO(data)) as img:
            width, height = img.size
            if width * height > MAX_DECODE_PIXELS:
                raise RasterError(
                    f"image is {width}x{height}; the limit is "
                    f"{MAX_DECODE_PIXELS // 1_000_000}MP"
                )
            target = _fit(width, height, MAX_OCR_PIXELS)
            # draft() lets the JPEG decoder downscale as it reads, so the full
            # resolution is never allocated at all. It is a no-op for formats
            # that cannot, which the resize below then handles.
            img.draft("RGB", target)
            rgb_img = img.convert("RGB")
            if rgb_img.size != target and rgb_img.width * rgb_img.height > MAX_OCR_PIXELS:
                rgb_img = rgb_img.resize(target, Image.Resampling.LANCZOS)
            rgb = np.asarray(rgb_img)
    except RasterError:
        raise
    except Exception as exc:
        raise RasterError(f"cannot decode image: {exc}") from exc

    # The page keeps the source's own pixel dimensions whatever was rasterized,
    # so a caller's bounding boxes land on the image they uploaded. `scale`
    # carries the reduction, exactly as it carries DPI on the PDF path.
    raster_width = rgb.shape[1]
    return [
        RasteredPage(
            number=1,
            image=rgb,
            width_pt=float(width),
            height_pt=float(height),
            scale=raster_width / float(width),
        )
    ]


def _fit(width: int, height: int, budget: int) -> tuple[int, int]:
    """The largest size with this aspect ratio within a pixel budget."""
    if width * height <= budget:
        return width, height
    ratio = (budget / (width * height)) ** 0.5
    return max(1, int(width * ratio)), max(1, int(height * ratio))
