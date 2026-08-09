"""Line grouping. Pure geometry, no model, so these run in milliseconds."""

from dolico_ocr.layout import Line, group_lines, horizontal_overlap, lines_from_prediction


def line(text, x0, y0, x1, y1, conf=0.99):
    return Line(text=text, confidence=conf, x0=x0, y0=y0, x1=x1, y1=y1)


def test_no_lines_no_paragraphs():
    assert group_lines([]) == []


def test_tightly_spaced_lines_become_one_paragraph():
    # Single-spaced body text: consecutive lines nearly touch.
    lines = [
        line("The quick brown fox", 100, 100, 400, 120),
        line("jumps over the lazy", 100, 122, 400, 142),
        line("dog and keeps going.", 100, 144, 400, 164),
    ]
    paragraphs = group_lines(lines)
    assert len(paragraphs) == 1
    assert paragraphs[0].text == "The quick brown fox jumps over the lazy dog and keeps going."


def test_a_wide_gap_starts_a_new_paragraph():
    lines = [
        line("First paragraph text", 100, 100, 400, 120),
        line("Second paragraph text", 100, 300, 400, 320),
    ]
    assert len(group_lines(lines)) == 2


def test_two_columns_stay_separate():
    # Vertically interleaved but horizontally disjoint: the classic case a
    # purely vertical rule gets wrong.
    lines = [
        line("left column one", 50, 100, 250, 120),
        line("right column one", 400, 100, 600, 120),
        line("left column two", 50, 122, 250, 142),
        line("right column two", 400, 122, 600, 142),
    ]
    paragraphs = group_lines(lines)
    texts = sorted(p.text for p in paragraphs)
    assert texts == ["left column one left column two", "right column one right column two"]


def test_slightly_overlapping_lines_from_a_skewed_scan_still_group():
    # A skewed scan can produce a negative vertical gap.
    lines = [
        line("first line of text", 100, 100, 400, 122),
        line("second line of text", 100, 120, 400, 142),
    ]
    assert len(group_lines(lines)) == 1


def test_reading_order_is_top_down_then_left_to_right():
    lines = [
        line("third", 100, 300, 200, 320),
        line("first", 100, 100, 200, 120),
        line("second", 300, 100, 400, 120),
    ]
    paragraphs = group_lines(lines)
    assert paragraphs[0].lines[0].text == "first"
    assert [p.lines[0].text for p in paragraphs][-1] == "third"


def test_confidence_is_length_weighted():
    # A short bad line should not drag down a long good one as much as an
    # unweighted mean would.
    lines = [
        line("a" * 100, 100, 100, 400, 120, conf=1.0),
        line("xy", 100, 122, 400, 142, conf=0.0),
    ]
    paragraph = group_lines(lines)[0]
    assert paragraph.confidence > 0.95
    unweighted = (1.0 + 0.0) / 2
    assert paragraph.confidence > unweighted


def test_paragraph_bbox_is_the_union():
    lines = [
        line("one", 100, 100, 300, 120),
        line("two", 80, 122, 250, 142),
    ]
    x0, y0, x1, y1 = group_lines(lines)[0].bbox()
    assert (x0, y0, x1, y1) == (80, 100, 300, 142)


def test_blank_text_is_dropped_from_the_paragraph_text():
    lines = [line("real", 100, 100, 300, 120), line("   ", 100, 122, 300, 142)]
    assert group_lines(lines)[0].text == "real"


def test_horizontal_overlap_is_a_fraction_of_the_narrower_line():
    wide = line("wide", 0, 0, 100, 10)
    narrow = line("narrow", 40, 0, 60, 10)
    assert horizontal_overlap(wide, narrow) == 1.0
    assert horizontal_overlap(narrow, wide) == 1.0
    disjoint = line("far", 200, 0, 300, 10)
    assert horizontal_overlap(wide, disjoint) == 0.0


class TestLinesFromPrediction:
    """PaddleOCR's output shape, including the parts that vary."""

    def test_parallel_lists_become_lines(self):
        result = {
            "rec_texts": ["hello", "world"],
            "rec_scores": [0.9, 0.8],
            "rec_polys": [
                [[10, 20], [50, 20], [50, 40], [10, 40]],
                [[10, 50], [60, 50], [60, 70], [10, 70]],
            ],
        }
        lines = lines_from_prediction(result)
        assert [ln.text for ln in lines] == ["hello", "world"]
        assert lines[0].x0 == 10 and lines[0].y1 == 40

    def test_rotated_quads_use_the_axis_aligned_extent(self):
        # A skewed line's corners are not axis-aligned; taking points 0 and 2
        # as opposite corners would understate the extent.
        result = {
            "rec_texts": ["skewed"],
            "rec_scores": [0.9],
            "rec_polys": [[[10, 25], [50, 20], [52, 38], [12, 43]]],
        }
        ln = lines_from_prediction(result)[0]
        assert (ln.x0, ln.y0, ln.x1, ln.y1) == (10, 20, 52, 43)

    def test_falls_back_to_dt_polys(self):
        result = {
            "rec_texts": ["x"],
            "rec_scores": [0.5],
            "dt_polys": [[[0, 0], [10, 0], [10, 10], [0, 10]]],
        }
        assert len(lines_from_prediction(result)) == 1

    def test_empty_and_whitespace_text_is_dropped(self):
        result = {
            "rec_texts": ["", "   ", "real"],
            "rec_scores": [0.9, 0.9, 0.9],
            "rec_polys": [[[0, 0]] * 4, [[0, 0]] * 4, [[0, 0], [10, 0], [10, 10], [0, 10]]],
        }
        assert [ln.text for ln in lines_from_prediction(result)] == ["real"]

    def test_confidence_is_clamped(self):
        result = {
            "rec_texts": ["a", "b"],
            "rec_scores": [1.5, -0.2],
            "rec_polys": [[[0, 0], [1, 0], [1, 1], [0, 1]]] * 2,
        }
        lines = lines_from_prediction(result)
        assert lines[0].confidence == 1.0
        assert lines[1].confidence == 0.0

    def test_missing_keys_do_not_raise(self):
        assert lines_from_prediction({}) == []
