"""Tier 3: MinerU blocks -> canonical blocks.

MinerU itself is not exercised here — loading it costs gigabytes and seconds.
What is under test is everything around it: the label mapping, the two
coordinate conversions, table conversion, and reading order.

The fixtures are shaped like real MinerU output: boxes normalized to 0–1000
with a top-left origin, tables as HTML in `table_body`.
"""

import pytest

from dolico_ocr.canonical import vision_page_payload
from dolico_ocr.vision import ENGINE_NAME, VisionBlock, _remote_backend, VisionError

TABLE_HTML = (
    "<table><tr><td>Region</td><td>Units</td></tr>"
    "<tr><td>North</td><td>120</td></tr></table>"
)

# Letter, in points.
W, H = 612.0, 792.0


def block(label, text="some text", x0=100, y0=100, x1=500, y1=140, level=None):
    return VisionBlock(label=label, text=text, x0=x0, y0=y0, x1=x1, y1=y1, text_level=level)


def build(blocks, backend="hybrid-engine"):
    return vision_page_payload(1, blocks, W, H, "2.5.0", backend)


class TestLabelMapping:
    @pytest.mark.parametrize(
        "label,expected",
        [
            ("text", "paragraph"),
            ("header", "paragraph"),
            ("footer", "paragraph"),
            ("table", "table"),
            ("list", "list"),
            ("equation", "formula"),
            ("interline_equation", "formula"),
            ("image", "image"),
            ("figure", "image"),
            ("code", "code"),
            # An unmapped upstream label keeps its text rather than vanishing.
            ("something_new", "paragraph"),
        ],
    )
    def test_labels_map_to_block_types(self, label, expected):
        text = TABLE_HTML if label == "table" else "some text"
        out = build([block(label, text)])
        assert out["blocks"][0]["type"] == expected

    def test_the_original_label_and_backend_ride_in_provenance(self):
        out = build([block("footer", "page 3")])
        method = out["blocks"][0]["provenance"]["method"]
        # The backend is recorded because it is the difference between a real
        # third tier and a second run of Tier 2's model family.
        assert method == "mineru/hybrid-engine:footer"
        assert out["blocks"][0]["provenance"]["engine"] == ENGINE_NAME

    def test_text_level_promotes_a_text_block_to_a_heading(self):
        out = build([block("text", "Chapter One", level=1)])
        assert out["blocks"][0]["type"] == "heading"
        assert out["blocks"][0]["level"] == 1

    def test_heading_level_is_clamped(self):
        out = build([block("text", "Deep", level=99)])
        assert out["blocks"][0]["level"] == 6


class TestGeometry:
    def test_bbox_converts_from_0_1000_top_left_to_points_bottom_left(self):
        # A box spanning the top-left quarter of the page.
        out = build([block("text", "x", x0=0, y0=0, x1=500, y1=500)])
        bbox = out["blocks"][0]["bbox"]
        assert bbox["x"] == pytest.approx(0.0)
        assert bbox["width"] == pytest.approx(W / 2)
        assert bbox["height"] == pytest.approx(H / 2)
        # Top-left in MinerU's frame is the *upper* half in PDF space, so the
        # box's bottom edge sits at half the page height.
        assert bbox["y"] == pytest.approx(H / 2)

    def test_the_vertical_flip_is_monotonic(self):
        higher = build([block("text", "x", y0=100, y1=200)])["blocks"][0]["bbox"]
        lower = build([block("text", "x", y0=700, y1=800)])["blocks"][0]["bbox"]
        assert higher["y"] > lower["y"], "a block further down the page must sit lower in PDF space"

    def test_degenerate_box_is_omitted(self):
        out = build([block("text", "x", x0=100, y0=100, x1=100, y1=100)])
        assert "bbox" not in out["blocks"][0]

    def test_page_reports_the_size_mineru_saw(self):
        out = build([block("text")])
        assert out["width"] == pytest.approx(W)
        assert out["height"] == pytest.approx(H)


class TestTables:
    def test_table_html_becomes_a_canonical_grid(self):
        out = build([block("table", TABLE_HTML)])
        grid = out["blocks"][0]["table"]["grid"]
        assert len(grid) == 2
        assert grid[0][0]["blocks"][0]["text"] == "Region"
        assert grid[1][1]["blocks"][0]["text"] == "120"

    def test_cells_carry_ids_and_provenance(self):
        out = build([block("table", TABLE_HTML)])
        cell = out["blocks"][0]["table"]["grid"][0][0]["blocks"][0]
        assert cell["id"] == "p1-vis0-r0c0"
        assert cell["provenance"]["engine"] == ENGINE_NAME

    def test_an_unparseable_table_is_dropped_rather_than_emitted_empty(self):
        out = build([block("table", "<p>not a table</p>")])
        assert out["blocks"] == []


class TestPage:
    def test_pages_are_marked_as_vision_read(self):
        out = build([block("text")])
        assert out["classification"]["reasons"] == ["ocr", "vision"]
        assert out["classification"]["type"] == "scanned"

    def test_an_empty_page_says_so(self):
        out = build([])
        assert out["blocks"] == []
        assert out["classification"]["confidence"] == 0.0
        assert "no_text_found" in out["classification"]["reasons"]

    def test_no_per_block_confidence_is_invented(self):
        # MinerU reports none, so none is fabricated.
        out = build([block("text")])
        assert "confidence" not in out["blocks"][0]

    def test_whitespace_only_blocks_are_dropped(self):
        out = build([block("text", "   \n ")])
        assert out["blocks"] == []

    def test_block_ids_are_unique_and_positional(self):
        out = build([block("text", "a"), block("text", "b"), block("table", TABLE_HTML)])
        assert [b["id"] for b in out["blocks"]] == ["p1-vis0", "p1-vis1", "p1-vis2"]


class TestRemoteBackend:
    """A remote MinerU keeps its own ~8GB out of the OCR service's process."""

    @pytest.mark.parametrize(
        "local,remote",
        [
            ("hybrid-engine", "hybrid-http-client"),
            ("vlm-engine", "vlm-http-client"),
            ("hybrid-http-client", "hybrid-http-client"),
        ],
    )
    def test_local_backends_map_to_their_http_clients(self, local, remote):
        assert _remote_backend(local) == remote

    def test_pipeline_has_no_remote_form_and_says_so(self):
        # Failing here beats failing deep inside MinerU's parse.
        with pytest.raises(VisionError, match="cannot run against a remote"):
            _remote_backend("pipeline")
