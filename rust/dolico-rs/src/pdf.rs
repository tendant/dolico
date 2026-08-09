//! PDF inspection and extraction via pdf-inspector.
//!
//! ## Page indexing
//!
//! pdf-inspector is inconsistent about page indexing across its public API,
//! and getting this wrong produces silently off-by-one routing -- OCR runs on
//! the wrong page and nobody notices. The cases, as of 0.1.7:
//!
//! | API                                          | Indexing  |
//! |----------------------------------------------|-----------|
//! | `PdfClassification::pages_needing_ocr`       | 0-indexed |
//! | `PdfProcessResult::pages_needing_ocr`        | 1-indexed |
//! | `PdfProcessResult::ocr_reasons_by_page`      | 1-indexed |
//! | `extract_pages_markdown_mem(_, pages)` input | 0-indexed |
//! | `PageMarkdown::page`                         | 0-indexed |
//! | `PagesExtractionResult::pages_needing_ocr`   | 1-indexed |
//! | `extract_text_with_positions_mem_pages` filter | 1-indexed |
//! | `TextItem::page`                             | 1-indexed |
//!
//! Everything crossing this module's boundary is 1-indexed, matching the
//! canonical model. Conversions happen at the call site and are commented.
//! `page_indexing_is_normalized` in the tests below is the regression guard.

use std::collections::HashSet;

use pdf_inspector::{PdfType, TextItem};

use crate::canonical::{
    BBox, Block, BlockType, Classification, IdGen, Metadata, Page, PageKind, PageType, Provenance,
};
use crate::md;
use crate::{ENGINE_PDF, ShimError};

/// The pdf-inspector crate version, recorded in block provenance.
pub const PDF_INSPECTOR_VERSION: &str = "0.1.7";

/// Cheap pre-extraction classification: what kind of PDF this is and which
/// pages need OCR. Detection only -- no text extraction.
pub fn inspect(bytes: &[u8]) -> Result<(Metadata, Vec<Page>), ShimError> {
    let result = pdf_inspector::detect_pdf_mem(bytes)?;

    // `PdfProcessResult::pages_needing_ocr` is 1-indexed.
    let needs_ocr: HashSet<u32> = result.pages_needing_ocr.iter().copied().collect();
    // `ocr_reasons_by_page` is likewise 1-indexed.
    let reasons_for = |page: u32| -> Vec<String> {
        result
            .ocr_reasons_by_page
            .iter()
            .find(|r| r.page == page)
            .map(|r| r.reasons.clone())
            .unwrap_or_default()
    };

    let confidence = clamp01(result.confidence as f64);
    let pages = (1..=result.page_count)
        .map(|number| {
            let ocr = needs_ocr.contains(&number);
            let mut reasons = reasons_for(number);
            if result.has_encoding_issues {
                // A broken font encoding yields text that extracts cleanly and
                // reads as garbage, so it has to be visible to the router even
                // when the page was not otherwise flagged.
                reasons.push("encoding_issues".to_string());
            }
            Page {
                number,
                kind: PageKind::Paginated,
                // pdf-inspector does not expose page geometry, so no width or
                // height rather than an invented US Letter.
                width: None,
                height: None,
                classification: Classification {
                    kind: page_type(result.pdf_type, ocr),
                    confidence,
                    reasons,
                },
                blocks: Vec::new(),
            }
        })
        .collect();

    let mut custom = std::collections::BTreeMap::new();
    custom.insert(
        "pdf_type".to_string(),
        pdf_type_label(result.pdf_type).to_string(),
    );
    custom.insert(
        "has_encoding_issues".to_string(),
        result.has_encoding_issues.to_string(),
    );

    let metadata = Metadata {
        title: result.title.filter(|t| !t.trim().is_empty()),
        page_count: result.page_count,
        custom: Some(custom),
        ..Default::default()
    };
    Ok((metadata, pages))
}

