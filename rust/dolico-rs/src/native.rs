//! Native (non-PDF) extraction: anydoc's document model -> canonical blocks,
//! plus the plain-text and Markdown inputs anydoc does not cover.
//!
//! anydoc's `Format` enum handles office and ebook containers (DOCX, XLSX,
//! PPTX, ODF, RTF, EPUB, CSV, and the legacy binary formats). It has no
//! Markdown, plain-text or HTML variant, so `.md` and `.txt` are handled here
//! directly. HTML is not supported in v1.
//!
//! ## One page, honestly
//!
//! anydoc's `Document` is a flat block list. It carries no slide, sheet or
//! section boundaries -- a multi-sheet workbook is emitted as a level-2
//! heading followed by a table, per sheet, with nothing structural marking the
//! break. Rather than guess where pages begin, every native document becomes a
//! single canonical page of kind `section`. Fabricating slide boundaries by
//! pattern-matching on headings would produce geometry-shaped data that is not
//! actually geometry.

use std::collections::BTreeMap;
use std::path::Path;

use anydoc::model as am;

use crate::canonical::{
    Asset, Block, BlockType, Cell, CellRef, Classification, IdGen, List, ListItem, Metadata, Page,
    PageKind, PageType, Provenance, Span, Table,
};
use crate::md;
use crate::{ENGINE_NATIVE, ShimError};

/// The anydoc crate version, recorded in every block's provenance so a
/// document can be traced back to the exact parser build that produced it.
pub const ANYDOC_VERSION: &str = "0.1.7";

/// What the shim decided the input is.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    /// An anydoc-supported container.
    Anydoc(anydoc::Format),
    /// Markdown source.
    Markdown,
    /// Plain text.
    Text,
    /// A PDF, which the PDF path handles instead.
    Pdf,
}

/// Decide what an input is, from its content signature first and its extension
/// second. CSV and Markdown carry no signature, so they can only be named.
pub fn detect(bytes: &[u8], path: &Path) -> Option<Kind> {
    if let Some(f) = anydoc::Format::from_bytes(bytes) {
        return Some(if f == anydoc::Format::Pdf {
            Kind::Pdf
        } else {
            Kind::Anydoc(f)
        });
    }
    let ext = path
        .extension()
        .and_then(|e| e.to_str())
        .unwrap_or_default()
        .to_ascii_lowercase();
    match ext.as_str() {
        "md" | "markdown" => Some(Kind::Markdown),
        "txt" | "text" | "log" => Some(Kind::Text),
        _ => anydoc::Format::from_extension(&ext).map(Kind::Anydoc),
    }
}

