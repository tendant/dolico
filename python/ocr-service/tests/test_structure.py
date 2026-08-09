"""Tier 2: layout blocks -> canonical blocks.

PP-StructureV3 itself is not exercised here -- loading it costs a dozen models
and several seconds. What is under test is everything around it: the label
mapping, the coordinate flip, per-block confidence, reading order, and the
table conversion.
"""

import numpy as np
import pytest

from dolico_ocr.canonical import layout_page_payload
from dolico_ocr.layout import Line
from dolico_ocr.raster import RasteredPage
from dolico_ocr.structure import ENGINE_NAME, LayoutBlock, _in_reading_order

TABLE_HTML = (
    "<html><body><table><tbody>"
    "<tr><td>Region</td><td>Units</td></tr>"
    "<tr><td>North</td><td>120</td></tr>"
    "</tbody></table></body></html>"
)


def page():
    return RasteredPage(
        number=1,
        image=np.zeros((792, 612, 3), dtype=np.uint8),
        width_pt=612.0,
        height_pt=792.0,
        scale=1.0,
    )


def block(label, content="text here", x0=100, y0=100, x1=400, y1=130, order=None, det=0.95):
    return LayoutBlock(
        label=label, content=content, x0=x0, y0=y0, x1=x1, y1=y1, order=order, det_score=det
    )


def build(blocks, lines=None):
    return layout_page_payload(page(), blocks, lines or [], "3.7.0", ENGINE_NAME)


class TestLabelMapping:
    @pytest.mark.parametrize(
        "label,expected",
        [
            ("text", "paragraph"),
            ("header", "paragraph"),
            ("footer", "paragraph"),
            ("doc_title", "heading"),
            ("title", "heading"),
            ("paragraph_title", "heading"),
            ("table_title", "heading"),
            ("figure", "image"),
            ("chart", "image"),
            ("formula", "formula"),
            # Anything unmapped stays readable rather than being dropped.
            ("something_new_upstream", "paragraph"),
        ],
    )
    def test_labels_map_to_block_types(self, label, expected):
        out = build([block(label)])
        assert out["blocks"][0]["type"] == expected

    def test_the_original_label_survives_in_provenance(self):
        out = build([block("table_title", "Table 1")])
        assert out["blocks"][0]["provenance"]["method"] == "pp-structurev3/layout:table_title"

    def test_document_titles_are_level_one_and_others_are_not(self):
        assert build([block("doc_title", "Report")])["blocks"][0]["level"] == 1
        assert build([block("table_title", "Table 1")])["blocks"][0]["level"] == 2


class TestTableBlocks:
    def test_table_html_becomes_a_grid(self):
        out = build([block("table", TABLE_HTML)])
        table = out["blocks"][0]["table"]
        assert len(table["grid"]) == 2
        assert table["grid"][0][0]["blocks"][0]["text"] == "Region"
        assert table["grid"][1][1]["blocks"][0]["text"] == "120"

    def test_table_cells_carry_ids_and_provenance(self):
        out = build([block("table", TABLE_HTML)])
        cell_block = out["blocks"][0]["table"]["grid"][0][0]["blocks"][0]
        assert cell_block["id"] == "p1-ly0-r0c0"
        assert cell_block["provenance"]["engine"] == ENGINE_NAME

    def test_an_unparseable_table_is_dropped_rather_than_emitted_empty(self):
        out = build([block("table", "<p>no table here</p>")])
        assert out["blocks"] == []

    def test_empty_cells_have_no_blocks(self):
        html = "<table><tr><td>a</td><td></td></tr></table>"
        grid = build([block("table", html)])["blocks"][0]["table"]["grid"]
        assert "blocks" not in grid[0][1]