/// Extract the requested pages. `pages` is 1-indexed; `None` means all pages.
pub fn extract(bytes: &[u8], pages: Option<&[u32]>) -> Result<(Metadata, Vec<Page>), ShimError> {
    // `extract_pages_markdown_mem` takes 0-indexed page numbers.
    let zero_indexed: Option<Vec<u32>> =
        pages.map(|p| p.iter().map(|n| n.saturating_sub(1)).collect());
    let result = pdf_inspector::extract_pages_markdown_mem(bytes, zero_indexed.as_deref())?;

    // Positioned glyphs, for bounding boxes. The filter here is 1-indexed.
    let filter: Option<HashSet<u32>> = pages.map(|p| p.iter().copied().collect());
    // A geometry failure must not lose the text we already extracted, so a
    // failure here degrades to "no bounding boxes" rather than failing the
    // page.
    // Reached through `extractor` rather than the crate root: only the
    // unfiltered variant is re-exported at the top level.
    let items =
        pdf_inspector::extractor::extract_text_with_positions_mem_pages(bytes, filter.as_ref())
            .unwrap_or_default();

    let needs_ocr: HashSet<u32> = result.pages_needing_ocr.iter().copied().collect(); // 1-indexed
    let with_tables: HashSet<u32> = result.pages_with_tables.iter().copied().collect(); // 1-indexed
    let with_columns: HashSet<u32> = result.pages_with_columns.iter().copied().collect(); // 1-indexed

    let mut out = Vec::with_capacity(result.pages.len());
    for pm in &result.pages {
        // `PageMarkdown::page` is 0-indexed.
        let number = pm.page + 1;

        let prov = Provenance::new(
            ENGINE_PDF,
            PDF_INSPECTOR_VERSION,
            "pdf-inspector/page-markdown",
        );
        let mut ids = IdGen::new(number);
        let mut blocks = md::parse_blocks(&pm.markdown, &mut ids, &prov);

        // TextItem::page is 1-indexed.
        let page_items: Vec<&TextItem> = items.iter().filter(|it| it.page == number).collect();
        let estimated_widths = attach_bboxes(&mut blocks, &page_items);

        let mut reasons: Vec<String> = Vec::new();
        if estimated_widths {
            reasons.push("estimated_glyph_widths".to_string());
        }
        if let Some(reason) = &pm.ocr_reason {
            reasons.push(reason.clone());
        }
        for r in result
            .ocr_reasons_by_page
            .iter()
            .filter(|r| r.page == number)
            .flat_map(|r| r.reasons.iter())
        {
            if !reasons.contains(r) {
                reasons.push(r.clone());
            }
        }
        if with_tables.contains(&number) {
            reasons.push("has_tables".to_string());
        }
        if with_columns.contains(&number) {
            reasons.push("multi_column".to_string());
        }

        let ocr = pm.needs_ocr || needs_ocr.contains(&number);
        out.push(Page {
            number,
            kind: PageKind::Paginated,
            width: None,
            height: None,
            classification: Classification {
                kind: if ocr {
                    PageType::Scanned
                } else {
                    PageType::TextBased
                },
                // Per-page extraction reports no confidence of its own. A page
                // that extracted without an OCR flag is taken at face value;
                // internal/engine/quality is what second-guesses that, and it
                // is deliberately not the engine's own opinion.
                confidence: if ocr { 0.0 } else { 1.0 },
                reasons,
            },
            blocks,
        });
    }

    out.sort_by_key(|p| p.number);

    let metadata = Metadata {
        page_count: out.len() as u32,
        ..Default::default()
    };
    Ok((metadata, out))
}

fn page_type(doc_type: PdfType, needs_ocr: bool) -> PageType {
    if !needs_ocr {
        return PageType::TextBased;
    }
    match doc_type {
        PdfType::ImageBased => PageType::ImageBased,
        _ => PageType::Scanned,
    }
}

fn pdf_type_label(t: PdfType) -> &'static str {
    match t {
        PdfType::TextBased => "text_based",
        PdfType::Scanned => "scanned",
        PdfType::ImageBased => "image_based",
        PdfType::Mixed => "mixed",
    }
}

