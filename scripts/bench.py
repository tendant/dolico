#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["requests"]
# ///
"""Score extraction against ground truth.

Every quality default in this repository -- the OCR model, the escalation
threshold, the four weights in `internal/engine/quality` -- was chosen by
looking at output and deciding it seemed fine. This measures instead.

    make bench                       # score every fixture
    make bench BENCH_ARGS=--json     # machine-readable, for comparing runs
    ./scripts/bench.py --corpus /path/to/real/documents

What it reports, per document:

  CER   character error rate against the expected text, 0 is perfect
  WER   word error rate, which is the more legible of the two
  cells table cell accuracy, when the document is expected to contain a table
  ms    wall time through the whole pipeline

**On what these numbers do and do not mean.** The bundled fixtures are clean
synthetic renderings, so a low error rate on them says the pipeline is wired
correctly, not that OCR is accurate on real documents. Photographs of creased
paper will score far worse. The `--corpus` flag exists so a directory of real
documents can be scored by the same harness: put the files in it alongside a
`ground-truth.json` of the same shape.
"""

from __future__ import annotations

import argparse
import json
import pathlib
import statistics
import sys
import time

import requests

ROOT = pathlib.Path(__file__).resolve().parent.parent


# ---------------------------------------------------------------------------
# Scoring
# ---------------------------------------------------------------------------


def levenshtein(a: list, b: list) -> int:
    """Edit distance between two sequences.

    Iterative with a single row, because a page of text against its expectation
    is a few thousand elements and the full matrix is not worth allocating.
    """
    if a == b:
        return 0
    if not a:
        return len(b)
    if not b:
        return len(a)

    previous = list(range(len(b) + 1))
    for i, ca in enumerate(a, start=1):
        current = [i]
        for j, cb in enumerate(b, start=1):
            current.append(
                min(
                    previous[j] + 1,  # deletion
                    current[j - 1] + 1,  # insertion
                    previous[j - 1] + (ca != cb),  # substitution
                )
            )
        previous = current
    return previous[-1]


def normalize(text: str) -> str:
    """Collapse whitespace, keep everything else.

    Case and punctuation are kept on purpose: an OCR engine that reads
    "Amountdue" for "Amount due" has made a real mistake, and normalizing
    whitespace away would hide exactly the class of error these engines
    actually make.
    """
    return " ".join(text.split())


def error_rate(expected: str, actual: str) -> float:
    """Edit distance over the length of the expectation, capped at 1."""
    expected, actual = normalize(expected), normalize(actual)
    if not expected:
        return 0.0 if not actual else 1.0
    return min(1.0, levenshtein(list(expected), list(actual)) / len(expected))


def word_error_rate(expected: str, actual: str) -> float:
    expected_words = normalize(expected).split()
    actual_words = normalize(actual).split()
    if not expected_words:
        return 0.0 if not actual_words else 1.0
    return min(1.0, levenshtein(expected_words, actual_words) / len(expected_words))


def cell_accuracy(expected: list[list[str]], actual: list[list[str]]) -> tuple[float, str]:
    """Share of expected cells that appear, normalized, at the same position.

    Position matters: a grid whose rows are correct but reversed has recovered
    the text and lost the table, and this is the check that catches it. The
    table orientation classifier was doing exactly that until it was disabled.
    """
    total = sum(len(row) for row in expected)
    if total == 0:
        return 1.0, "no cells expected"
    if not actual:
        return 0.0, "no table recovered"

    matched = 0
    for r, row in enumerate(expected):
        for c, want in enumerate(row):
            if r < len(actual) and c < len(actual[r]):
                if normalize(actual[r][c]) == normalize(want):
                    matched += 1

    shape = f"{len(actual)}x{len(actual[0]) if actual else 0}"
    want_shape = f"{len(expected)}x{len(expected[0]) if expected else 0}"
    note = "" if shape == want_shape else f"shape {shape}, expected {want_shape}"
    return matched / total, note


# ---------------------------------------------------------------------------
# Reading a canonical document
# ---------------------------------------------------------------------------


def page_text(page: dict) -> str:
    """Every character on the page, in reading order, tables included.

    Table contents count toward the text score even though tables are also
    scored by cell, because the two measure different things and an engine
    should not be penalized on one for failing the other. Text-line OCR reads a
    scanned table's characters perfectly and recovers no grid: that should show
    as a good CER and a zero cell accuracy, not as a total failure at both.
    """
    parts: list[str] = []

    def walk(blocks):
        for b in blocks:
            if b.get("text"):
                parts.append(b["text"])
            walk(b.get("quote", []))
            for item in (b.get("list") or {}).get("items", []):
                walk(item.get("blocks", []))
            for row in (b.get("table") or {}).get("grid", []):
                for cell in row:
                    walk(cell.get("blocks", []))

    walk(page.get("blocks", []))
    return " ".join(parts)


def page_tables(page: dict) -> list[list[list[str]]]:
    """Every table on a page as a grid of strings, shadow slots skipped."""
    out = []

    def walk(blocks):
        for b in blocks:
            if b.get("type") == "table" and b.get("table"):
                grid = []
                for row in b["table"]["grid"]:
                    cells = []
                    for cell in row:
                        if "covered_by" in cell:
                            continue
                        cells.append(
                            " ".join(cb.get("text", "") for cb in cell.get("blocks", []))
                        )
                    grid.append(cells)
                out.append(grid)
            walk(b.get("quote", []))
            for item in (b.get("list") or {}).get("items", []):
                walk(item.get("blocks", []))

    walk(page.get("blocks", []))
    return out


