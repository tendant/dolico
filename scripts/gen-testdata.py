#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["reportlab", "python-docx", "openpyxl", "python-pptx", "pillow"]
# ///
"""Generate the binary test fixtures in testdata/.

The fixtures are committed, so this script exists to make them reproducible and
to document what each one is *for*. Run it only when a fixture needs to change:

    ./scripts/gen-testdata.py

Text fixtures (.md, .txt, .csv) are hand-written and committed directly; this
script does not touch them.

The PDFs are the interesting ones, because they are what exercise routing:

  text.pdf           every page has real text operators  -> native extraction
  scanned.pdf        image only, no text operators       -> must route to OCR
  mixed.pdf          one of each                         -> must route per page
  scanned-table.pdf  a ruled table drawn as pixels       -> needs layout analysis
  corrupt.pdf        a PDF header over garbage           -> must fail cleanly

scanned-table.pdf is the one that separates the OCR tiers: text-line OCR reads
the cells but returns them as flat paragraphs, and only layout analysis
recovers the grid.
"""

import io
import json
import pathlib

from docx import Document as Docx
from docx.shared import Pt
from openpyxl import Workbook
from PIL import Image, ImageDraw
from pptx import Presentation
from pptx.util import Inches
from reportlab.lib.pagesizes import LETTER
from reportlab.lib.utils import ImageReader
from reportlab.pdfgen import canvas

OUT = pathlib.Path(__file__).resolve().parent.parent / "testdata"

# ---------------------------------------------------------------------------
# Fixture content
#
# Declared once and used twice: to draw the fixture, and to write the ground
# truth the benchmark scores against. Because these documents are generated
# rather than collected, the expected output is known exactly rather than
# transcribed by hand -- which is what makes a character error rate meaningful
# on them at all.
# ---------------------------------------------------------------------------

TEXT_PDF_PAGES = [
    (
        "Quarterly Report",
        [
            "Revenue grew by twelve percent over the prior quarter.",
            "Operating costs were flat.",
            "The outlook for the coming quarter remains positive.",
        ],
    ),
    (
        "Appendix A",
        [
            "All figures are unaudited.",
            "Currency is United States dollars.",
        ],
    ),
]

SCANNED_LINES = [
    "SCANNED INVOICE",
    "",
    "Invoice number 4471",
    "Amount due 1,250.00",
    "Due on receipt",
]

MIXED_TEXT_PAGE = (
    "Cover Letter",
    [
        "Please find the signed agreement attached.",
        "The scanned copy follows on the next page.",
    ],
)

MIXED_SCANNED_LINES = [
    "SIGNED AGREEMENT",
    "",
    "Party A and Party B agree",
    "Signature on file",
]

def flatten(rows) -> list[str]:
    """Table cells in reading order, for the text expectation."""
    return [cell for row in rows for cell in row]


# sample.csv is hand-written and committed; these are its contents, restated so
# the ground truth can be generated from one place.
CSV_ROWS = [
    ("region", "units", "revenue"),
    ("North", "120", "14400.00"),
    ("South", "86", "10320.00"),
    ("East", "203", "24360.00"),
    ("West", "54", "6480.00"),
]

DOCX_ROUTING_TABLE = [
    ("Format", "Engine", "Needs OCR"),
    ("DOCX", "anydoc", "no"),
    ("Scanned PDF", "paddleocr", "yes"),
]

# A merged cell: the second column of row 0 is a shadow slot, which the
# benchmark skips, so that row has one cell where the other has two.
DOCX_MERGED_TABLE = [
    ("Spans two columns",),
    ("left", "right"),
]

TABLE_TITLE = "QUARTERLY SALES"
TABLE_NOTE = "All figures are unaudited."
TABLE_FOOTER = "Totals exclude tax."
TABLE_ROWS = [
    ("Region", "Units", "Revenue"),
    ("North", "120", "14,400.00"),
    ("South", "86", "10,320.00"),
    ("East", "203", "24,360.00"),
    ("West", "54", "6,480.00"),
]


def page_image(text: str, size=(1200, 1600)) -> Image.Image:
    """A white page with black text, drawn as pixels.

    This is what makes scanned.pdf a genuine OCR case: the words are visible to
    a human and to an OCR engine, and completely absent from the PDF's content
    stream.
    """
    img = Image.new("RGB", size, "white")
    draw = ImageDraw.Draw(img)
    y = 120
    for line in text.splitlines():
        draw.text((120, y), line, fill="black")
        y += 60
    return img


def text_page(c: canvas.Canvas, title: str, body: list[str]) -> None:
    c.setFont("Helvetica-Bold", 18)
    c.drawString(72, 720, title)
    c.setFont("Helvetica", 11)
    y = 690
    for line in body:
        c.drawString(72, y, line)
        y -= 16


