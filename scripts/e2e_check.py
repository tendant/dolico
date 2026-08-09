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

ROOT = pathlib.Path(__file__).resolve().parent.parent
TESTDATA = ROOT / "testdata"
BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8080"

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
CASES = [
    # fixture,        min_blocks, expected engines,          forbidden engines
    ("sample.md",     10, {"anydoc"},                  {"ocr-stub", "pdf-inspector"}),
    ("sample.txt",     3, {"anydoc"},                  {"ocr-stub", "pdf-inspector"}),
    ("sample.csv",     1, {"anydoc"},                  {"ocr-stub", "pdf-inspector"}),
    ("sample.docx",    8, {"anydoc"},                  {"ocr-stub", "pdf-inspector"}),
    ("sample.xlsx",    2, {"anydoc"},                  {"ocr-stub", "pdf-inspector"}),
    ("sample.pptx",    4, {"anydoc"},                  {"ocr-stub", "pdf-inspector"}),
    ("text.pdf",       2, {"pdf-inspector"},           {"ocr-stub"}),
    ("scanned.pdf",    1, {"ocr-stub"},                {"pdf-inspector"}),
    ("mixed.pdf",      2, {"pdf-inspector", "ocr-stub"}, set()),
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
    print("\nscanned.pdf routing detail")
    doc = upload("scanned.pdf").json()
    ocr_blocks = [b for b in blocks(doc) if b["provenance"]["engine"] == "ocr-stub"]
    check("the scanned page produced OCR blocks, not silence", len(ocr_blocks) > 0)

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
