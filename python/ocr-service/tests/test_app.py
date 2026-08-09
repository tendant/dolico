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
    """A client pinned to Tier 1 with a fake engine.

    The tier is forced because the service now prefers layout analysis
    whenever its dependencies are installed; without this the fake would be
    bypassed and every test here would load real models.
    """
    fake = FakeEngine()
    monkeypatch.setattr(app_module, "engine", fake)
    monkeypatch.setattr(app_module, "TIER", "text")
    with TestClient(app_module.app) as c:
        c.fake = fake
        yield c


def pdf(name="scanned.pdf") -> bytes:
    return (TESTDATA / name).read_bytes()


def test_healthz_reports_readiness_and_models(client):
    body = client.get("/healthz").json()
    assert body["status"] == "ok"
    assert body["engine"] == "paddleocr"
    assert body["tier"] == "text"
    assert body["schema_version"] == SCHEMA_VERSION
    assert body["models"]["det_model"] == "fake"


def test_version_endpoint(client):
    body = client.get("/v1/version").json()
    assert body["schema_version"] == SCHEMA_VERSION
    assert body["engine_version"] == "fake-1.0"
    assert body["engine"] == "paddleocr"
    assert body["tier"] == "text"


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


class FakeStructureEngine:
    """Returns a heading, a table and a paragraph, like a real page."""

    version = "fake-structure-1.0"
    loaded = True

    def __init__(self):
        self.calls = []

    def describe(self):
        return {"lang": "en", "det_model": "fake", "rec_model": "fake"}

    def load(self):
        pass

    def read(self, image):
        from dolico_ocr.structure import LayoutBlock

        self.calls.append(image.shape)
        blocks = [
            LayoutBlock("doc_title", "QUARTERLY SALES", 100, 100, 500, 130, None, 0.99),
            LayoutBlock(
                "table",
                "<table><tr><td>Region</td><td>Units</td></tr>"
                "<tr><td>North</td><td>120</td></tr></table>",
                100, 200, 500, 400, None, 0.98,
            ),
            LayoutBlock("text", "Totals exclude tax.", 100, 450, 400, 480, None, 0.97),
        ]
        lines = [
            Line(text="QUARTERLY SALES", confidence=0.95, x0=110, y0=105, x1=490, y1=125),
            Line(text="Totals exclude tax.", confidence=0.90, x0=110, y0=455, x1=390, y1=475),
        ]
        return blocks, lines


@pytest.fixture
def layout_client(monkeypatch):
    fake = FakeStructureEngine()
    monkeypatch.setattr(app_module, "structure", fake)
    monkeypatch.setattr(app_module, "TIER", "layout")
    with TestClient(app_module.app) as c:
        c.fake = fake
        yield c


class TestLayoutTier:
    def test_healthz_reports_the_layout_tier(self, layout_client):
        body = layout_client.get("/healthz").json()
        assert body["tier"] == "layout"
        assert body["engine"] == "pp-structurev3"

    def test_the_envelope_names_the_layout_engine(self, layout_client):
        body = layout_client.post(
            "/v1/extract", files={"file": ("scanned.pdf", pdf(), "application/pdf")}
        ).json()
        assert body["engine"] == "pp-structurev3"
        assert body["engine_version"] == "fake-structure-1.0"

    def test_structure_is_recovered_not_flattened(self, layout_client):
        body = layout_client.post(
            "/v1/extract", files={"file": ("scanned.pdf", pdf(), "application/pdf")}
        ).json()
        blocks = body["pages"][0]["blocks"]
        assert [b["type"] for b in blocks] == ["heading", "table", "paragraph"]

        table = blocks[1]["table"]
        assert table["grid"][0][0]["blocks"][0]["text"] == "Region"
        assert table["grid"][1][1]["blocks"][0]["text"] == "120"

    def test_layout_blocks_carry_confidence_and_geometry(self, layout_client):
        body = layout_client.post(
            "/v1/extract", files={"file": ("scanned.pdf", pdf(), "application/pdf")}
        ).json()
        for block in body["pages"][0]["blocks"]:
            assert 0.0 <= block["confidence"] <= 1.0
            assert block["bbox"]["width"] > 0
            assert block["provenance"]["engine"] == "pp-structurev3"
            assert block["provenance"]["method"].startswith("pp-structurev3/layout:")

    def test_the_page_is_marked_as_layout_analyzed(self, layout_client):
        body = layout_client.post(
            "/v1/extract", files={"file": ("scanned.pdf", pdf(), "application/pdf")}
        ).json()
        assert "layout_analysis" in body["pages"][0]["classification"]["reasons"]