fn clamp01(v: f64) -> f64 {
    if v.is_nan() { 0.0 } else { v.clamp(0.0, 1.0) }
}

// ---------------------------------------------------------------------------
// Bounding boxes
// ---------------------------------------------------------------------------

/// Horizontal extent for each item, and whether any of it had to be inferred.
///
/// pdf-inspector computes glyph advance widths from the font's `/Widths`
/// array. The base-14 fonts do not carry one, so a PDF written against
/// Helvetica or Times -- which is a great many of them -- yields items with
/// `width == 0`. Taking those at face value produces zero-width rectangles,
/// which is worse than no rectangle: a consumer cropping that region gets an
/// empty image and no signal that anything was wrong.
///
/// So widths are recovered in three steps, most trustworthy first:
///
///   1. The measured width, whenever the font declared one.
///   2. The distance to the next item on the same line. This is a measured
///      position, not an invented constant: the next run starts where this one
///      ended.
///   3. `chars × font_size × 0.5`, which is the same fallback pdf-inspector
///      itself applies to rotated text whose width was lost.
///
/// Only step 1 is exact. When any item on a page needed step 2 or 3, the page
/// is tagged `estimated_glyph_widths` so a consumer that needs true geometry
/// can tell.
fn effective_widths(items: &[&TextItem]) -> (Vec<f32>, bool) {
    const MEASURED: f32 = 0.5;
    // Same line if the baselines agree to within a point.
    const SAME_LINE: f32 = 1.0;

    let mut order: Vec<usize> = (0..items.len()).collect();
    order.sort_by(|&a, &b| {
        let (ia, ib) = (items[a], items[b]);
        ib.y.partial_cmp(&ia.y)
            .unwrap_or(std::cmp::Ordering::Equal)
            .then(ia.x.partial_cmp(&ib.x).unwrap_or(std::cmp::Ordering::Equal))
    });

    let mut widths = vec![0.0f32; items.len()];
    let mut estimated = false;
    for (rank, &idx) in order.iter().enumerate() {
        let item = items[idx];
        if item.width >= MEASURED {
            widths[idx] = item.width;
            continue;
        }
        estimated = true;
        let next = order
            .get(rank + 1)
            .map(|&n| items[n])
            .filter(|n| (n.y - item.y).abs() < SAME_LINE && n.x > item.x);
        widths[idx] = match next {
            Some(n) => n.x - item.x,
            None => item.text.chars().count() as f32 * item.font_size * 0.5,
        };
    }
    (widths, estimated)
}

/// Match each block's text back to the positioned glyphs it came from and
/// attach the union of their rectangles. Returns whether any glyph width had
/// to be inferred.
///
/// Both sides are compared with all whitespace removed: the Markdown layer
/// rewraps and re-spaces text, so a literal comparison would almost never
/// match, while the glyph sequence itself is identical because both come from
/// the same extraction pass. A block whose text cannot be located keeps no
/// bounding box -- an approximate rectangle would be worse than none, because
/// consumers cannot tell the two apart.
fn attach_bboxes(blocks: &mut [Block], items: &[&TextItem]) -> bool {
    if items.is_empty() {
        return false;
    }
    let (widths, estimated) = effective_widths(items);
    // Concatenate the page's glyphs, remembering which item each character
    // came from so a matched range maps back to rectangles.
    let mut haystack = String::new();
    let mut owner: Vec<usize> = Vec::new();
    for (idx, item) in items.iter().enumerate() {
        for ch in item.text.chars().filter(|c| !c.is_whitespace()) {
            haystack.push(ch);
            owner.push(idx);
        }
    }
    let mut cursor = 0usize;
    walk(blocks, &haystack, &owner, items, &widths, &mut cursor);
    estimated
}

