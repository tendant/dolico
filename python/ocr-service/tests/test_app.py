"""The HTTP surface and the canonical envelope.

The OCR engine is replaced with a fake so these run without loading a model:
what is under test is the contract with the Go client, not PaddleOCR.
"""

import pathlib

import numpy as np
import pytest
from fastapi.testclient import TestClient

from dolico_ocr import SCHEMA_VERSION
from dolico_ocr import app as app_module
from dolico_ocr.canonical import bbox_from_paragraph, page_payload
from dolico_ocr.layout import Line, Paragraph
from dolico_ocr.raster import RasteredPage

TESTDATA = pathlib.Path(__file__).resolve().parents[3] / "testdata"


class FakeEngine:
    """Returns one line near the top-left of whatever it is given."""

    version = "fake-1.0"
    loaded = True

    def __init__(self):
        self.calls = []

    def describe(self):
        return {"lang": "en", "det_model": "fake", "rec_model": "fake"}

    def load(self):
        pass

    def read(self, image: np.ndarray):
        self.calls.append(image.shape)
        height = image.shape[0]
        return [
            Line(text="hello world", confidence=0.9, x0=100, y0=100, x1=400, y1=130),
            Line(text="second line", confidence=0.8, x0=100, y0=132, x1=400, y1=162),
            Line(text="far below", confidence=0.7, x0=100, y0=height - 200, x1=400, y1=height - 170),
        ]


@pytest.fixture
def client(monkeypatch):
    fake = FakeEngine()
    monkeypatch.setattr(app_module, "engine", fake)
    with TestClient(app_module.app) as c:
        c.fake = fake
        yield c


def pdf(name="scanned.pdf") -> bytes:
    return (TESTDATA / name).read_bytes()


def test_healthz_reports_readiness_and_models(client):
    body = client.get("/healthz").json()
    assert body["status"] == "ok"
    assert body["engine"] == "paddleocr"
    assert body["schema_version"] == SCHEMA_VERSION
    assert body["models"]["det_model"] == "fake"


def test_version_endpoint(client):
    body = client.get("/v1/version").json()
    assert body["schema_version"] == SCHEMA_VERSION
    assert body["engine_version"] == "fake-1.0"


def test_extract_returns_the_canonical_envelope(client):
    resp = client.post(
        "/v1/extract", files={"file": ("scanned.pdf", pdf(), "application/pdf")}, data={"pages": "1"}
    )
    assert resp.status_code == 200
    body = resp.json()

    assert body["schema_version"] == SCHEMA_VERSION
    assert body["engine"] == "paddleocr"
    assert body["metadata"]["page_count"] == 1
    assert isinstance(body["duration_ms"], int)

    page = body["pages"][0]
    assert page["number"] == 1
    assert page["kind"] == "paginated"
    assert page["classification"]["type"] == "scanned"
    # Rendering means the page size is genuinely known here, unlike on the
    # pdf-inspector path.
    assert page["width"] == pytest.approx(612.0)
    assert page["height"] == pytest.approx(792.0)


def test_blocks_carry_provenance_confidence_and_geometry(client):
    body = client.post(
        "/v1/extract", files={"file": ("scanned.pdf", pdf(), "application/pdf")}
    ).json()
    blocks = body["pages"][0]["blocks"]
    assert blocks, "expected at least one block"
    for block in blocks:
        assert block["type"] == "paragraph"
        assert block["provenance"]["engine"] == "paddleocr"
        assert block["provenance"]["method"] == "paddleocr/text-lines"
        assert 0.0 <= block["confidence"] <= 1.0
        bbox = block["bbox"]
        assert bbox["width"] > 0 and bbox["height"] > 0


def test_adjacent_lines_group_and_a_distant_one_does_not(client):
    body = client.post(
        "/v1/extract", files={"file": ("scanned.pdf", pdf(), "application/pdf")}
    ).json()
    blocks = body["pages"][0]["blocks"]
    assert len(blocks) == 2
    assert blocks[0]["text"] == "hello world second line"
    assert blocks[1]["text"] == "far below"


