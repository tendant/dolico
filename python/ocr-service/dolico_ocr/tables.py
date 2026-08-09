"""Parse recognized table HTML into the canonical grid.

PP-StructureV3 reports a recognized table as an HTML fragment. The canonical
model wants a grid of slots where a merged cell is one origin carrying its
spans plus shadow slots pointing back at it, so that `grid[r][c]` is always
addressable. This module does that conversion.

The HTML comes from a model, not from a document, so it is treated as
untrusted-ish: only the table structure is read, everything else is discarded,
and no markup survives into the canonical model.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from html.parser import HTMLParser


@dataclass
class Cell:
    text: str
    row_span: int = 1
    col_span: int = 1
    header: bool = False


@dataclass
class Row:
    cells: list[Cell] = field(default_factory=list)
    in_head: bool = False


class _TableParser(HTMLParser):
    """Collects rows and cells; ignores everything else."""

    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.rows: list[Row] = []
        self._row: Row | None = None
        self._cell: Cell | None = None
        self._in_head = False
        self._depth = 0

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if tag == "thead":
            self._in_head = True
        elif tag == "tr":
            self._row = Row(in_head=self._in_head)
        elif tag in ("td", "th"):
            if self._row is None:
                # A cell outside any row: start one rather than dropping it.
                self._row = Row(in_head=self._in_head)
            self._cell = Cell(
                text="",
                row_span=_span(attrs.get("rowspan")),
                col_span=_span(attrs.get("colspan")),
                header=(tag == "th" or self._in_head),
            )
        elif self._cell is not None:
            # Markup inside a cell (<b>, <br>, ...) contributes no structure.
            self._depth += 1
            if tag == "br":
                self._cell.text += " "

    def handle_endtag(self, tag):
        if tag == "thead":
            self._in_head = False
        elif tag == "tr":
            if self._row is not None:
                self.rows.append(self._row)
                self._row = None
        elif tag in ("td", "th"):
            if self._cell is not None and self._row is not None:
                self._cell.text = " ".join(self._cell.text.split())
                self._row.cells.append(self._cell)
            self._cell = None
        elif self._depth > 0:
            self._depth -= 1

    def handle_data(self, data):
        if self._cell is not None:
            self._cell.text += data

    def close(self):
        super().close()
        # An unterminated <tr> still holds cells worth keeping.
        if self._row is not None and self._row.cells:
            self.rows.append(self._row)
            self._row = None


def _span(raw: str | None) -> int:
    try:
        return max(1, int(raw))
    except (TypeError, ValueError):
        return 1


def parse_table_html(html: str) -> tuple[list[list[dict]], int]:
    """Return (grid, header_rows) in the canonical shape.

    Each grid entry is either an origin cell -- `{"text", "row_span",
    "col_span"}` -- or a shadow `{"covered_by": {"row", "col"}}` pointing at the
    origin that spans into it.
    """
    parser = _TableParser()
    parser.feed(html or "")
    parser.close()
    if not parser.rows:
        return [], 0

    # Lay cells out, skipping positions already claimed by an earlier span.
    grid: list[list[dict | None]] = []
    for _ in parser.rows:
        grid.append([])

    def ensure(r: int, c: int) -> None:
        while len(grid) <= r:
            grid.append([])
        while len(grid[r]) <= c:
            grid[r].append(None)

    for r, row in enumerate(parser.rows):
        col = 0
        for cell in row.cells:
            # Advance past shadow slots an earlier row's rowspan already took.
            while col < len(grid[r]) and grid[r][col] is not None:
                col += 1
            ensure(r, col)
            grid[r][col] = {
                "text": cell.text,
                "row_span": cell.row_span,
                "col_span": cell.col_span,
            }
            for dr in range(cell.row_span):
                for dc in range(cell.col_span):
                    if dr == 0 and dc == 0:
                        continue
                    ensure(r + dr, col + dc)
                    grid[r + dr][col + dc] = {"covered_by": {"row": r, "col": col}}
            col += cell.col_span

    width = max((len(row) for row in grid), default=0)
    normalized: list[list[dict]] = []
    for row in grid:
        padded = list(row) + [None] * (width - len(row))
        # A gap left by a ragged source becomes an empty origin cell, so that
        # every position in the grid is addressable.
        normalized.append(
            [slot if slot is not None else {"text": "", "row_span": 1, "col_span": 1} for slot in padded]
        )

    # Only what the HTML actually says. A first row that looks like a header but
    # is marked up as <td> is not reported as one: guessing would put invented
    # structure into the canonical model.
    header_rows = 0
    for row in parser.rows:
        if row.in_head or (row.cells and all(c.header for c in row.cells)):
            header_rows += 1
        else:
            break

    return normalized, header_rows


def table_text(grid: list[list[dict]]) -> str:
    """Flatten a grid to plain text, for quality scoring and search."""
    lines = []
    for row in grid:
        values = [slot.get("text", "") for slot in row if "covered_by" not in slot]
        if any(values):
            lines.append(" ".join(v for v in values if v))
    return "\n".join(lines)
