#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["jsonschema", "requests"]
# ///
"""Drive a running dolico server over HTTP and validate everything it returns.

This is the check the Go tests cannot make: the Go tests assert invariants in
Go, but `schema/canonical-v1.json` is the actual cross-language contract, and
the only way to know a produced document satisfies it is to run a real JSON
Schema validator over it. Drift between the schema, the Go model and the Rust
model shows up here.

Usage: e2e_check.py [base-url]
"""

import json
import pathlib
import sys

import requests
from jsonschema import Draft202012Validator

import os

ROOT = pathlib.Path(__file__).resolve().parent.parent
TESTDATA = ROOT / "testdata"
BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8080"

# Which engine should have handled the scanned pages. The OCR tier is optional,
# so by default the sweep accepts either and only checks that *something* read
# them; `make e2e-ocr` sets this to "paddleocr" to assert the real one ran.
EXPECT_OCR = os.environ.get("DOLICO_EXPECT_OCR", "")

PASS, FAIL = "\033[32mPASS\033[0m", "\033[31mFAIL\033[0m"
failures: list[str] = []


def check(name: str, condition: bool, detail: str = "") -> None:
    print(f"  {PASS if condition else FAIL}  {name}" + (f" -- {detail}" if detail and not condition else ""))
    if not condition:
        failures.append(f"{name}: {detail}")


def upload(fixture: str, wait: bool = True) -> requests.Response:
    with open(TESTDATA / fixture, "rb") as fh:
        return requests.post(
            f"{BASE}/v1/documents",
            files={"file": (fixture, fh)},
            params={"wait": "true"} if wait else {},
            timeout=180,
        )


def blocks(doc: dict) -> list[dict]:
    """Every block on every page, including nested ones."""
    out = []

    def walk(bs):
        for b in bs:
            out.append(b)
            walk(b.get("quote", []))
            for item in (b.get("list") or {}).get("items", []):
                walk(item.get("blocks", []))
            for row in (b.get("table") or {}).get("grid", []):
                for cell in row:
                    walk(cell.get("blocks", []))

    for page in doc["pages"]:
        walk(page["blocks"])
    return out


def engines_used(doc: dict) -> set[str]:
    return {b["provenance"]["engine"] for b in blocks(doc)}


# Each fixture, and what routing must do with it. The PDFs are the interesting
# rows: they are what the per-page routing design exists for.
OCR_ENGINES = {"ocr-stub", "paddleocr", "pp-structurev3"}
# The tiers that actually read pixels, as opposed to the stub.
REAL_OCR = {"paddleocr", "pp-structurev3"}
# The OCR engine the assertions expect. Unset means "whichever is wired".
OCR = EXPECT_OCR or None


def ocr_engines_in(doc: dict) -> set[str]:
    return engines_used(doc) & OCR_ENGINES


CASES = [
    # fixture,        min_blocks, expected engines,   forbidden engines
    ("sample.md",     10, {"anydoc"},        OCR_ENGINES | {"pdf-inspector"}),
    ("sample.txt",     3, {"anydoc"},        OCR_ENGINES | {"pdf-inspector"}),
    ("sample.csv",     1, {"anydoc"},        OCR_ENGINES | {"pdf-inspector"}),
    ("sample.docx",    8, {"anydoc"},        OCR_ENGINES | {"pdf-inspector"}),
    ("sample.xlsx",    2, {"anydoc"},        OCR_ENGINES | {"pdf-inspector"}),
    ("sample.pptx",    4, {"anydoc"},        OCR_ENGINES | {"pdf-inspector"}),
    ("text.pdf",       2, {"pdf-inspector"}, OCR_ENGINES),
    # The OCR fixtures are checked separately below, because which engine is
    # acceptable depends on what is wired.
    ("scanned.pdf",       1, set(),             {"pdf-inspector"}),
    ("scanned-table.pdf", 1, set(),             {"pdf-inspector"}),
    ("mixed.pdf",         2, {"pdf-inspector"}, set()),
]