def test_page_filter_selects_pages(client):
    body = client.post(
        "/v1/extract",
        files={"file": ("mixed.pdf", pdf("mixed.pdf"), "application/pdf")},
        data={"pages": "2"},
    ).json()
    assert [p["number"] for p in body["pages"]] == [2]


def test_no_page_filter_means_every_page(client):
    body = client.post(
        "/v1/extract", files={"file": ("mixed.pdf", pdf("mixed.pdf"), "application/pdf")}
    ).json()
    assert [p["number"] for p in body["pages"]] == [1, 2]


def test_dpi_changes_the_raster_the_engine_sees(client):
    client.post(
        "/v1/extract",
        files={"file": ("scanned.pdf", pdf(), "application/pdf")},
        data={"pages": "1", "dpi": "72"},
    )
    low = client.fake.calls[-1]
    client.post(
        "/v1/extract",
        files={"file": ("scanned.pdf", pdf(), "application/pdf")},
        data={"pages": "1", "dpi": "144"},
    )
    high = client.fake.calls[-1]
    assert high[0] > low[0]


def test_empty_upload_is_rejected(client):
    resp = client.post("/v1/extract", files={"file": ("empty.pdf", b"", "application/pdf")})
    assert resp.status_code == 400
    assert resp.json()["kind"] == "malformed"


def test_unreadable_bytes_are_malformed(client):
    resp = client.post("/v1/extract", files={"file": ("x.bin", b"\x00\x01\x02", "application/octet-stream")})
    assert resp.status_code == 422
    assert resp.json()["kind"] == "malformed"


def test_corrupt_pdf_is_malformed(client):
    resp = client.post(
        "/v1/extract", files={"file": ("corrupt.pdf", pdf("corrupt.pdf"), "application/pdf")}
    )
    assert resp.status_code == 422
    assert resp.json()["kind"] == "malformed"


def test_page_outside_the_document_is_reported(client):
    resp = client.post(
        "/v1/extract",
        files={"file": ("scanned.pdf", pdf(), "application/pdf")},
        data={"pages": "99"},
    )
    assert resp.status_code == 422


def test_bad_page_list_is_a_bad_request(client):
    for bad in ("abc", "0", "-3"):
        resp = client.post(
            "/v1/extract",
            files={"file": ("scanned.pdf", pdf(), "application/pdf")},
            data={"pages": bad},
        )
        assert resp.status_code == 400, bad


def test_error_envelope_matches_the_shim(client):
    # The Go client classifies failures the same way regardless of which engine
    # produced them, so the envelope has to be identical.
    body = client.post("/v1/extract", files={"file": ("e.pdf", b"", "application/pdf")}).json()
    assert set(body) == {"schema_version", "kind", "message"}


class TestBBoxConversion:
    def page(self):
        return RasteredPage(
            number=1,
            image=np.zeros((792, 612, 3), dtype=np.uint8),
            width_pt=612.0,
            height_pt=792.0,
            scale=1.0,
        )

    def test_bbox_is_flipped_into_pdf_space(self):
        # A paragraph 100px from the top of a 792pt page sits at y=792-130=662.
        paragraph = Paragraph([Line("x", 0.9, 100, 100, 400, 130)])
        bbox = bbox_from_paragraph(paragraph, self.page())
        assert bbox["x"] == pytest.approx(100.0)
        assert bbox["y"] == pytest.approx(662.0)
        assert bbox["width"] == pytest.approx(300.0)
        assert bbox["height"] == pytest.approx(30.0)

    def test_degenerate_box_is_omitted(self):
        # Same rule as the rest of the pipeline: no box beats a zero-area one.
        paragraph = Paragraph([Line("x", 0.9, 100, 100, 100, 100)])
        assert bbox_from_paragraph(paragraph, self.page()) is None

    def test_a_page_with_no_text_scores_zero_and_says_why(self):
        payload = page_payload(self.page(), [], "fake-1.0")
        assert payload["blocks"] == []
        assert payload["classification"]["confidence"] == 0.0
        assert "no_text_found" in payload["classification"]["reasons"]
