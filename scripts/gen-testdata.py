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

  text.pdf     every page has real text operators  -> native extraction
  scanned.pdf  image only, no text operators       -> must route to OCR
  mixed.pdf    one of each                         -> must route per page
  corrupt.pdf  a PDF header over garbage           -> must fail cleanly
"""

import io
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
    text_page(
        c,
        "Quarterly Report",
        [
            "Revenue grew by twelve percent over the prior quarter.",
            "Operating costs were flat.",
            "The outlook for the coming quarter remains positive.",
        ],
    )
    c.showPage()
    text_page(
        c,
        "Appendix A",
        [
            "All figures are unaudited.",
            "Currency is United States dollars.",
        ],
    )
    c.showPage()
    c.save()


def write_scanned_pdf() -> None:
    c = canvas.Canvas(str(OUT / "scanned.pdf"), pagesize=LETTER)
    image_page(
        c,
        page_image(
            "SCANNED INVOICE\n\nInvoice number 4471\nAmount due 1,250.00\nDue on receipt"
        ),
    )
    c.showPage()
    c.save()


def write_mixed_pdf() -> None:
    c = canvas.Canvas(str(OUT / "mixed.pdf"), pagesize=LETTER)
    text_page(
        c,
        "Cover Letter",
        [
            "Please find the signed agreement attached.",
            "The scanned copy follows on the next page.",
        ],
    )
    c.showPage()
    image_page(c, page_image("SIGNED AGREEMENT\n\nParty A and Party B agree\nSignature on file"))
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
    for row, cells in enumerate(
        [
            ("Format", "Engine", "Needs OCR"),
            ("DOCX", "anydoc", "no"),
            ("Scanned PDF", "paddleocr", "yes"),
        ]
    ):
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


def main() -> None:
    OUT.mkdir(parents=True, exist_ok=True)
    write_text_pdf()
    write_scanned_pdf()
    write_mixed_pdf()
    write_corrupt_pdf()
    write_docx()
    write_xlsx()
    write_pptx()
    for path in sorted(OUT.glob("*")):
        if path.is_file():
            print(f"{path.name:16} {path.stat().st_size:>8} bytes")


if __name__ == "__main__":
    main()
