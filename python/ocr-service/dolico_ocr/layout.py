"""Group OCR text lines into paragraph blocks.

PaddleOCR detects *lines*, not paragraphs. Emitting one block per line would be
faithful to the engine and useless to a reader: a page of prose would arrive as
forty disconnected fragments that render as forty Markdown paragraphs.

So lines are grouped by geometry -- vertical proximity and horizontal overlap --
which is the same signal a human uses to see a paragraph. What this module
deliberately does *not* do is infer headings from font size. That is layout
analysis, it belongs to PP-StructureV3 in the next tier, and guessing at it here
would put invented structure into the canonical model where a consumer could not
tell it from the real thing.

All geometry in this module is in raster pixels, top-down, matching what
PaddleOCR returns. The conversion to PDF points happens once, at the boundary in
`canonical.py`.
"""

from __future__ import annotations

from dataclasses import dataclass
from statistics import median


@dataclass(frozen=True)
class Line:
    """One text line as PaddleOCR found it, in raster pixels, top-down."""

    text: str
    confidence: float
    x0: float
    y0: float
    x1: float
    y1: float

    @property
    def height(self) -> float:
        return self.y1 - self.y0

    @property
    def width(self) -> float:
        return self.x1 - self.x0


@dataclass
class Paragraph:
    """A run of lines that belong together."""

    lines: list[Line]

    @property
    def text(self) -> str:
        # Joined with spaces, not newlines: these are the wrapped lines of one
        # paragraph, and the line breaks are an artifact of the page width.
        return " ".join(line.text.strip() for line in self.lines if line.text.strip())

    @property
    def confidence(self) -> float:
        """Length-weighted mean of the line confidences.

        Weighting by length keeps a mis-read two-character line from dragging
        down a paragraph that is otherwise clean, while still letting a bad
        long line count for what it is.
        """
        total = sum(len(line.text) for line in self.lines)
        if total == 0:
            return 0.0
        return sum(line.confidence * len(line.text) for line in self.lines) / total

    def bbox(self) -> tuple[float, float, float, float]:
        return (
            min(line.x0 for line in self.lines),
            min(line.y0 for line in self.lines),
            max(line.x1 for line in self.lines),
            max(line.y1 for line in self.lines),
        )


# A gap larger than this many line-heights starts a new paragraph. Single
# spacing puts consecutive lines at a gap near zero; a paragraph break is
# typically half a line or more.
GAP_FACTOR = 0.6

# Two lines must overlap horizontally by at least this fraction of the narrower
# one to be in the same paragraph. This is what keeps two columns apart: they
# are vertically adjacent but share no horizontal extent.
MIN_OVERLAP = 0.15


def group_lines(
    lines: list[Line], gap_factor: float = GAP_FACTOR, min_overlap: float = MIN_OVERLAP
) -> list[Paragraph]:
    """Group lines into paragraphs by vertical gap and horizontal overlap.

    Each line is matched against every paragraph built so far, not merely the
    one before it. That distinction is what makes multi-column pages work: read
    in top-down order the lines of two columns interleave, so a purely
    sequential grouper sees every line as starting a new paragraph and returns
    one fragment per line. Considering all open paragraphs lets the second line
    of the left column find the first line of the left column, past the
    right-column line that sits between them.
    """
    if not lines:
        return []

    ordered = sorted(lines, key=lambda ln: (round(ln.y0), ln.x0))
    heights = [ln.height for ln in ordered if ln.height > 0]
    typical = median(heights) if heights else 1.0

    paragraphs: list[Paragraph] = []
    for line in ordered:
        best: Paragraph | None = None
        best_gap = float("inf")

        for paragraph in paragraphs:
            previous = paragraph.lines[-1]
            gap = line.y0 - previous.y1

            # Too far below to continue this paragraph.
            if gap > gap_factor * typical:
                continue
            # Substantially *above* the paragraph's last line: this belongs to
            # something else that happens to share the column.
            if gap < -typical:
                continue
            if horizontal_overlap(previous, line) < min_overlap:
                continue
            # Among the candidates, the closest one wins.
            if gap < best_gap:
                best, best_gap = paragraph, gap

        if best is not None:
            best.lines.append(line)
        else:
            paragraphs.append(Paragraph([line]))

    # Creation order roughly follows reading order already, but sorting by the
    # paragraph's own top-left makes it exact -- and makes the output stable.
    paragraphs.sort(key=lambda p: (round(p.bbox()[1]), p.bbox()[0]))
    return paragraphs


def horizontal_overlap(a: Line, b: Line) -> float:
    """Overlap of two lines as a fraction of the narrower one, 0..1."""
    overlap = min(a.x1, b.x1) - max(a.x0, b.x0)
    if overlap <= 0:
        return 0.0
    narrower = min(a.width, b.width)
    if narrower <= 0:
        return 0.0
    return overlap / narrower


def lines_from_prediction(result: dict) -> list[Line]:
    """Convert one PaddleOCR prediction into lines.

    PaddleOCR 3.x returns parallel lists under `rec_texts`, `rec_scores` and
    `rec_polys`, where each poly is a four-point quad in raster pixels. The
    quad may be rotated for skewed text, so the axis-aligned extent is taken
    from its corners rather than assuming the first and third points are
    opposite.
    """
    texts = result.get("rec_texts") or []
    scores = result.get("rec_scores") or []
    polys = result.get("rec_polys")
    if polys is None:
        polys = result.get("dt_polys") or []

    lines: list[Line] = []
    for index, text in enumerate(texts):
        if not text or not text.strip():
            continue
        score = float(scores[index]) if index < len(scores) else 0.0
        if index >= len(polys):
            continue
        xs = [float(point[0]) for point in polys[index]]
        ys = [float(point[1]) for point in polys[index]]
        lines.append(
            Line(
                text=text,
                confidence=max(0.0, min(1.0, score)),
                x0=min(xs),
                y0=min(ys),
                x1=max(xs),
                y1=max(ys),
            )
        )
    return lines
