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
    """
    try:
        import io

        from PIL import Image  # Pillow arrives as a PaddleOCR dependency.

        with Image.open(io.BytesIO(data)) as img:
            rgb = np.array(img.convert("RGB"))
    except Exception as exc:
        raise RasterError(f"cannot decode image: {exc}") from exc

    height, width = rgb.shape[:2]
    return [
        RasteredPage(
            number=1,
            image=rgb,
            width_pt=float(width),
            height_pt=float(height),
            scale=1.0,
        )
    ]
