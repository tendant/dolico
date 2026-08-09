"""Dolico OCR tier.

A small HTTP service that turns the pages of a document nobody could extract
natively into canonical blocks. It is the third engine in the pipeline, behind
`anydoc` and `pdf-inspector`, and it only ever sees the pages the router
decided need it.

Two responsibilities the rest of the system deliberately does not have:

  * **Rasterization.** Neither `pdf-inspector` nor `anydoc` renders a PDF, and
    keeping pdfium out of the Go binary and the Rust shim was a deliberate
    choice. This service renders with `pypdfium2` from the original bytes plus
    the page numbers it was asked for.
  * **Real confidences.** PaddleOCR reports a score per text line, which is the
    first genuine per-block confidence in the pipeline; native extraction has
    nothing meaningful to report.
"""

__version__ = "0.1.0"

SCHEMA_VERSION = "1.0"
ENGINE_NAME = "paddleocr"