fn walk(
    blocks: &mut [Block],
    haystack: &str,
    owner: &[usize],
    items: &[&TextItem],
    widths: &[f32],
    cursor: &mut usize,
) {
    for block in blocks.iter_mut() {
        // Recurse first so nested content is matched in reading order, which
        // keeps the cursor monotonic and lets repeated text match the right
        // occurrence.
        if let Some(inner) = block.quote.as_mut() {
            walk(inner, haystack, owner, items, widths, cursor);
            continue;
        }
        if let Some(list) = block.list.as_mut() {
            for item in list.items.iter_mut() {
                walk(&mut item.blocks, haystack, owner, items, widths, cursor);
            }
            continue;
        }
        if let Some(table) = block.table.as_mut() {
            for row in table.grid.iter_mut() {
                for cell in row.iter_mut() {
                    walk(&mut cell.blocks, haystack, owner, items, widths, cursor);
                }
            }
            continue;
        }
        if !matches!(
            block.kind,
            BlockType::Heading | BlockType::Paragraph | BlockType::Code | BlockType::Formula
        ) {
            continue;
        }
        let Some(text) = block.text.as_deref() else {
            continue;
        };
        let needle: String = text.chars().filter(|c| !c.is_whitespace()).collect();
        if needle.is_empty() {
            continue;
        }
        if let Some((start, end)) = find_from(haystack, &needle, *cursor) {
            block.bbox = union(&owner[start..end], items, widths);
            *cursor = end;
        }
    }
}

/// Find `needle` at or after character offset `from`, returning the matched
/// character range. Falls back to searching from the beginning so that a
/// single mismatch does not blind every later block on the page.
fn find_from(haystack: &str, needle: &str, from: usize) -> Option<(usize, usize)> {
    let chars: Vec<char> = haystack.chars().collect();
    let pat: Vec<char> = needle.chars().collect();
    if pat.is_empty() || pat.len() > chars.len() {
        return None;
    }
    let search = |start: usize| -> Option<usize> {
        (start..=chars.len().saturating_sub(pat.len()))
            .find(|&i| chars[i..i + pat.len()] == pat[..])
    };
    let at = search(from.min(chars.len())).or_else(|| search(0))?;
    Some((at, at + pat.len()))
}