class TestGeometry:
    def test_bbox_is_flipped_into_pdf_space(self):
        out = build([block("text", "x", x0=100, y0=100, x1=400, y1=130)])
        bbox = out["blocks"][0]["bbox"]
        assert bbox["x"] == pytest.approx(100.0)
        assert bbox["y"] == pytest.approx(792.0 - 130)
        assert bbox["width"] == pytest.approx(300.0)
        assert bbox["height"] == pytest.approx(30.0)

    def test_degenerate_regions_get_no_bbox(self):
        out = build([block("text", "x", x0=100, y0=100, x1=100, y1=100)])
        assert "bbox" not in out["blocks"][0]

    def test_the_page_reports_real_dimensions(self):
        out = build([block("text")])
        assert out["width"] == pytest.approx(612.0)
        assert out["height"] == pytest.approx(792.0)


class TestConfidence:
    def test_confidence_comes_from_the_ocr_lines_inside_the_region(self):
        lines = [
            Line(text="aaaa", confidence=1.0, x0=110, y0=105, x1=200, y1=125),
            Line(text="bbbb", confidence=0.5, x0=210, y0=105, x1=300, y1=125),
        ]
        out = build([block("text", "aaaa bbbb")], lines)
        # Equal lengths, so the mean of 1.0 and 0.5.
        assert out["blocks"][0]["confidence"] == pytest.approx(0.75)

    def test_lines_outside_the_region_are_ignored(self):
        lines = [
            Line(text="inside", confidence=1.0, x0=110, y0=105, x1=200, y1=125),
            Line(text="outside", confidence=0.0, x0=110, y0=600, x1=200, y1=620),
        ]
        out = build([block("text", "inside")], lines)
        assert out["blocks"][0]["confidence"] == pytest.approx(1.0)

    def test_a_figure_falls_back_to_the_layout_detection_score(self):
        # No text inside a figure, so the only signal is how sure the layout
        # model was that it is one.
        out = build([block("figure", "", det=0.87)])
        assert out["blocks"][0]["confidence"] == pytest.approx(0.87)

    def test_page_confidence_is_the_mean_of_its_blocks(self):
        lines = [Line(text="aaaa", confidence=0.8, x0=110, y0=105, x1=200, y1=125)]
        out = build([block("text", "aaaa")], lines)
        assert out["classification"]["confidence"] == pytest.approx(0.8)

    def test_a_page_with_nothing_on_it_says_so(self):
        out = build([])
        assert out["blocks"] == []
        assert out["classification"]["confidence"] == 0.0
        assert "no_text_found" in out["classification"]["reasons"]

    def test_layout_pages_are_marked_as_analyzed(self):
        out = build([block("text")])
        assert "layout_analysis" in out["classification"]["reasons"]


class TestReadingOrder:
    def test_order_index_is_used_when_every_region_has_one(self):
        blocks = [block("text", "second", order=2), block("text", "first", order=1)]
        assert [b.content for b in _in_reading_order(blocks)] == ["first", "second"]

    def test_the_models_own_order_stands_when_order_index_is_partial(self):
        # PP-StructureV3 numbers only some regions. Sorting on that anyway sent
        # every unnumbered one to the end, which put the page title below the
        # table it introduced.
        blocks = [
            block("header", "QUARTERLY SALES", order=None),
            block("text", "All figures are unaudited.", order=1),
            block("table", TABLE_HTML, order=None),
            block("text", "Totals exclude tax.", order=2),
        ]
        assert [b.content for b in _in_reading_order(blocks)] == [
            "QUARTERLY SALES",
            "All figures are unaudited.",
            TABLE_HTML,
            "Totals exclude tax.",
        ]

    def test_empty_input(self):
        assert _in_reading_order([]) == []


def test_whitespace_only_text_regions_are_dropped():
    out = build([block("text", "   \n  ")])
    assert out["blocks"] == []


def test_block_ids_are_unique_and_positional():
    out = build([block("text", "a"), block("text", "b"), block("table", TABLE_HTML)])
    assert [b["id"] for b in out["blocks"]] == ["p1-ly0", "p1-ly1", "p1-ly2"]
