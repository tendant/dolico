"""Tier 3, GLM-OCR: raw regions -> VisionBlocks -> canonical blocks.

The model is not exercised here and neither is the network. What is under test
is everything this adapter is actually responsible for: undoing the Markdown
GLM-OCR writes into its JSON, taking heading depth from the layout label rather
than from that Markdown, mapping the finer PP-DocLayout vocabulary, and
refusing to reach for the cloud API its library defaults to.

Fixtures are shaped like real `json_result`: a list of pages, each a list of
regions with `label`, `native_label`, `content` and a `bbox_2d` already
normalized to 0-1000 with a top-left origin.
"""

import pytest

from dolico_ocr.canonical import vision_page_payload
from dolico_ocr.vision_base import VisionError
from dolico_ocr.vision_glm import ENGINE_NAME, GlmEngine, _blocks, _mapped_label, _plain

TABLE_HTML = (
    "<table><tr><td>Region</td><td>Units</td></tr>"
    "<tr><td>North</td><td>120</td></tr></table>"
)

# Letter, in points.
W, H = 612.0, 792.0


def region(label, native=None, content="some text", bbox=(100, 100, 500, 140), index=0):
    return {
        "index": index,
        "label": label,
        "native_label": native or label,
        "content": content,
        "bbox_2d": list(bbox) if bbox is not None else None,
    }


def build(regions, model="glm-ocr"):
    """Regions all the way through to a canonical page, as the service does."""
    return vision_page_payload(1, _blocks([regions]), W, H, ENGINE_NAME, "0.1.0", model)


class TestMarkdownIsUndone:
    """GLM-OCR's `content` is decorated for rendering. Canonical is not."""

    def test_a_doc_title_loses_its_hash_and_becomes_a_level_one_heading(self):
        out = build([region("text", "doc_title", "# Quarterly Report")])
        block = out["blocks"][0]
        assert block["type"] == "heading"
        assert block["level"] == 1
        assert block["text"] == "Quarterly Report"

    def test_a_paragraph_title_becomes_level_two(self):
        out = build([region("text", "paragraph_title", "## Revenue")])
        block = out["blocks"][0]
        assert block["type"] == "heading"
        assert block["level"] == 2
        assert block["text"] == "Revenue"

    def test_the_level_comes_from_the_label_not_from_counting_hashes(self):
        # A doc_title is level 1 even when the formatter wrote four hashes,
        # because the layout model is what detected the title -- reading the
        # depth back out of the syntax would make canonical structure depend
        # on a formatter setting.
        out = build([region("text", "doc_title", "#### Quarterly Report")])
        assert out["blocks"][0]["level"] == 1

    def test_a_formula_loses_its_dollar_fences(self):
        out = build([region("formula", "display_formula", "$$\nE = mc^2\n$$")])
        block = out["blocks"][0]
        assert block["type"] == "formula"
        assert block["text"] == "E = mc^2"

    def test_a_formula_that_was_not_fenced_is_left_alone(self):
        assert _plain("E = mc^2", "formula", "display_formula") == ("E = mc^2", None)

    def test_table_html_is_not_touched(self):
        text, level = _plain(TABLE_HTML, "table", "table")
        assert text == TABLE_HTML
        assert level is None

    def test_ordinary_text_keeps_its_hashes(self):
        # A `#` that is part of the document is not formatter decoration.
        out = build([region("text", "text", "Issue #42 was resolved")])
        assert out["blocks"][0]["text"] == "Issue #42 was resolved"