class FakeVisionEngine:
    """Stands in for MinerU. Returns one heading and one table per page."""

    version = "2.5.0-fake"
    backend = "hybrid-engine"
    loaded = True

    def __init__(self):
        self.calls = []
        self.fail_pages = set()

    def describe(self):
        return {"backend": self.backend, "effort": "medium", "server_url": ""}

    def load(self):
        pass

    def read(self, pdf_bytes, page_number):
        from dolico_ocr.vision import VisionBlock, VisionError

        self.calls.append(page_number)
        if page_number in self.fail_pages:
            raise VisionError(f"page {page_number} exploded")
        blocks = [
            VisionBlock("text", "QUARTERLY SALES", 100, 100, 500, 130, 1),
            VisionBlock(
                "table",
                "<table><tr><td>Region</td><td>Units</td></tr>"
                "<tr><td>North</td><td>120</td></tr></table>",
                100, 200, 800, 400, None,
            ),
        ]
        return blocks, 612.0, 792.0


@pytest.fixture
def vision_client(monkeypatch):
    fake = FakeVisionEngine()
    monkeypatch.setattr(app_module, "vision", fake)
    monkeypatch.setattr(app_module.vision_mod, "available", lambda: True)
    monkeypatch.setattr(app_module, "TIER", "text")
    monkeypatch.setattr(app_module, "engine", FakeEngine())
    with TestClient(app_module.app) as c:
        c.fake = fake
        yield c


class TestVisionTier:
    def post(self, client, pages="1", name="scanned.pdf", data=None):
        return client.post(
            "/v1/extract",
            files={"file": (name, data if data is not None else pdf(name), "application/pdf")},
            data={"pages": pages, "tier": "vision"},
        )

    def test_the_envelope_names_the_vision_engine(self, vision_client):
        body = self.post(vision_client).json()
        assert body["engine"] == "mineru"
        assert body["engine_version"] == "2.5.0-fake"

    def test_blocks_are_mapped_with_geometry_and_provenance(self, vision_client):
        page = self.post(vision_client).json()["pages"][0]
        assert [b["type"] for b in page["blocks"]] == ["heading", "table"]
        assert page["width"] == pytest.approx(612.0)
        for block in page["blocks"]:
            assert block["provenance"]["engine"] == "mineru"
            assert block["provenance"]["method"].startswith("mineru/hybrid-engine:")
            # Unlike a hosted reasoning model, MinerU measures geometry.
            assert block["bbox"]["width"] > 0

    def test_only_the_named_pages_are_read(self, vision_client):
        body = self.post(vision_client, pages="2,5", name="mixed.pdf").json()
        assert vision_client.fake.calls == [2, 5]
        assert [p["number"] for p in body["pages"]] == [2, 5]

    def test_a_whole_document_request_is_refused(self, vision_client):
        # Vision runs on pages the cheaper tiers already lost; a document-wide
        # request is a caller mistake, not something to expensively honor.
        resp = self.post(vision_client, pages="")
        assert resp.status_code == 400
        assert vision_client.fake.calls == []

    def test_one_failed_page_does_not_lose_the_others(self, vision_client):
        vision_client.fake.fail_pages = {1}
        body = self.post(vision_client, pages="1,2", name="mixed.pdf").json()
        assert [p["number"] for p in body["pages"]] == [2]

    def test_every_page_failing_is_an_error(self, vision_client):
        vision_client.fake.fail_pages = {1}
        resp = self.post(vision_client, pages="1")
        assert resp.status_code == 422

    def test_non_pdf_input_is_rejected(self, vision_client):
        resp = self.post(vision_client, name="x.png", data=b"\x89PNG\r\n\x1a\n")
        assert resp.status_code == 415

    def test_unavailable_when_mineru_is_not_installed(self, vision_client, monkeypatch):
        monkeypatch.setattr(app_module.vision_mod, "available", lambda: False)
        resp = self.post(vision_client)
        assert resp.status_code == 503
        assert "uv sync --extra vision" in resp.json()["message"]

    def test_availability_is_advertised_separately_from_the_serving_tier(self, vision_client):
        health = vision_client.get("/healthz").json()
        # Vision is reached per page by the router, never as the service tier.
        assert health["tier"] == "text"
        assert health["vision_available"] is True
        assert health["vision"]["backend"] == "hybrid-engine"

        version = vision_client.get("/v1/version").json()
        assert version["vision_available"] is True
        assert version["vision_engine"] == "mineru"


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