/// Extract a native document into a single canonical page.
pub fn extract(
    bytes: &[u8],
    kind: Kind,
    assets_dir: Option<&Path>,
) -> Result<(Metadata, Vec<Page>, Vec<Asset>), ShimError> {
    let mut ids = IdGen::new(1);
    let (blocks, assets) = match kind {
        Kind::Pdf => return Err(ShimError::Unsupported("PDF handled by the PDF path".into())),
        Kind::Markdown => {
            let prov = Provenance::new(ENGINE_NATIVE, ANYDOC_VERSION, "markdown/source");
            let text = String::from_utf8_lossy(bytes);
            (md::parse_blocks(&text, &mut ids, &prov), Vec::new())
        }
        Kind::Text => {
            let prov = Provenance::new(ENGINE_NATIVE, ANYDOC_VERSION, "text/paragraphs");
            let text = String::from_utf8_lossy(bytes);
            (text_blocks(&text, &mut ids, &prov), Vec::new())
        }
        Kind::Anydoc(format) => {
            let doc = anydoc::to_document(bytes, format)?;
            let assets = write_assets(&doc.assets, assets_dir)?;
            let prov = Provenance::new(ENGINE_NATIVE, ANYDOC_VERSION, "anydoc/document-model");
            let mut blocks = convert_blocks(&doc.blocks, &mut ids, &prov);
            // Notes have no place in the body flow, so they trail it. The note
            // id lives in provenance.method, which keeps the reference from
            // the text resolvable instead of dropping it on the floor.
            for note in &doc.notes {
                let kind = match note.kind {
                    am::NoteKind::Footnote => "footnote",
                    am::NoteKind::Endnote => "endnote",
                };
                let np = Provenance::new(
                    ENGINE_NATIVE,
                    ANYDOC_VERSION,
                    &format!("anydoc/{}:{}", kind, note.id),
                );
                blocks.extend(convert_blocks(&note.blocks, &mut ids, &np));
            }
            (blocks, assets)
        }
    };

    let mut custom = BTreeMap::new();
    custom.insert("source_kind".to_string(), kind_label(kind).to_string());

    let metadata = Metadata {
        page_count: 1,
        custom: Some(custom),
        ..Default::default()
    };
    let page = Page {
        number: 1,
        kind: PageKind::Section,
        width: None,
        height: None,
        classification: Classification {
            // Natively-parsed formats are read as data structures. The
            // text-versus-scanned distinction does not apply, and reporting a
            // confidence below 1.0 would invite pointless OCR escalation.
            kind: PageType::Native,
            confidence: 1.0,
            reasons: Vec::new(),
        },
        blocks,
    };
    Ok((metadata, vec![page], assets))
}

fn kind_label(kind: Kind) -> &'static str {
    match kind {
        Kind::Markdown => "markdown",
        Kind::Text => "text",
        Kind::Pdf => "pdf",
        Kind::Anydoc(f) => match f {
            anydoc::Format::Doc | anydoc::Format::Docx => "word",
            anydoc::Format::Odt => "odt",
            anydoc::Format::Ppt | anydoc::Format::Pptx => "powerpoint",
            anydoc::Format::Odp => "odp",
            anydoc::Format::Excel => "excel",
            anydoc::Format::Ods => "ods",
            anydoc::Format::Rtf => "rtf",
            anydoc::Format::Epub => "epub",
            anydoc::Format::Csv => "csv",
            anydoc::Format::Pdf => "pdf",
        },
    }
}

/// Plain text: blank lines separate paragraphs, and nothing is interpreted as
/// markup.
fn text_blocks(text: &str, ids: &mut IdGen, prov: &Provenance) -> Vec<Block> {
    let mut out = Vec::new();
    for para in text.split("\n\n") {
        let joined = para
            .lines()
            .map(str::trim_end)
            .collect::<Vec<_>>()
            .join("\n");
        if joined.trim().is_empty() {
            continue;
        }
        let mut b = Block::new(ids.next(), BlockType::Paragraph, prov.clone());
        b.text = Some(joined);
        out.push(b);
    }
    out
}

// ---------------------------------------------------------------------------
// anydoc model -> canonical
// ---------------------------------------------------------------------------

fn convert_blocks(blocks: &[am::Block], ids: &mut IdGen, prov: &Provenance) -> Vec<Block> {
    blocks
        .iter()
        .filter_map(|b| convert_block(b, ids, prov))
        .collect()
}