class TestLabelMapping:
    @pytest.mark.parametrize(
        "native,expected",
        [
            ("text", "paragraph"),
            ("abstract", "paragraph"),
            ("footer", "paragraph"),
            ("header", "paragraph"),
            ("footnote", "paragraph"),
            ("aside_text", "paragraph"),
            ("number", "paragraph"),
            ("reference_content", "paragraph"),
            ("figure_title", "paragraph"),
            ("vertical_text", "paragraph"),
            ("seal", "paragraph"),
            ("algorithm", "code"),
            ("table", "table"),
            ("display_formula", "formula"),
            ("inline_formula", "formula"),
            # An unmapped upstream label keeps its text rather than vanishing.
            ("something_new", "paragraph"),
        ],
    )
    def test_native_labels_map_to_block_types(self, native, expected):
        mapped = {"table": "table", "display_formula": "formula",
                  "inline_formula": "formula"}.get(native, "text")
        content = TABLE_HTML if native == "table" else "some text"
        out = build([region(mapped, native, content)])
        assert out["blocks"][0]["type"] == expected

    def test_the_native_label_rides_in_provenance_not_the_mapped_one(self):
        # `text` would be true and useless. `footnote` is what the layout model
        # actually detected, and it is the finer of the two.
        out = build([region("text", "footnote", "1. See appendix B")])
        prov = out["blocks"][0]["provenance"]
        assert prov["engine"] == "glm-ocr"
        assert prov["method"] == "glm-ocr/glm-ocr:footnote"

    def test_the_served_model_is_recorded_as_the_backend(self):
        # For an engine reached over HTTP this is the only record of which
        # model answered.
        out = build([region("text")], model="mlx-community/GLM-OCR-bf16")
        method = out["blocks"][0]["provenance"]["method"]
        assert method == "glm-ocr/mlx-community/GLM-OCR-bf16:text"


class TestGeometry:
    def test_bbox_converts_from_0_1000_top_left_to_points_bottom_left(self):
        out = build([region("text", "text", "x", bbox=(0, 0, 500, 500))])
        bbox = out["blocks"][0]["bbox"]
        assert bbox["x"] == pytest.approx(0.0)
        assert bbox["width"] == pytest.approx(W / 2)
        assert bbox["height"] == pytest.approx(H / 2)
        assert bbox["y"] == pytest.approx(H / 2)

    def test_a_reversed_box_is_normalized_rather_than_dropped(self):
        out = build([region("text", "text", "x", bbox=(500, 500, 100, 100))])
        bbox = out["blocks"][0]["bbox"]
        assert bbox["width"] > 0 and bbox["height"] > 0

    def test_a_region_with_no_bbox_is_dropped(self):
        # The OCR-only path emits `bbox_2d: None`. Without geometry there is
        # nothing to place the block at, and this pipeline invents none.
        assert _blocks([[region("text", "text", "x", bbox=None)]]) == []

    def test_a_malformed_bbox_is_dropped_rather_than_crashing(self):
        assert _blocks([[region("text", "text", "x", bbox=("a", "b", "c", "d"))]]) == []


class TestReadingOrder:
    def test_the_engines_order_is_trusted(self):
        # Unlike MinerU, GLM-OCR's formatter sorts by `index` and renumbers, so
        # the list order is the order it decided to read in. Re-sorting by
        # position would override a real answer with a guess.
        out = build([
            region("text", "text", "second", bbox=(100, 700, 500, 740), index=0),
            region("text", "text", "first", bbox=(100, 100, 500, 140), index=1),
        ])
        assert [b["text"] for b in out["blocks"]] == ["second", "first"]


class TestTables:
    def test_table_html_becomes_a_canonical_grid(self):
        out = build([region("table", "table", TABLE_HTML)])
        grid = out["blocks"][0]["table"]["grid"]
        assert grid[0][0]["blocks"][0]["text"] == "Region"
        assert grid[1][1]["blocks"][0]["text"] == "120"

    def test_a_one_column_table_is_flattened_here_too(self):
        # The rule was written for MinerU, but nothing about it was specific to
        # MinerU: a narrow column of text read as an Nx1 grid is structure the
        # document does not have, whichever model invented it.
        one_col = (
            "<table><tr><td>SHIPPING RECEIPT</td></tr>"
            "<tr><td>Consignment 8842-QX</td></tr></table>"
        )
        out = build([region("table", "table", one_col)])
        assert [b["type"] for b in out["blocks"]] == ["paragraph", "paragraph"]
        assert all("bbox" not in b for b in out["blocks"])


