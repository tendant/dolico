"""Table HTML -> canonical grid.

The HTML comes from a model, so it is often ragged, occasionally malformed, and
never to be trusted for anything but structure.
"""

from dolico_ocr.tables import parse_table_html, table_text

SIMPLE = """
<html><body><table><tbody>
<tr><td>Region</td><td>Units</td><td>Revenue</td></tr>
<tr><td>North</td><td>120</td><td>14,400.00</td></tr>
</tbody></table></body></html>
"""


def texts(grid):
    return [[slot.get("text", "^" if "covered_by" in slot else "") for slot in row] for row in grid]


def test_simple_grid():
    grid, header_rows = parse_table_html(SIMPLE)
    assert texts(grid) == [
        ["Region", "Units", "Revenue"],
        ["North", "120", "14,400.00"],
    ]
    # PP-Structure emits <td> throughout, so nothing claims to be a header.
    assert header_rows == 0


def test_thead_is_reported_as_a_header():
    html = "<table><thead><tr><td>A</td><td>B</td></tr></thead><tbody><tr><td>1</td><td>2</td></tr></tbody></table>"
    _, header_rows = parse_table_html(html)
    assert header_rows == 1


def test_th_row_is_reported_as_a_header():
    html = "<table><tr><th>A</th><th>B</th></tr><tr><td>1</td><td>2</td></tr></table>"
    _, header_rows = parse_table_html(html)
    assert header_rows == 1


def test_a_header_looking_row_of_td_is_not_promoted():
    # Guessing would put invented structure into the canonical model; the
    # Markdown view makes its own rendering choice separately.
    _, header_rows = parse_table_html(SIMPLE)
    assert header_rows == 0


def test_colspan_creates_covered_slots():
    html = "<table><tr><td colspan='2'>wide</td></tr><tr><td>a</td><td>b</td></tr></table>"
    grid, _ = parse_table_html(html)
    assert grid[0][0]["text"] == "wide"
    assert grid[0][0]["col_span"] == 2
    assert grid[0][1] == {"covered_by": {"row": 0, "col": 0}}
    assert texts(grid)[1] == ["a", "b"]


def test_rowspan_creates_covered_slots_in_later_rows():
    html = "<table><tr><td rowspan='2'>tall</td><td>x</td></tr><tr><td>y</td></tr></table>"
    grid, _ = parse_table_html(html)
    assert grid[0][0]["text"] == "tall"
    assert grid[0][0]["row_span"] == 2
    assert grid[1][0] == {"covered_by": {"row": 0, "col": 0}}
    # The next cell in row 2 must land in column 1, not column 0.
    assert grid[1][1]["text"] == "y"


def test_combined_spans():
    html = "<table><tr><td colspan='2' rowspan='2'>big</td><td>x</td></tr><tr><td>y</td></tr></table>"
    grid, _ = parse_table_html(html)
    assert grid[0][0]["col_span"] == 2 and grid[0][0]["row_span"] == 2
    for r, c in ((0, 1), (1, 0), (1, 1)):
        assert grid[r][c] == {"covered_by": {"row": 0, "col": 0}}, (r, c)


def test_every_position_is_addressable_in_a_ragged_table():
    html = "<table><tr><td>a</td><td>b</td><td>c</td></tr><tr><td>d</td></tr></table>"
    grid, _ = parse_table_html(html)
    assert all(len(row) == 3 for row in grid)
    assert texts(grid)[1] == ["d", "", ""]


def test_markup_inside_cells_is_stripped():
    html = "<table><tr><td><b>bold</b> and <i>italic</i></td></tr></table>"
    grid, _ = parse_table_html(html)
    assert grid[0][0]["text"] == "bold and italic"


def test_br_becomes_a_space():
    html = "<table><tr><td>one<br/>two</td></tr></table>"
    grid, _ = parse_table_html(html)
    assert grid[0][0]["text"] == "one two"


def test_entities_are_decoded():
    html = "<table><tr><td>a &amp; b &lt;c&gt;</td></tr></table>"
    grid, _ = parse_table_html(html)
    assert grid[0][0]["text"] == "a & b <c>"


def test_whitespace_is_collapsed():
    html = "<table><tr><td>  lots   of\n  space  </td></tr></table>"
    grid, _ = parse_table_html(html)
    assert grid[0][0]["text"] == "lots of space"


def test_unterminated_row_still_yields_its_cells():
    grid, _ = parse_table_html("<table><tr><td>a</td><td>b</td></table>")
    assert texts(grid) == [["a", "b"]]


def test_bad_span_values_fall_back_to_one():
    html = "<table><tr><td colspan='abc'>a</td><td rowspan='-2'>b</td></tr></table>"
    grid, _ = parse_table_html(html)
    assert grid[0][0]["col_span"] == 1
    assert grid[0][1]["row_span"] == 1


def test_empty_and_garbage_input():
    assert parse_table_html("") == ([], 0)
    assert parse_table_html("<p>not a table</p>") == ([], 0)
    assert parse_table_html("<table></table>") == ([], 0)


def test_table_text_flattens_and_skips_shadows():
    grid, _ = parse_table_html(
        "<table><tr><td colspan='2'>wide</td></tr><tr><td>a</td><td>b</td></tr></table>"
    )
    assert table_text(grid) == "wide\na b"
