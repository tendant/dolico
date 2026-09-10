"""Which engine is in the Tier 3 slot, and what both engines owe the tier.

`vision` is the only module that knows the answer, and getting it wrong is not
a visible failure -- a deployment that thinks it is running GLM-OCR and is
running MinerU would attribute one engine's output to the other in provenance
and in the page cache. So the selection is tested rather than assumed.
"""

import importlib

import pytest

from dolico_ocr import vision as vision_mod
from dolico_ocr import vision_glm, vision_mineru
from dolico_ocr.vision_base import VisionAdapter, VisionError, server_url


def reload_with(monkeypatch, **env):
    for key, value in env.items():
        if value is None:
            monkeypatch.delenv(key, raising=False)
        else:
            monkeypatch.setenv(key, value)
    return importlib.reload(vision_mod)


@pytest.fixture(autouse=True)
def restore_module_state():
    # `vision` reads the environment at import, so a test that reloads it has
    # to put the process back the way it found it.
    yield
    importlib.reload(vision_mod)


class TestSelection:
    def test_mineru_is_the_default(self, monkeypatch):
        mod = reload_with(monkeypatch, DOLICO_VISION_ENGINE=None)
        assert mod.ENGINE_NAME == "mineru"
        assert isinstance(mod.new_engine(), vision_mineru.MineruEngine)

    def test_glm_is_selectable(self, monkeypatch):
        mod = reload_with(monkeypatch, DOLICO_VISION_ENGINE="glm-ocr")
        assert mod.ENGINE_NAME == "glm-ocr"
        assert isinstance(mod.new_engine(), vision_glm.GlmEngine)

    def test_the_name_is_case_and_whitespace_forgiving(self, monkeypatch):
        assert reload_with(monkeypatch, DOLICO_VISION_ENGINE="  GLM-OCR ").ENGINE_NAME == "glm-ocr"

    def test_an_unknown_engine_refuses_to_start(self, monkeypatch):
        # Falling back silently would mean provenance and cache keys naming an
        # engine that never ran. Not starting is the better failure.
        with pytest.raises(VisionError, match="is not an engine"):
            reload_with(monkeypatch, DOLICO_VISION_ENGINE="tesseract")

    def test_the_error_lists_what_would_have_worked(self, monkeypatch):
        with pytest.raises(VisionError, match="glm-ocr, mineru"):
            reload_with(monkeypatch, DOLICO_VISION_ENGINE="nope")


class TestBothEnginesHonorTheContract:
    @pytest.mark.parametrize("engine", [vision_mineru.MineruEngine, vision_glm.GlmEngine])
    def test_an_engine_satisfies_the_adapter_protocol(self, engine):
        assert isinstance(engine(), VisionAdapter)

    @pytest.mark.parametrize("engine", [vision_mineru.MineruEngine, vision_glm.GlmEngine])
    def test_an_engine_starts_unloaded_with_an_unknown_version(self, engine):
        # The version is part of the page cache key and is adopted from the
        # first real answer -- neither model is loaded at startup, so claiming
        # a version before one has run would be a guess in a cache key.
        built = engine()
        assert built.loaded is False
        assert built.version == "unknown"

    @pytest.mark.parametrize("engine", [vision_mineru.MineruEngine, vision_glm.GlmEngine])
    def test_an_engine_names_itself_in_describe(self, engine):
        described = engine().describe()
        assert described["engine"] in vision_mod.ENGINES
        assert set(described) >= {"engine", "backend", "server_url"}


class TestSharedEndpoint:
    """Both engines want the same knob: run the model somewhere else."""

    def test_the_new_name_wins(self, monkeypatch):
        monkeypatch.setenv("DOLICO_VISION_URL", "http://new:8080")
        monkeypatch.setenv("DOLICO_MINERU_URL", "http://old:8080")
        assert server_url() == "http://new:8080"

    def test_the_old_name_still_works(self, monkeypatch):
        # Renaming a variable is not a reason to break a running deployment.
        monkeypatch.delenv("DOLICO_VISION_URL", raising=False)
        monkeypatch.setenv("DOLICO_MINERU_URL", "http://old:8080")
        assert server_url() == "http://old:8080"

    def test_unset_is_none_rather_than_empty(self, monkeypatch):
        monkeypatch.delenv("DOLICO_VISION_URL", raising=False)
        monkeypatch.delenv("DOLICO_MINERU_URL", raising=False)
        assert server_url() is None

    def test_mineru_still_switches_to_its_http_backend(self, monkeypatch):
        monkeypatch.setenv("DOLICO_VISION_URL", "http://mineru:8000")
        assert vision_mineru.MineruEngine().backend == "hybrid-http-client"


class TestServingTierSelection:
    """Which tier serves, including in an image with no Paddle at all."""

    def _app(self, monkeypatch, tier, paddle, vision_ok):
        from dolico_ocr import app as app_module

        monkeypatch.setattr(app_module, "TIER", tier)
        monkeypatch.setattr(app_module, "_paddle_available", lambda: paddle)
        monkeypatch.setattr(app_module.vision_mod, "available", lambda: vision_ok)
        return app_module

    def test_vision_is_taken_when_asked_for(self, monkeypatch):
        assert self._app(monkeypatch, "vision", True, True)._serve_vision() is True

    def test_auto_falls_to_vision_when_there_is_no_paddle(self, monkeypatch):
        # A cloud-only image has no OCR tier. Refusing to start would be a
        # worse answer than serving the engine that is actually installed.
        assert self._app(monkeypatch, "auto", False, True)._serve_vision() is True

    def test_auto_prefers_paddle_when_it_is_there(self, monkeypatch):
        assert self._app(monkeypatch, "auto", True, True)._serve_vision() is False

    def test_auto_does_not_invent_a_tier_when_nothing_is_installed(self, monkeypatch):
        # Neither available: _serve_vision stays false and startup fails on the
        # OCR engine, which is the honest error rather than a silent stub.
        assert self._app(monkeypatch, "auto", False, False)._serve_vision() is False

    def test_an_explicit_ocr_tier_is_never_overridden(self, monkeypatch):
        for tier in ("text", "layout"):
            assert self._app(monkeypatch, tier, False, True)._serve_vision() is False