fn convert_block(block: &am::Block, ids: &mut IdGen, prov: &Provenance) -> Option<Block> {
    match block {
        am::Block::Heading { level, content, .. } => {
            let mut b = Block::new(ids.next(), BlockType::Heading, prov.clone());
            let (plain, spans) = convert_inlines(content);
            b.level = Some((*level).clamp(1, 6));
            b.text = Some(plain);
            b.inline = informative(spans);
            Some(b)
        }
        am::Block::Paragraph(content) => {
            // A paragraph holding nothing but an image is an image block, so
            // figures stay addressable rather than collapsing to alt text.
            if let Some((alt, source)) = lone_image(content) {
                let mut b = Block::new(ids.next(), BlockType::Image, prov.clone());
                b.alt = Some(alt);
                b.asset_ref = source;
                return Some(b);
            }
            if am::inlines_are_empty(content) {
                return None;
            }
            let mut b = Block::new(ids.next(), BlockType::Paragraph, prov.clone());
            let (plain, spans) = convert_inlines(content);
            b.text = Some(plain);
            b.inline = informative(spans);
            Some(b)
        }
        am::Block::List(list) => {
            let mut b = Block::new(ids.next(), BlockType::List, prov.clone());
            b.list = Some(List {
                ordered: list.ordered(),
                marker: Some(marker_label(list.marker)),
                start: if list.start != 1 {
                    Some(list.start)
                } else {
                    None
                },
                items: list
                    .items
                    .iter()
                    .map(|it| ListItem {
                        blocks: convert_blocks(&it.blocks, ids, prov),
                        checked: it.checked,
                    })
                    .collect(),
            });
            Some(b)
        }
        am::Block::Table(table) => {
            let mut b = Block::new(ids.next(), BlockType::Table, prov.clone());
            b.table = Some(convert_table(table, ids, prov));
            Some(b)
        }
        am::Block::BlockQuote(inner) => {
            let mut b = Block::new(ids.next(), BlockType::Quote, prov.clone());
            b.quote = Some(convert_blocks(inner, ids, prov));
            Some(b)
        }
        am::Block::CodeBlock { lang, text } => {
            let mut b = Block::new(ids.next(), BlockType::Code, prov.clone());
            b.text = Some(text.clone());
            b.lang = lang.clone();
            Some(b)
        }
        am::Block::Rule => Some(Block::new(ids.next(), BlockType::Rule, prov.clone())),
    }
}

fn convert_table(table: &am::Table, ids: &mut IdGen, prov: &Provenance) -> Table {
    let grid = table
        .grid
        .iter()
        .map(|row| {
            row.iter()
                .map(|slot| match slot {
                    am::CellSlot::Origin(cell) => Cell {
                        blocks: convert_blocks(&cell.blocks, ids, prov),
                        row_span: Some(cell.row_span.max(1)),
                        col_span: Some(cell.col_span.max(1)),
                        covered_by: None,
                    },
                    am::CellSlot::Covered {
                        origin_row,
                        origin_col,
                    } => Cell {
                        covered_by: Some(CellRef {
                            row: *origin_row,
                            col: *origin_col,
                        }),
                        ..Default::default()
                    },
                })
                .collect()
        })
        .collect();
    Table {
        header_rows: table.header_rows,
        kind: Some(match table.kind {
            am::TableKind::Data => "data",
            am::TableKind::Layout => "layout",
        }),
        grid,
    }
}

fn marker_label(m: am::MarkerKind) -> &'static str {
    match m {
        am::MarkerKind::Bullet => "bullet",
        am::MarkerKind::Decimal => "decimal",
        am::MarkerKind::LowerAlpha => "lower_alpha",
        am::MarkerKind::UpperAlpha => "upper_alpha",
        am::MarkerKind::LowerRoman => "lower_roman",
        am::MarkerKind::UpperRoman => "upper_roman",
    }
}

/// If these inlines are a single image (plus whitespace), return its alt text
/// and asset reference.
fn lone_image(inlines: &[am::Inline]) -> Option<(String, Option<String>)> {
    let mut found: Option<(String, Option<String>)> = None;
    for inline in inlines {
        match inline {
            am::Inline::Image { alt, source } => {
                if found.is_some() {
                    return None; // more than one image
                }
                found = Some((alt.clone(), image_ref(source)));
            }
            am::Inline::Anchor(_) | am::Inline::LineBreak => {}
            am::Inline::Text { text, .. } if text.trim().is_empty() => {}
            _ => return None,
        }
    }
    found
}

fn image_ref(source: &am::ImageSource) -> Option<String> {
    match source {
        am::ImageSource::Asset(id) => Some(asset_id(id.0)),
        am::ImageSource::External(url) => Some(url.clone()),
        am::ImageSource::Unavailable => None,
    }
}

