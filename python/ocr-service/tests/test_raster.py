"""Rasterization and the pixel-to-point conversion.

The coordinate flip is the part worth testing: raster space is top-down and PDF
space is bottom-up, and getting it wrong produces boxes that are vertically
mirrored -- close enough to plausible that nothing else catches it.
"""

import pathlib

import numpy as np
import pytest

from dolico_ocr.raster import (
    DEFAULT_DPI,
    RasterError,
    RasteredPage,
    is_pdf,
    render_pdf_pages,
    wrap_image,
)

TESTDATA = pathlib.Path(__file__).resolve().parents[3] / "testdata"


def read(name: str) -> bytes:
    return (TESTDATA / name).read_bytes()


def test_is_pdf():
    assert is_pdf(b"%PDF-1.7\nrest")
    assert not is_pdf(b"PK\x03\x04")
    assert not is_pdf(b"")


class TestCoordinateConversion:
    def page(self):
        return RasteredPage(
            number=1,
            image=np.zeros((2200, 1700, 3), dtype=np.uint8),
            width_pt=612.0,
            height_pt=792.0,
            scale=2200 / 792,
        )

    def test_top_left_pixel_maps_to_top_left_point(self):
        # Raster (0,0) is the top-left; in PDF space that is y = page height.
        x, y = self.page().to_points(0, 0)
        assert x == pytest.approx(0.0)
        assert y == pytest.approx(792.0)

    def test_bottom_left_pixel_maps_to_the_origin(self):
        x, y = self.page().to_points(0, 2200)
        assert x == pytest.approx(0.0)
        assert y == pytest.approx(0.0, abs=1e-6)

    def test_x_scales_without_flipping(self):
        x, _ = self.page().to_points(1700, 0)
        assert x == pytest.approx(612.0, rel=1e-3)

    def test_y_is_monotonically_decreasing_in_pixels(self):
        page = self.page()
        _, high = page.to_points(0, 100)
        _, low = page.to_points(0, 900)
        assert high > low, "a pixel further down the page must be lower in PDF space"


class TestRenderPDF:
    def test_renders_every_page_by_default(self):
        pages = render_pdf_pages(read("text.pdf"))
        assert [p.number for p in pages] == [1, 2]

    def test_renders_only_the_requested_pages(self):
        pages = render_pdf_pages(read("text.pdf"), pages=[2])
        assert [p.number for p in pages] == [2]

    def test_page_numbers_are_one_indexed(self):
        # Page 1 must be the first page, not the second.
        first = render_pdf_pages(read("mixed.pdf"), pages=[1])[0]
        second = render_pdf_pages(read("mixed.pdf"), pages=[2])[0]
        assert first.number == 1 and second.number == 2
        # mixed.pdf page 1 is text on white; page 2 is a rendered image. They
        # must not be the same pixels.
        assert not np.array_equal(first.image, second.image)

    def test_reports_real_page_geometry(self):
        page = render_pdf_pages(read("text.pdf"), pages=[1])[0]
        assert page.width_pt == pytest.approx(612.0)
        assert page.height_pt == pytest.approx(792.0)

    def test_raster_size_follows_dpi(self):
        low = render_pdf_pages(read("text.pdf"), pages=[1], dpi=72)[0]
        high = render_pdf_pages(read("text.pdf"), pages=[1], dpi=144)[0]
        assert high.image.shape[0] == pytest.approx(low.image.shape[0] * 2, rel=0.02)
        # The page is the same size in points regardless of how it is rendered.
        assert low.width_pt == high.width_pt

    def test_images_are_three_channel_rgb(self):
        page = render_pdf_pages(read("text.pdf"), pages=[1], dpi=DEFAULT_DPI)[0]
        assert page.image.ndim == 3 and page.image.shape[2] == 3
        assert page.image.dtype == np.uint8

    def test_out_of_range_pages_are_skipped_not_fatal(self):
        # The router works from a classification that may disagree with the
        # document; losing the real pages over one bad number would be wrong.
        pages = render_pdf_pages(read("text.pdf"), pages=[1, 99])
        assert [p.number for p in pages] == [1]
        assert render_pdf_pages(read("text.pdf"), pages=[99]) == []

    def test_duplicate_page_numbers_are_collapsed(self):
        assert len(render_pdf_pages(read("text.pdf"), pages=[1, 1, 1])) == 1

    def test_non_pdf_is_rejected(self):
        with pytest.raises(RasterError, match="not a PDF"):
            render_pdf_pages(b"PK\x03\x04 this is a zip")

    def test_corrupt_pdf_raises(self):
        with pytest.raises(RasterError):
            render_pdf_pages(read("corrupt.pdf"))


class TestWrapImage:
    def make_png(self) -> bytes:
        import io

        from PIL import Image

        buf = io.BytesIO()
        Image.new("RGB", (80, 40), "white").save(buf, format="PNG")
        return buf.getvalue()

    def test_image_becomes_one_page_at_one_point_per_pixel(self):
        pages = wrap_image(self.make_png())
        assert len(pages) == 1
        page = pages[0]
        assert page.number == 1
        assert (page.width_pt, page.height_pt) == (80.0, 40.0)
        assert page.scale == 1.0

    def test_undecodable_bytes_raise(self):
        with pytest.raises(RasterError):
            wrap_image(b"\x00\x01\x02\x03")