def image_page(c: canvas.Canvas, img: Image.Image) -> None:
    buf = io.BytesIO()
    img.save(buf, format="PNG")
    buf.seek(0)
    c.drawImage(ImageReader(buf), 0, 0, width=LETTER[0], height=LETTER[1])


def write_text_pdf() -> None:
    c = canvas.Canvas(str(OUT / "text.pdf"), pagesize=LETTER)
    for title, body in TEXT_PDF_PAGES:
        text_page(c, title, body)
        c.showPage()
    c.save()


def write_scanned_pdf() -> None:
    c = canvas.Canvas(str(OUT / "scanned.pdf"), pagesize=LETTER)
    image_page(c, page_image("\n".join(SCANNED_LINES)))
    c.showPage()
    c.save()


def write_mixed_pdf() -> None:
    c = canvas.Canvas(str(OUT / "mixed.pdf"), pagesize=LETTER)
    text_page(c, MIXED_TEXT_PAGE[0], MIXED_TEXT_PAGE[1])
    c.showPage()
    image_page(c, page_image("\n".join(MIXED_SCANNED_LINES)))
    c.showPage()
    c.save()


def table_image(size=(1700, 2200)) -> Image.Image:
    """A ruled table drawn as pixels, with no text operators anywhere.

    Deliberately *wired* -- every cell fully bordered -- because that is the
    case a table-structure model should get right, and because a human reading
    the rendered page can see immediately whether the recovered grid matches.
    """
    img = Image.new("RGB", size, "white")
    draw = ImageDraw.Draw(img)

    rows = TABLE_ROWS
    left, top = 200, 400
    col_w, row_h = 380, 120

    draw.text((left, top - 160), TABLE_TITLE, fill="black")
    draw.text((left, top - 100), TABLE_NOTE, fill="black")

    for r in range(len(rows) + 1):
        y = top + r * row_h
        draw.line([(left, y), (left + col_w * 3, y)], fill="black", width=3)
    for c in range(4):
        x = left + c * col_w
        draw.line([(x, top), (x, top + row_h * len(rows))], fill="black", width=3)

    for r, cells in enumerate(rows):
        for c, value in enumerate(cells):
            draw.text((left + c * col_w + 30, top + r * row_h + 45), value, fill="black")

    draw.text((left, top + row_h * len(rows) + 80), TABLE_FOOTER, fill="black")
    return img


def write_scanned_table_pdf() -> None:
    c = canvas.Canvas(str(OUT / "scanned-table.pdf"), pagesize=LETTER)
    image_page(c, table_image())
    c.showPage()
    c.save()


def write_corrupt_pdf() -> None:
    # A valid header over bytes that are not a PDF body: the shim must report
    # this as malformed rather than panicking or returning an empty document.
    (OUT / "corrupt.pdf").write_bytes(b"%PDF-1.7\n" + b"\x00\xff not a pdf body \x01" * 40)


def write_docx() -> None:
    d = Docx()
    d.add_heading("Engineering Handbook", level=1)
    d.add_paragraph("This document exercises headings, lists, tables and styling.")

    d.add_heading("Principles", level=2)
    for item in ("Extract natively whenever possible.", "OCR only pages that require OCR."):
        d.add_paragraph(item, style="List Bullet")
    for item in ("Inspect the document.", "Route each page.", "Normalize the result."):
        d.add_paragraph(item, style="List Number")

    d.add_heading("Styling", level=2)
    p = d.add_paragraph("Text can be ")
    p.add_run("bold").bold = True
    p.add_run(", ")
    p.add_run("italic").italic = True
    p.add_run(", or plain.")

    d.add_heading("Routing table", level=2)
    t = d.add_table(rows=3, cols=3)
    t.style = "Table Grid"
    for row, cells in enumerate(DOCX_ROUTING_TABLE):
        for col, value in enumerate(cells):
            cell = t.cell(row, col)
            cell.text = value
            if row == 0:
                cell.paragraphs[0].runs[0].font.bold = True
                cell.paragraphs[0].runs[0].font.size = Pt(11)

    # A merged cell, so the canonical grid's covered_by slots get exercised.
    t2 = d.add_table(rows=2, cols=2)
    t2.style = "Table Grid"
    t2.cell(0, 0).merge(t2.cell(0, 1)).text = "Spans two columns"
    t2.cell(1, 0).text = "left"
    t2.cell(1, 1).text = "right"

    d.save(OUT / "sample.docx")


def write_xlsx() -> None:
    wb = Workbook()
    ws = wb.active
    ws.title = "Inventory"
    for row in [("item", "qty", "price"), ("widget", 3, 9.99), ("gadget", 12, 24.50)]:
        ws.append(row)

    ws2 = wb.create_sheet("Notes")
    ws2.append(("note",))
    ws2.append(("Prices exclude tax.",))
    wb.save(OUT / "sample.xlsx")