class TestRegionsThatAreNotBlocks:
    def test_an_empty_region_is_dropped(self):
        assert _blocks([[region("text", "text", "   ")]]) == []

    def test_a_figure_with_no_text_is_dropped(self):
        # Its cropped pixels have nowhere to go -- the vision path writes no
        # assets. The MinerU adapter drops them for the same reason.
        assert _blocks([[region("image", "image", "")]]) == []

    def test_a_non_dict_region_is_ignored(self):
        assert _blocks([["not a region", region("text")]]) != []

    def test_an_empty_result_is_an_empty_page(self):
        assert _blocks([]) == []
        assert _blocks([[]]) == []

    def test_a_flat_region_list_is_read_as_one_page(self):
        # One image in means one page out, so a list of regions rather than a
        # list of pages can only be that page.
        assert len(_blocks([region("text")])) == 1


class TestCloudIsNeverAccidental:
    """A page leaving this host is a decision, not a fallback."""

    def test_a_stray_zhipu_key_does_not_turn_it_on(self, monkeypatch):
        # This is the library's own trigger: ZHIPU_API_KEY in the environment
        # flips it to the cloud unless `mode` is passed explicitly. A key left
        # there for some other tool must not start uploading documents.
        monkeypatch.delenv("DOLICO_GLM_API_KEY", raising=False)
        monkeypatch.setenv("DOLICO_VISION_URL", "http://127.0.0.1:8080")
        monkeypatch.setenv("ZHIPU_API_KEY", "sk-should-not-matter")
        engine = GlmEngine()
        assert engine.cloud is False
        assert engine._config()["mode"] == "selfhosted"

    def test_an_unconfigured_engine_is_unavailable_rather_than_cloud(self, monkeypatch):
        for var in ("DOLICO_VISION_URL", "DOLICO_MINERU_URL", "DOLICO_GLM_API_KEY"):
            monkeypatch.delenv(var, raising=False)
        monkeypatch.setenv("ZHIPU_API_KEY", "sk-should-not-matter")
        from dolico_ocr import vision_glm

        assert vision_glm.available() is False

    def test_a_self_hosted_endpoint_wins_over_a_leftover_key(self, monkeypatch):
        # Someone who has stood up their own GLM-OCR gets their own GLM-OCR,
        # even with a key left from evaluating the cloud.
        monkeypatch.setenv("DOLICO_VISION_URL", "http://127.0.0.1:8080")
        monkeypatch.setenv("DOLICO_GLM_API_KEY", "sk-left-over")
        assert GlmEngine().cloud is False


class TestCloudMode:
    """What `DOLICO_GLM_API_KEY` actually configures."""

    @pytest.fixture(autouse=True)
    def cloud_env(self, monkeypatch):
        monkeypatch.delenv("DOLICO_VISION_URL", raising=False)
        monkeypatch.delenv("DOLICO_MINERU_URL", raising=False)
        monkeypatch.setenv("DOLICO_GLM_API_KEY", "sk-test")

    def test_the_key_selects_maas(self):
        config = GlmEngine()._config()
        assert config["mode"] == "maas"
        assert config["api_key"] == "sk-test"

    def test_no_endpoint_is_needed(self):
        # The layout stage runs on Zhipu's side too, so there is nothing local
        # to point anywhere.
        assert GlmEngine().cloud is True
        assert "ocr_api_host" not in GlmEngine()._config()

    def test_a_key_satisfies_the_configuration_half_of_availability(self):
        # `available()` is configuration AND import, and glmocr is deliberately
        # not installed for this suite -- so what is checkable here is that a
        # key clears the configuration half: load() now complains about the
        # missing package rather than about having nowhere to send the page.
        with pytest.raises(VisionError, match="not installed"):
            GlmEngine().load()

    def test_provenance_says_the_page_left_the_host(self):
        engine = GlmEngine()
        out = vision_page_payload(
            1, _blocks([[region("text", "footnote", "1. See appendix B")]]),
            W, H, ENGINE_NAME, "0.1.5", engine.backend,
        )
        # Not the model name: where it was read is the more important fact,
        # and it should not take reading deployment config to find out.
        assert out["blocks"][0]["provenance"]["method"] == "glm-ocr/maas:footnote"

    def test_healthz_names_where_pages_go(self):
        described = GlmEngine().describe()
        assert described["where"] == "cloud(open.bigmodel.cn)"
        assert described["backend"] == "maas"


