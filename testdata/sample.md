# Routing Notes

The router decides **per page**, never per document. A PDF whose first page is
text and whose second page is a scan should cost one OCR call, not two.

## Principles

- Extract natively whenever possible.
- OCR only the pages that require it.
- Keep engines *replaceable*.

## Steps

1. Inspect the document.
2. Route each page.
3. Normalize into the canonical model.

## Support matrix

| Format | Engine | Needs OCR |
|---|---|---|
| DOCX | anydoc | no |
| Text PDF | pdf-inspector | no |
| Scanned PDF | paddleocr | yes |

## Escalation

> A page that extracts cleanly is taken at face value. A page that scores
> below the threshold is re-extracted by the next tier.

Configuration lives in `DOLICO_OCR_THRESHOLD`:

```go
if page.Quality.Score < threshold {
    escalate(page)
}
```

- [x] Native extraction
- [ ] Real OCR

See [the design document](document-processing-design.md) for the full rationale.

---

Multibyte text must survive intact: héllo — wörld 日本語.