def write_pptx() -> None:
    prs = Presentation()
    title_layout, bullet_layout = prs.slide_layouts[0], prs.slide_layouts[1]

    s1 = prs.slides.add_slide(title_layout)
    s1.shapes.title.text = "Document Processing"
    s1.placeholders[1].text = "A per-page routing pipeline"

    s2 = prs.slides.add_slide(bullet_layout)
    s2.shapes.title.text = "Pipeline"
    body = s2.placeholders[1].text_frame
    body.text = "Inspect"
    for step in ("Route", "Extract", "Normalize"):
        body.add_paragraph().text = step

    s3 = prs.slides.add_slide(prs.slide_layouts[5])
    s3.shapes.title.text = "Embedded image"
    buf = io.BytesIO()
    page_image("chart placeholder", size=(600, 400)).save(buf, format="PNG")
    buf.seek(0)
    s3.shapes.add_picture(buf, Inches(1), Inches(2), width=Inches(4))

    prs.save(OUT / "sample.pptx")


def write_ground_truth() -> None:
    """Write what each fixture is supposed to say.

    `scripts/bench.py` scores extraction against this. Because these documents
    are generated rather than collected, the expectation is exact rather than
    transcribed, so a character error rate computed against it means something.

    What it does *not* mean is real-world accuracy: these are clean synthetic
    renderings, not photographs of creased paper. The format is deliberately
    corpus-agnostic so a directory of real documents with hand-written ground
    truth can be scored by the same harness.
    """
    truth = {
        "text.pdf": {
            "pages": [
                {"number": n, "text": [title, *body]}
                for n, (title, body) in enumerate(TEXT_PDF_PAGES, start=1)
            ]
        },
        "scanned.pdf": {
            "pages": [{"number": 1, "text": [ln for ln in SCANNED_LINES if ln]}]
        },
        "mixed.pdf": {
            "pages": [
                {"number": 1, "text": [MIXED_TEXT_PAGE[0], *MIXED_TEXT_PAGE[1]]},
                {"number": 2, "text": [ln for ln in MIXED_SCANNED_LINES if ln]},
            ]
        },
        "scanned-table.pdf": {
            "pages": [
                {
                    # In reading order, table contents included: the text score
                    # measures whether the characters were read, and an
                    # engine's inability to structure them should not also
                    # register as a failure to read them.
                    "number": 1,
                    "text": [TABLE_TITLE, TABLE_NOTE, *flatten(TABLE_ROWS), TABLE_FOOTER],
                    # Scored separately, because recovering a grid is a
                    # different capability that only the layout tier claims.
                    "tables": [[list(row) for row in TABLE_ROWS]],
                }
            ]
        },
        "sample.csv": {
            "pages": [
                {
                    "number": 1,
                    "text": flatten(CSV_ROWS),
                    "tables": [[list(row) for row in CSV_ROWS]],
                }
            ]
        },
        "sample.docx": {
            "pages": [
                {
                    "number": 1,
                    "text": [
                        "Engineering Handbook",
                        "This document exercises headings, lists, tables and styling.",
                        "Principles",
                        "Extract natively whenever possible.",
                        "OCR only pages that require OCR.",
                        "Inspect the document.",
                        "Route each page.",
                        "Normalize the result.",
                        "Styling",
                        "Text can be bold, italic, or plain.",
                        "Routing table",
                        *flatten(DOCX_ROUTING_TABLE),
                        *flatten(DOCX_MERGED_TABLE),
                    ],
                }
            ]
        },
        "sample.txt": {
            "pages": [
                {
                    "number": 1,
                    "text": [
                        "Plain text carries no markup, and nothing here should be interpreted as any.",
                        "Characters like # and * and | are literal in this file. A line that looks",
                        "like - this is not a list item.",
                        "Indented lines keep their indentation.",
                        "The last paragraph has no trailing newline issues.",
                    ],
                }
            ]
        },
    }
    # The DOCX tables are expected too: the native path recovers grids as much
    # as the layout tier does, and a regression there should show up here.
    truth["sample.docx"]["pages"][0]["tables"] = [
        [list(row) for row in DOCX_ROUTING_TABLE],
        [list(row) for row in DOCX_MERGED_TABLE],
    ]
    path = OUT / "ground-truth.json"
    path.write_text(json.dumps(truth, indent=2, ensure_ascii=False) + "\n")


def main() -> None:
    OUT.mkdir(parents=True, exist_ok=True)
    write_text_pdf()
    write_scanned_pdf()
    write_scanned_table_pdf()
    write_mixed_pdf()
    write_corrupt_pdf()
    write_docx()
    write_xlsx()
    write_pptx()
    write_ground_truth()
    for path in sorted(OUT.glob("*")):
        if path.is_file():
            print(f"{path.name:16} {path.stat().st_size:>8} bytes")


if __name__ == "__main__":
    main()
