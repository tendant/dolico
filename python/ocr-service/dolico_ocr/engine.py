"""PaddleOCR wrapper.

Isolates the one piece of the service that owns a model: loading it, keeping it
warm, and serializing access to it.
"""

from __future__ import annotations

import logging
import os
import threading

import numpy as np

from .layout import Line, lines_from_prediction

log = logging.getLogger(__name__)


# Default detection and recognition models.
#
# PaddleOCR would otherwise pick the `medium` variants, which on CPU take about
# 17 seconds for a letter page at 200 DPI. The `mobile` variants take about
# 2.5 seconds for the same page. On the repository's scanned fixture they
# return character-identical text at comparable confidence -- but that is one
# clean synthetic page, not a benchmark, and a corpus of real scans may well
# separate them. Both are overridable, and a deployment that cares more about
# accuracy than latency should measure its own documents and set
# DOLICO_OCR_DET_MODEL / DOLICO_OCR_REC_MODEL accordingly.
DEFAULT_DET_MODEL = "PP-OCRv5_mobile_det"
DEFAULT_REC_MODEL = "PP-OCRv5_mobile_rec"


class OCREngine:
    """A PaddleOCR instance behind a lock.

    PaddleOCR's predictor is not safe to call from several threads at once, so
    every prediction is serialized here. That makes this service
    single-inference-at-a-time by construction; concurrency comes from running
    more replicas, not more threads, which is also what a GPU deployment would
    want.
    """

    def __init__(self, lang: str = "en") -> None:
        self.lang = lang
        self.det_model = os.environ.get("DOLICO_OCR_DET_MODEL", DEFAULT_DET_MODEL)
        self.rec_model = os.environ.get("DOLICO_OCR_REC_MODEL", DEFAULT_REC_MODEL)
        self._lock = threading.Lock()
        self._ocr = None
        self._version = "unknown"

    def load(self) -> None:
        """Load the models. Called at startup so the first request is not the
        one that pays for a cold model and a possible download."""
        if self._ocr is not None:
            return
        from paddleocr import PaddleOCR

        try:
            import paddleocr

            self._version = getattr(paddleocr, "__version__", "unknown")
        except Exception:  # pragma: no cover - version is cosmetic
            pass

        log.info(
            "loading PaddleOCR (lang=%s det=%s rec=%s)",
            self.lang,
            self.det_model,
            self.rec_model,
        )
        # The document-level preprocessors are off on purpose. Orientation
        # classification and unwarping are a second and third model, and the
        # pages reaching this service have already been identified as scans of
        # ordinary documents by pdf-inspector. They can be enabled per
        # deployment when the corpus needs them.
        self._ocr = PaddleOCR(
            lang=self.lang,
            text_detection_model_name=self.det_model,
            text_recognition_model_name=self.rec_model,
            use_doc_orientation_classify=_flag("DOLICO_OCR_ORIENTATION", False),
            use_doc_unwarping=_flag("DOLICO_OCR_UNWARP", False),
            use_textline_orientation=_flag("DOLICO_OCR_TEXTLINE_ORIENTATION", False),
        )
        log.info("PaddleOCR ready (version=%s)", self._version)

    def describe(self) -> dict[str, str]:
        """Which models are actually in use, for the health endpoint. A
        deployment that is silently slow is usually one running a model nobody
        realized was the default."""
        return {"lang": self.lang, "det_model": self.det_model, "rec_model": self.rec_model}

    @property
    def version(self) -> str:
        return self._version

    @property
    def loaded(self) -> bool:
        return self._ocr is not None

    def read(self, image: np.ndarray) -> list[Line]:
        """OCR one page image, returning text lines in raster pixels."""
        if self._ocr is None:
            self.load()
        with self._lock:
            results = self._ocr.predict(image)
        if not results:
            return []
        return lines_from_prediction(_as_mapping(results[0]))


def _as_mapping(result: object) -> dict:
    """PaddleOCR returns a result object that behaves like a mapping but is not
    a dict. Normalize it so the layout code can stay unaware of the SDK."""
    if isinstance(result, dict):
        return result
    if hasattr(result, "keys"):
        return {key: result[key] for key in result.keys()}
    return getattr(result, "json", {}) or {}


def _flag(name: str, default: bool) -> bool:
    raw = os.environ.get(name)
    if raw is None or raw == "":
        return default
    return raw.strip().lower() in {"1", "true", "yes", "on"}