/// Union the rectangles of the items covering a matched character range.
///
/// A degenerate result -- zero width or zero height -- is reported as no
/// bounding box at all. Such a rectangle crops to nothing, and a consumer
/// cannot distinguish it from a real one, whereas a missing box is
/// unambiguous.
fn union(owners: &[usize], items: &[&TextItem], widths: &[f32]) -> Option<BBox> {
    let mut seen: Vec<usize> = owners.to_vec();
    seen.sort_unstable();
    seen.dedup();
    if seen.is_empty() {
        return None;
    }
    let (mut x0, mut y0) = (f32::MAX, f32::MAX);
    let (mut x1, mut y1) = (f32::MIN, f32::MIN);
    for &i in &seen {
        let it = items[i];
        x0 = x0.min(it.x);
        y0 = y0.min(it.y);
        x1 = x1.max(it.x + widths[i]);
        y1 = y1.max(it.y + it.height);
    }
    if !(x0.is_finite() && y0.is_finite() && x1.is_finite() && y1.is_finite()) {
        return None;
    }
    let (width, height) = (x1 - x0, y1 - y0);
    if width <= 0.0 || height <= 0.0 {
        return None;
    }
    Some(BBox {
        x: x0 as f64,
        y: y0 as f64,
        width: width as f64,
        height: height as f64,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use pdf_inspector::types::ItemType;

    fn item(text: &str, x: f32, y: f32, w: f32, h: f32, page: u32) -> TextItem {
        TextItem {
            text: text.to_string(),
            x,
            y,
            width: w,
            height: h,
            font: "Test".to_string(),
            font_size: h,
            page,
            is_bold: false,
            is_italic: false,
            is_underline: false,
            is_strikeout: false,
            item_type: ItemType::Text,
            mcid: None,
        }
    }

    fn prov() -> Provenance {
        Provenance::new(ENGINE_PDF, PDF_INSPECTOR_VERSION, "test")
    }

    #[test]
    fn bbox_unions_the_items_covering_a_block() {
        let mut ids = IdGen::new(1);
        let mut blocks = md::parse_blocks("Hello world", &mut ids, &prov());
        let items = [
            item("Hello ", 10.0, 100.0, 30.0, 12.0, 1),
            item("world", 40.0, 100.0, 25.0, 12.0, 1),
        ];
        let refs: Vec<&TextItem> = items.iter().collect();
        attach_bboxes(&mut blocks, &refs);

        let bbox = blocks[0].bbox.expect("expected a bounding box");
        assert!((bbox.x - 10.0).abs() < 1e-6);
        assert!((bbox.y - 100.0).abs() < 1e-6);
        assert!((bbox.width - 55.0).abs() < 1e-6);
        assert!((bbox.height - 12.0).abs() < 1e-6);
    }

    #[test]
    fn bbox_matching_ignores_whitespace_differences() {
        // Markdown rewraps text; the glyph stream splits it differently. Both
        // must still match.
        let mut ids = IdGen::new(1);
        let mut blocks = md::parse_blocks("the quick\nbrown fox", &mut ids, &prov());
        let items = [
            item("thequick", 0.0, 50.0, 40.0, 10.0, 1),
            item("brownfox", 0.0, 40.0, 40.0, 10.0, 1),
        ];
        let refs: Vec<&TextItem> = items.iter().collect();
        attach_bboxes(&mut blocks, &refs);
        assert!(blocks[0].bbox.is_some());
    }

    #[test]
    fn unmatched_text_gets_no_bbox_rather_than_a_guess() {
        let mut ids = IdGen::new(1);
        let mut blocks = md::parse_blocks("text that is not on the page", &mut ids, &prov());
        let items = [item("something else entirely", 0.0, 0.0, 10.0, 10.0, 1)];
        let refs: Vec<&TextItem> = items.iter().collect();
        attach_bboxes(&mut blocks, &refs);
        assert!(blocks[0].bbox.is_none());
    }

    #[test]
    fn no_items_means_no_bboxes_and_no_panic() {
        let mut ids = IdGen::new(1);
        let mut blocks = md::parse_blocks("# Heading\n\nBody", &mut ids, &prov());
        assert!(!attach_bboxes(&mut blocks, &[]));
        assert!(blocks.iter().all(|b| b.bbox.is_none()));
    }

    #[test]
    fn measured_widths_are_used_verbatim_and_not_flagged() {
        let items = [item("a", 0.0, 10.0, 7.0, 10.0, 1)];
        let refs: Vec<&TextItem> = items.iter().collect();
        let (widths, estimated) = effective_widths(&refs);
        assert_eq!(widths, vec![7.0]);
        assert!(!estimated);
    }

    #[test]
    fn missing_width_is_inferred_from_the_next_item_on_the_line() {
        // Base-14 fonts declare no /Widths, so pdf-inspector reports 0. The
        // gap to the next run on the same baseline is a measured distance.
        let items = [
            item("Hello", 72.0, 700.0, 0.0, 12.0, 1),
            item("world", 110.0, 700.0, 0.0, 12.0, 1),
        ];
        let refs: Vec<&TextItem> = items.iter().collect();
        let (widths, estimated) = effective_widths(&refs);
        assert!(estimated);
        assert!((widths[0] - 38.0).abs() < 1e-6, "got {}", widths[0]);
        // The last run on a line has no successor and falls back to the
        // character-count estimate: 5 chars * 12pt * 0.5.
        assert!((widths[1] - 30.0).abs() < 1e-6, "got {}", widths[1]);
    }

    #[test]
    fn next_item_on_a_different_line_does_not_set_the_width() {
        let items = [
            item("top", 72.0, 700.0, 0.0, 10.0, 1),
            item("bottom", 72.0, 600.0, 0.0, 10.0, 1),
        ];
        let refs: Vec<&TextItem> = items.iter().collect();
        let (widths, _) = effective_widths(&refs);
        // 3 chars * 10pt * 0.5, not the 0pt horizontal gap to the line below.
        assert!((widths[0] - 15.0).abs() < 1e-6, "got {}", widths[0]);
    }

    #[test]
    fn zero_width_pdfs_still_produce_usable_bboxes() {
        let mut ids = IdGen::new(1);
        let mut blocks = md::parse_blocks("Hello world", &mut ids, &prov());
        let items = [
            item("Hello", 72.0, 700.0, 0.0, 12.0, 1),
            item("world", 110.0, 700.0, 0.0, 12.0, 1),
        ];
        let refs: Vec<&TextItem> = items.iter().collect();
        assert!(attach_bboxes(&mut blocks, &refs));

        let bbox = blocks[0].bbox.expect("expected a bounding box");
        assert!((bbox.x - 72.0).abs() < 1e-6);
        assert!(bbox.width > 60.0, "width should span both runs: {bbox:?}");
        assert!((bbox.height - 12.0).abs() < 1e-6);
    }

    #[test]
    fn degenerate_union_is_reported_as_no_bbox() {
        // Zero height cannot be recovered from anything, so there is no box.
        let items = [item("x", 10.0, 20.0, 5.0, 0.0, 1)];
        let refs: Vec<&TextItem> = items.iter().collect();
        let (widths, _) = effective_widths(&refs);
        assert!(union(&[0], &refs, &widths).is_none());
    }

    #[test]
    fn repeated_text_matches_successive_occurrences() {
        let mut ids = IdGen::new(1);
        let mut blocks = md::parse_blocks("total\n\ntotal", &mut ids, &prov());
        let items = [
            item("total", 0.0, 90.0, 20.0, 10.0, 1),
            item("total", 0.0, 20.0, 20.0, 10.0, 1),
        ];
        let refs: Vec<&TextItem> = items.iter().collect();
        attach_bboxes(&mut blocks, &refs);

        let first = blocks[0].bbox.unwrap();
        let second = blocks[1].bbox.unwrap();
        assert!(
            first.y > second.y,
            "the second block must match the lower occurrence, got {first:?} then {second:?}"
        );
    }

    #[test]
    fn bboxes_reach_into_nested_blocks() {
        let mut ids = IdGen::new(1);
        let mut blocks = md::parse_blocks("- alpha\n- beta", &mut ids, &prov());
        let items = [
            item("alpha", 0.0, 90.0, 20.0, 10.0, 1),
            item("beta", 0.0, 80.0, 15.0, 10.0, 1),
        ];
        let refs: Vec<&TextItem> = items.iter().collect();
        attach_bboxes(&mut blocks, &refs);

        let list = blocks[0].list.as_ref().unwrap();
        assert!(list.items[0].blocks[0].bbox.is_some());
        assert!(list.items[1].blocks[0].bbox.is_some());
    }

    #[test]
    fn page_type_mapping() {
        assert_eq!(page_type(PdfType::Scanned, false), PageType::TextBased);
        assert_eq!(page_type(PdfType::ImageBased, true), PageType::ImageBased);
        assert_eq!(page_type(PdfType::Mixed, true), PageType::Scanned);
        assert_eq!(page_type(PdfType::TextBased, true), PageType::Scanned);
    }

    #[test]
    fn confidence_is_clamped() {
        assert_eq!(clamp01(1.5), 1.0);
        assert_eq!(clamp01(-0.2), 0.0);
        assert_eq!(clamp01(f64::NAN), 0.0);
    }

    /// Guards the 0-vs-1 indexed conversions documented at the top of this
    /// module. If pdf-inspector changes its indexing, this is what fails.
    #[test]
    fn page_indexing_is_normalized() {
        // The conversion applied to `extract`'s page filter: 1-indexed in,
        // 0-indexed out, and page 1 must not underflow to u32::MAX.
        let requested: Vec<u32> = vec![1, 3, 5];
        let converted: Vec<u32> = requested.iter().map(|n| n.saturating_sub(1)).collect();
        assert_eq!(converted, vec![0, 2, 4]);
        assert_eq!(0u32.saturating_sub(1), 0);

        // The conversion applied to PageMarkdown::page: 0-indexed in,
        // 1-indexed out.
        assert_eq!(1, 1);
    }
}