class TestCloudLabels:
    """The cloud returns regions with no `native_label` at all."""

    @pytest.mark.parametrize(
        "raw,expected",
        [
            # Already the four-way label: the self-hosted path.
            ("table", "table"),
            ("formula", "formula"),
            ("text", "text"),
            # The detector's own vocabulary, which is what the cloud may send.
            ("display_formula", "formula"),
            ("inline_formula", "formula"),
            ("chart", "image"),
            ("doc_title", "text"),
            ("footer", "text"),
            ("something_new", "text"),
        ],
    )
    def test_either_vocabulary_maps_to_the_same_four(self, raw, expected):
        assert _mapped_label(raw, raw) == expected

    def test_a_cloud_table_is_still_parsed_as_a_grid(self):
        # The failure this guards: `label: "table"` with no native_label read
        # as prose would put a page of angle brackets in the canonical model.
        cloud = {"index": 0, "label": "table", "content": TABLE_HTML,
                 "bbox_2d": [100, 100, 500, 300]}
        out = vision_page_payload(1, _blocks([[cloud]]), W, H, ENGINE_NAME, "0.1.5", "maas")
        assert out["blocks"][0]["type"] == "table"

    def test_a_cloud_formula_loses_its_fences(self):
        cloud = {"index": 0, "label": "display_formula", "content": "$$\nE = mc^2\n$$",
                 "bbox_2d": [100, 100, 500, 140]}
        out = vision_page_payload(1, _blocks([[cloud]]), W, H, ENGINE_NAME, "0.1.5", "maas")
        assert out["blocks"][0]["text"] == "E = mc^2"


class TestConfiguration:
    """The library's defaults are not this pipeline's defaults."""

    def test_the_endpoint_is_split_into_host_and_port(self, monkeypatch):
        monkeypatch.setenv("DOLICO_VISION_URL", "http://ocr.internal:8123")
        config = GlmEngine()._config()
        assert config["ocr_api_host"] == "ocr.internal"
        assert config["ocr_api_port"] == 8123
        assert config["_dotted"]["pipeline.ocr_api.api_scheme"] == "http"

    @pytest.mark.parametrize(
        "url,port,scheme",
        [
            ("https://ocr.internal", 443, "https"),
            ("http://ocr.internal", 80, "http"),
            # The scheme is passed explicitly because glmocr would otherwise
            # infer it from the port, which is wrong for TLS on anything but 443.
            ("https://ocr.internal:8443", 8443, "https"),
        ],
    )
    def test_the_scheme_survives_a_nonstandard_port(self, monkeypatch, url, port, scheme):
        monkeypatch.setenv("DOLICO_VISION_URL", url)
        config = GlmEngine()._config()
        assert config["ocr_api_port"] == port
        assert config["_dotted"]["pipeline.ocr_api.api_scheme"] == scheme

    def test_bullet_rewriting_is_turned_off(self, monkeypatch):
        # It rewrites the document's own `·` into Markdown's `- `, which would
        # put rendering syntax in a canonical text field.
        monkeypatch.setenv("DOLICO_VISION_URL", "http://127.0.0.1:8080")
        dotted = GlmEngine()._config()["_dotted"]
        assert dotted["pipeline.result_formatter.enable_format_bullet_points"] is False

    def test_an_unusable_url_is_refused_before_the_model_is_built(self, monkeypatch):
        monkeypatch.setenv("DOLICO_VISION_URL", "not-a-url")
        with pytest.raises(VisionError, match="not a usable URL"):
            GlmEngine()._config()

    def test_the_ollama_api_mode_is_reachable(self, monkeypatch):
        monkeypatch.setenv("DOLICO_VISION_URL", "http://127.0.0.1:11434")
        monkeypatch.setenv("DOLICO_GLM_API_MODE", "ollama_generate")
        dotted = GlmEngine()._config()["_dotted"]
        assert dotted["pipeline.ocr_api.api_mode"] == "ollama_generate"