fn asset_id(index: usize) -> String {
    format!("a{index}")
}

fn convert_inlines(inlines: &[am::Inline]) -> (String, Vec<Span>) {
    let mut plain = String::new();
    let mut spans = Vec::new();
    walk_inlines(inlines, &mut plain, &mut spans, &Span::default());
    (plain, spans)
}

fn walk_inlines(inlines: &[am::Inline], plain: &mut String, spans: &mut Vec<Span>, ctx: &Span) {
    for inline in inlines {
        match inline {
            am::Inline::Text { text, style } => {
                plain.push_str(text);
                spans.push(Span {
                    text: text.clone(),
                    bold: ctx.bold || style.bold,
                    italic: ctx.italic || style.italic,
                    strike: ctx.strike || style.strike,
                    code: ctx.code || style.code,
                    underline: ctx.underline,
                    href: ctx.href.clone(),
                });
            }
            am::Inline::Link { content, target } => {
                let href = match target {
                    am::LinkTarget::External(s)
                    | am::LinkTarget::Relative(s)
                    | am::LinkTarget::Anchor(s) => s.clone(),
                };
                let nested = Span {
                    href: Some(href),
                    ..ctx.clone_flags()
                };
                walk_inlines(content, plain, spans, &nested);
            }
            am::Inline::Image { alt, source } => {
                // Markdown cannot embed bytes, and neither can plain text: an
                // inline image reads as its alt text, with the asset still
                // referenced from the span.
                plain.push_str(alt);
                spans.push(Span {
                    text: alt.clone(),
                    href: image_ref(source).or_else(|| ctx.href.clone()),
                    ..ctx.clone_flags()
                });
            }
            am::Inline::LineBreak => {
                plain.push('\n');
                spans.push(Span {
                    text: "\n".to_string(),
                    ..ctx.clone_flags()
                });
            }
            // Anchors are zero-width targets and note references are resolved
            // through the trailing note blocks; neither contributes text.
            am::Inline::Anchor(_) | am::Inline::NoteRef(_) => {}
        }
    }
}

impl Span {
    /// A copy carrying only the style flags, not the text.
    fn clone_flags(&self) -> Span {
        Span {
            text: String::new(),
            bold: self.bold,
            italic: self.italic,
            strike: self.strike,
            code: self.code,
            underline: self.underline,
            href: self.href.clone(),
        }
    }
}

/// Drop the span list when it says nothing the plain text does not.
fn informative(spans: Vec<Span>) -> Option<Vec<Span>> {
    let any = spans
        .iter()
        .any(|s| s.bold || s.italic || s.strike || s.code || s.underline || s.href.is_some());
    if any { Some(spans) } else { None }
}

// ---------------------------------------------------------------------------
// Assets
// ---------------------------------------------------------------------------

/// Write embedded asset bytes into `assets_dir` and describe them. Bytes never
/// go into the JSON: a document with fifty images would otherwise base64 its
/// way into tens of megabytes of output.
fn write_assets(assets: &[am::Asset], dir: Option<&Path>) -> Result<Vec<Asset>, ShimError> {
    let Some(dir) = dir else {
        return Ok(Vec::new());
    };
    if assets.is_empty() {
        return Ok(Vec::new());
    }
    std::fs::create_dir_all(dir)?;

    let mut out = Vec::with_capacity(assets.len());
    for asset in assets {
        let id = asset_id(asset.id.0);
        let name = format!("{id}{}", extension_for(&asset.media_type));
        std::fs::write(dir.join(&name), &asset.bytes)?;
        out.push(Asset {
            id,
            media_type: asset.media_type.clone(),
            blob_ref: name,
            size_bytes: asset.bytes.len() as u64,
            origin: if asset.origin_part.is_empty() {
                None
            } else {
                Some(asset.origin_part.clone())
            },
        });
    }
    Ok(out)
}