def main() -> int:
    schema = json.loads((ROOT / "schema" / "canonical-v1.json").read_text())
    validator = Draft202012Validator(schema)

    print(f"Health check against {BASE}")
    health = requests.get(f"{BASE}/healthz", timeout=10)
    check("server is healthy", health.ok and health.json()["status"] == "ok", health.text)
    if failures:
        return 1

    for fixture, min_blocks, expected, forbidden in CASES:
        print(f"\n{fixture}")
        resp = upload(fixture)
        if not resp.ok:
            check("upload succeeded", False, f"HTTP {resp.status_code}: {resp.text[:200]}")
            continue
        doc = resp.json()

        errors = sorted(validator.iter_errors(doc), key=lambda e: e.path)
        check(
            "validates against canonical-v1.json",
            not errors,
            "; ".join(f"{'/'.join(map(str, e.path))}: {e.message}" for e in errors[:3]),
        )

        found = engines_used(doc)
        check(f"blocks >= {min_blocks}", len(blocks(doc)) >= min_blocks, f"got {len(blocks(doc))}")
        check(f"routed to {sorted(expected)}", expected <= found, f"got {sorted(found)}")
        check(
            f"never routed to {sorted(forbidden) or '(nothing)'}",
            not (forbidden & found),
            f"unexpectedly used {sorted(forbidden & found)}",
        )
        check(
            "every page is scored",
            all(p.get("quality") is not None for p in doc["pages"]),
            "some pages have no quality score",
        )
        check(
            "every block records its engine",
            all(b["provenance"]["engine"] for b in blocks(doc)),
            "a block has no provenance",
        )

        md = requests.get(f"{BASE}/v1/documents/{doc['id']}.md", timeout=30)
        check("markdown view is served", md.ok and md.text.strip() != "", f"HTTP {md.status_code}")

        for asset in doc.get("assets", []):
            got = requests.get(f"{BASE}/v1/documents/{doc['id']}/assets/{asset['id']}", timeout=30)
            check(
                f"asset {asset['id']} is served",
                got.ok and len(got.content) == asset["size_bytes"],
                f"HTTP {got.status_code}, {len(got.content)} of {asset['size_bytes']} bytes",
            )

    # A scanned page that quietly returns nothing is the failure mode this
    # whole design is meant to prevent, so it gets its own explicit check.
    print("\nOCR routing")
    engines = requests.get(f"{BASE}/v1/engines", timeout=10).json()["engines"]
    wired = next((e["name"] for e in engines if e["name"] in OCR_ENGINES), None)
    print(f"  OCR tier wired: {wired}")
    if OCR:
        check(f"the {OCR} tier is wired", wired == OCR, f"found {wired}")

    doc = upload("scanned.pdf").json()
    ocr_blocks = [b for b in blocks(doc) if b["provenance"]["engine"] in OCR_ENGINES]
    check("the scanned page produced OCR blocks, not silence", len(ocr_blocks) > 0)
    if OCR:
        check(
            f"the scanned page was read by {OCR}",
            all(b["provenance"]["engine"] == OCR for b in ocr_blocks),
            f"engines: {sorted({b['provenance']['engine'] for b in ocr_blocks})}",
        )

    # mixed.pdf is the design's central claim: page 1 native, page 2 OCR.
    mixed = upload("mixed.pdf").json()
    page_engines = [
        sorted({b["provenance"]["engine"] for b in page["blocks"]})
        for page in mixed["pages"]
    ]
    check("mixed.pdf page 1 was extracted natively", page_engines[0] == ["pdf-inspector"], f"{page_engines}")
    check("mixed.pdf page 2 went to OCR", set(page_engines[1]) <= OCR_ENGINES, f"{page_engines}")

    if OCR in REAL_OCR:
        # Only real OCR can recover text that exists solely as pixels.
        text = " ".join(b.get("text", "") for b in blocks(doc)).upper()
        for word in ("INVOICE", "4471"):
            check(f"real OCR recovered {word!r} from the pixels", word in text, f"got {text[:120]!r}")
        # OCR is the only tier that reports a genuine per-block confidence, and
        # the only one that knows the page size, because it renders.
        check(
            "OCR blocks carry confidence and geometry",
            all(b.get("confidence") is not None and b.get("bbox") for b in ocr_blocks),
            "a block is missing confidence or bbox",
        )
        page = doc["pages"][0]
        check(
            "the OCR page reports real dimensions",
            page.get("width") and page.get("height"),
            f"width={page.get('width')} height={page.get('height')}",
        )

    if OCR == "pp-structurev3":
        # The whole point of Tier 2: a table that exists only as pixels comes
        # back as a grid, not as loose text.
        print("\nlayout analysis (scanned-table.pdf)")
        table_doc = upload("scanned-table.pdf").json()
        tables = [b for b in blocks(table_doc) if b["type"] == "table"]
        check("the scanned table was recognized as a table", len(tables) == 1, f"found {len(tables)}")
        if tables:
            grid = tables[0]["table"]["grid"]
            rows = [
                [
                    (c["blocks"][0]["text"] if c.get("blocks") else "")
                    for c in row
                    if "covered_by" not in c
                ]
                for row in grid
            ]
            check("the grid is 5 rows by 3 columns", len(grid) == 5 and len(grid[0]) == 3, f"{len(grid)}x{len(grid[0]) if grid else 0}")
            # Row order and column order both matter: the table orientation
            # classifier rotates the grid 180 degrees when left enabled, which
            # puts the header last and reverses the columns.
            check(
                "the header row is first and its columns are in order",
                rows and rows[0] == ["Region", "Units", "Revenue"],
                f"first row: {rows[0] if rows else None}",
            )
            check(
                "the first data row follows the header",
                len(rows) > 1 and rows[1][0] == "North",
                f"second row: {rows[1] if len(rows) > 1 else None}",
            )
        labels = [b["provenance"]["method"] for b in blocks(table_doc)]
        check(
            "regions are labelled by the layout model",
            all(m.startswith("pp-structurev3/layout:") for m in labels),
            f"methods: {sorted(set(labels))}",
        )
        # Reading order: the page title must precede the table it introduces.
        texts = [b.get("text", "") for b in table_doc["pages"][0]["blocks"]]
        check(
            "the page title comes before the table",
            texts and "QUARTERLY" in texts[0].upper(),
            f"first block text: {texts[0] if texts else None!r}",
        )

        md = requests.get(f"{BASE}/v1/documents/{table_doc['id']}.md", timeout=30).text
        check(
            "the Markdown view renders a real table",
            "| Region | Units | Revenue |" in md and "| North | 120 |" in md,
            md[:200],
        )

    print("\nerror paths")
    corrupt = upload("corrupt.pdf")
    check("corrupt PDF returns 422", corrupt.status_code == 422, f"HTTP {corrupt.status_code}")
    check("corrupt PDF is reported as malformed", corrupt.json().get("kind") == "malformed", corrupt.text[:120])

    junk = requests.post(
        f"{BASE}/v1/documents",
        files={"file": ("mystery.bin", b"\x00\x01\x02\x03")},
        params={"wait": "true"},
        timeout=60,
    )
    check("unknown format returns 415", junk.status_code == 415, f"HTTP {junk.status_code}")

    missing = requests.get(f"{BASE}/v1/documents/{'0' * 64}", timeout=10)
    check("unknown document returns 404", missing.status_code == 404, f"HTTP {missing.status_code}")

    # Re-uploading identical bytes must produce an identical document. This is
    # the invariant that a caching bug breaks: an earlier version served the
    # pages from cache, lost the assets along the way, and overwrote the good
    # stored document with a degraded one.
    print("\nidempotency")
    first = upload("sample.pptx").json()
    second = upload("sample.pptx").json()
    check("re-upload resolves to the same document id", first["id"] == second["id"])
    check(
        "re-upload keeps every asset",
        [a["id"] for a in first.get("assets", [])] == [a["id"] for a in second.get("assets", [])],
        f"{first.get('assets')} then {second.get('assets')}",
    )
    # Trace timings and ids legitimately differ between runs; the content must
    # not.
    for doc in (first, second):
        doc.pop("trace", None)
    check(
        "re-upload produces identical content",
        json.dumps(first, sort_keys=True) == json.dumps(second, sort_keys=True),
        "the second run returned different content",
    )

    stats = requests.get(f"{BASE}/v1/engines", timeout=10).json()["cache"]
    print(f"  cache stats: {stats}")

    print()
    if failures:
        print(f"\033[31m{len(failures)} check(s) failed\033[0m")
        for f in failures:
            print(f"  - {f}")
        return 1
    print("\033[32mAll checks passed\033[0m")
    return 0


if __name__ == "__main__":
    sys.exit(main())