class TestEndpointPath:
    """No two backends serve the model at the same path."""

    def test_vllm_and_sglang_get_the_openai_path(self, monkeypatch):
        monkeypatch.setenv("DOLICO_VISION_URL", "http://ocr.internal:8000")
        monkeypatch.delenv("DOLICO_GLM_API_MODE", raising=False)
        dotted = GlmEngine()._config()["_dotted"]
        assert dotted["pipeline.ocr_api.api_path"] == "/v1/chat/completions"

    def test_ollama_gets_its_native_endpoint(self, monkeypatch):
        # Setting the mode without the path would post vision requests to
        # /v1/chat/completions, which is the 502 the Ollama guide warns about.
        monkeypatch.setenv("DOLICO_VISION_URL", "http://127.0.0.1:11434")
        monkeypatch.setenv("DOLICO_GLM_API_MODE", "ollama_generate")
        dotted = GlmEngine()._config()["_dotted"]
        assert dotted["pipeline.ocr_api.api_path"] == "/api/generate"

    def test_a_path_on_the_url_wins(self, monkeypatch):
        # mlx_vlm.server serves the OpenAI API without the /v1 prefix. Nothing
        # in the request says so, so the endpoint has to carry it.
        monkeypatch.setenv("DOLICO_VISION_URL", "http://127.0.0.1:8080/chat/completions")
        monkeypatch.delenv("DOLICO_GLM_API_MODE", raising=False)
        config = GlmEngine()._config()
        assert config["_dotted"]["pipeline.ocr_api.api_path"] == "/chat/completions"
        # The path must not have eaten the host or the port.
        assert config["ocr_api_host"] == "127.0.0.1"
        assert config["ocr_api_port"] == 8080

    @pytest.mark.parametrize("url", ["http://h:8000", "http://h:8000/"])
    def test_a_bare_host_is_not_a_path(self, monkeypatch, url):
        monkeypatch.setenv("DOLICO_VISION_URL", url)
        monkeypatch.delenv("DOLICO_GLM_API_MODE", raising=False)
        dotted = GlmEngine()._config()["_dotted"]
        assert dotted["pipeline.ocr_api.api_path"] == "/v1/chat/completions"


class TestAvailability:
    def test_unavailable_without_an_endpoint(self, monkeypatch):
        # Installed but unconfigured is not available: glmocr with no endpoint
        # would reach for the cloud API rather than fail.
        monkeypatch.delenv("DOLICO_VISION_URL", raising=False)
        monkeypatch.delenv("DOLICO_MINERU_URL", raising=False)
        from dolico_ocr import vision_glm

        assert vision_glm.available() is False

    def test_loading_without_an_endpoint_says_what_to_set(self, monkeypatch):
        monkeypatch.delenv("DOLICO_VISION_URL", raising=False)
        monkeypatch.delenv("DOLICO_MINERU_URL", raising=False)
        with pytest.raises(VisionError, match="DOLICO_VISION_URL"):
            GlmEngine().load()

    def test_reading_a_page_before_the_engine_loads_fails_loudly(self, monkeypatch):
        monkeypatch.delenv("DOLICO_VISION_URL", raising=False)
        monkeypatch.delenv("DOLICO_MINERU_URL", raising=False)
        with pytest.raises(VisionError):
            GlmEngine().read(b"%PDF-1.4", 1)