def engines_used(doc: dict) -> set[str]:
    found = set()

    def walk(blocks):
        for b in blocks:
            found.add(b["provenance"]["engine"])
            walk(b.get("quote", []))
            for item in (b.get("list") or {}).get("items", []):
                walk(item.get("blocks", []))
            for row in (b.get("table") or {}).get("grid", []):
                for cell in row:
                    walk(cell.get("blocks", []))

    for page in doc.get("pages", []):
        walk(page.get("blocks", []))
    return found


# ---------------------------------------------------------------------------
# Running
# ---------------------------------------------------------------------------


def score_document(base: str, corpus: pathlib.Path, name: str, expected: dict) -> dict:
    path = corpus / name
    if not path.exists():
        return {"document": name, "error": "missing fixture"}

    started = time.monotonic()
    with open(path, "rb") as fh:
        resp = requests.post(
            f"{base}/v1/documents",
            files={"file": (name, fh)},
            params={"wait": "true"},
            timeout=1800,
        )
    wall_ms = int((time.monotonic() - started) * 1000)

    if not resp.ok:
        return {"document": name, "error": f"HTTP {resp.status_code}: {resp.text[:120]}"}
    doc = resp.json()

    by_number = {p["number"]: p for p in doc.get("pages", [])}
    cers, wers, accuracies, notes = [], [], [], []

    for want_page in expected["pages"]:
        page = by_number.get(want_page["number"])
        if page is None:
            cers.append(1.0)
            wers.append(1.0)
            notes.append(f"page {want_page['number']} missing")
            continue

        # The expectation is already in reading order with table contents
        # inline, so it can be compared against the page as extracted.
        want_text = " ".join(want_page.get("text", []))
        if want_text:
            got = page_text(page)
            cers.append(error_rate(want_text, got))
            wers.append(word_error_rate(want_text, got))

        for i, want_table in enumerate(want_page.get("tables", [])):
            tables = page_tables(page)
            got_table = tables[i] if i < len(tables) else []
            accuracy, note = cell_accuracy(want_table, got_table)
            accuracies.append(accuracy)
            if note:
                notes.append(note)

    return {
        "document": name,
        "pages": len(doc.get("pages", [])),
        "cer": statistics.mean(cers) if cers else None,
        "wer": statistics.mean(wers) if wers else None,
        "cells": statistics.mean(accuracies) if accuracies else None,
        "wall_ms": wall_ms,
        "engines": sorted(engines_used(doc)),
        "notes": notes,
    }


def render(rows: list[dict], meta: dict) -> None:
    print(f"\nengines: {', '.join(f'{k}@{v}' for k, v in sorted(meta.items()))}\n")
    print(f"{'document':22} {'pages':>5} {'CER':>7} {'WER':>7} {'cells':>7} {'ms':>7}  engines")
    print("-" * 88)

    for row in rows:
        if "error" in row:
            print(f"{row['document']:22} {'':>5} {'':>7} {'':>7} {'':>7} {'':>7}  {row['error']}")
            continue
        fmt = lambda v: "   -   " if v is None else f"{v:7.3f}"
        print(
            f"{row['document']:22} {row['pages']:>5} {fmt(row['cer'])} {fmt(row['wer'])} "
            f"{fmt(row['cells'])} {row['wall_ms']:>7}  {','.join(row['engines'])}"
        )
        for note in row["notes"]:
            print(f"{'':22} note: {note}")

    scored = [r for r in rows if "error" not in r]
    cers = [r["cer"] for r in scored if r["cer"] is not None]
    cells = [r["cells"] for r in scored if r["cells"] is not None]
    print("-" * 88)
    if cers:
        print(f"{'mean CER':22} {statistics.mean(cers):.4f}   (0 is perfect)")
    if cells:
        print(f"{'mean cell accuracy':22} {statistics.mean(cells):.4f}   (1 is perfect)")
    print(f"{'total wall time':22} {sum(r['wall_ms'] for r in scored) / 1000:.1f}s")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", default="http://127.0.0.1:8080", help="dolico API")
    parser.add_argument(
        "--corpus",
        type=pathlib.Path,
        default=ROOT / "testdata",
        help="directory holding the documents and a ground-truth.json",
    )
    parser.add_argument("--only", help="score just this document")
    parser.add_argument("--json", action="store_true", help="emit JSON instead of a table")
    args = parser.parse_args()

    truth_path = args.corpus / "ground-truth.json"
    if not truth_path.exists():
        print(f"no ground truth at {truth_path}", file=sys.stderr)
        return 2
    truth = json.loads(truth_path.read_text())

    try:
        info = requests.get(f"{args.base}/v1/engines", timeout=10).json()
    except requests.RequestException as exc:
        print(f"cannot reach {args.base}: {exc}", file=sys.stderr)
        return 2
    meta = {e["name"]: e["version"] for e in info["engines"]}

    rows = [
        score_document(args.base, args.corpus, name, expected)
        for name, expected in sorted(truth.items())
        if args.only is None or name == args.only
    ]

    if args.json:
        print(json.dumps({"engines": meta, "results": rows}, indent=2))
    else:
        render(rows, meta)
    return 0


if __name__ == "__main__":
    sys.exit(main())