fn extension_for(media_type: &str) -> &'static str {
    match media_type {
        "image/png" => ".png",
        "image/jpeg" => ".jpg",
        "image/gif" => ".gif",
        "image/bmp" => ".bmp",
        "image/tiff" => ".tiff",
        "image/webp" => ".webp",
        "image/svg+xml" => ".svg",
        "image/x-emf" | "image/emf" => ".emf",
        "image/x-wmf" | "image/wmf" => ".wmf",
        _ => ".bin",
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn detects_markdown_and_text_by_extension() {
        assert_eq!(detect(b"# hi", Path::new("a.md")), Some(Kind::Markdown));
        assert_eq!(detect(b"hi", Path::new("a.txt")), Some(Kind::Text));
    }

    #[test]
    fn detects_pdf_by_signature_even_with_wrong_extension() {
        assert_eq!(
            detect(
                b"%PDF-1.7\n%\xE2\xE3\xCF\xD3\n",
                Path::new("mislabeled.docx")
            ),
            Some(Kind::Pdf)
        );
    }

    #[test]
    fn detects_csv_only_by_extension() {
        // CSV carries no signature, so content alone cannot identify it.
        assert_eq!(detect(b"a,b\n1,2\n", Path::new("d")), None);
        assert_eq!(
            detect(b"a,b\n1,2\n", Path::new("d.csv")),
            Some(Kind::Anydoc(anydoc::Format::Csv))
        );
    }

    #[test]
    fn unknown_extension_is_unrecognized() {
        assert_eq!(detect(b"\x00\x01\x02", Path::new("x.bin")), None);
    }

    #[test]
    fn text_splits_on_blank_lines_and_keeps_inner_newlines() {
        let mut ids = IdGen::new(1);
        let prov = Provenance::new("t", "0", "t");
        let blocks = text_blocks("a\nb\n\nc", &mut ids, &prov);
        assert_eq!(blocks.len(), 2);
        assert_eq!(blocks[0].text.as_deref(), Some("a\nb"));
        assert_eq!(blocks[1].text.as_deref(), Some("c"));
    }

    #[test]
    fn markdown_input_produces_one_section_page() {
        let (meta, pages, assets) =
            extract(b"# Title\n\nBody text.", Kind::Markdown, None).unwrap();
        assert_eq!(meta.page_count, 1);
        assert!(assets.is_empty());
        assert_eq!(pages.len(), 1);
        assert_eq!(pages[0].kind, PageKind::Section);
        assert_eq!(pages[0].classification.kind, PageType::Native);
        assert_eq!(pages[0].number, 1);
        assert_eq!(pages[0].blocks.len(), 2);
        assert_eq!(pages[0].blocks[0].kind, BlockType::Heading);
    }

    #[test]
    fn native_pages_carry_no_geometry() {
        let (_, pages, _) = extract(b"text", Kind::Text, None).unwrap();
        assert!(pages[0].width.is_none());
        assert!(pages[0].blocks.iter().all(|b| b.bbox.is_none()));
    }

    #[test]
    fn csv_becomes_a_table() {
        let (_, pages, _) = extract(
            b"name,qty\nwidget,3\ngadget,4\n",
            Kind::Anydoc(anydoc::Format::Csv),
            None,
        )
        .unwrap();
        let table = pages[0]
            .blocks
            .iter()
            .find(|b| b.kind == BlockType::Table)
            .expect("csv should produce a table block");
        let grid = &table.table.as_ref().unwrap().grid;
        assert_eq!(grid.len(), 3);
        assert_eq!(grid[0].len(), 2);
    }

    #[test]
    fn pdf_kind_is_rejected_by_the_native_path() {
        let err = extract(b"%PDF-1.7", Kind::Pdf, None).unwrap_err();
        assert!(matches!(err, ShimError::Unsupported(_)));
    }
}
