//! Serde mirror of the canonical document model.
//!
//! This mirrors `internal/canonical/document.go` and `schema/canonical-v1.json`.
//! All three must agree; the Go golden tests validate assembled documents
//! against the schema, which is what catches drift between these mirrors.
//!
//! The shim emits *fragments*, not whole documents: it knows about pages,
//! blocks and assets, but not about document ids, source hashes or trace ids,
//! which belong to the orchestrator. See [`ExtractOutput`] and
//! [`InspectOutput`].

use serde::Serialize;

pub const SCHEMA_VERSION: &str = "1.0";

// ---------------------------------------------------------------------------
// Output envelopes
// ---------------------------------------------------------------------------

/// What `dolico-rs inspect` writes: the cheap pre-extraction look.
#[derive(Debug, Serialize)]
pub struct InspectOutput {
    pub schema_version: &'static str,
    pub engine: String,
    pub engine_version: String,
    pub page_count: u32,
    pub page_kind: PageKind,
    pub metadata: Metadata,
    /// Per-page classification with empty block lists. 1-indexed by `number`.
    pub pages: Vec<Page>,
    pub duration_ms: u64,
}

/// What `dolico-rs extract` writes.
#[derive(Debug, Serialize)]
pub struct ExtractOutput {
    pub schema_version: &'static str,
    pub engine: String,
    pub engine_version: String,
    pub metadata: Metadata,
    pub pages: Vec<Page>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub assets: Vec<Asset>,
    pub duration_ms: u64,
}

/// The failure envelope written to stdout on error, so the orchestrator gets
/// something structured instead of having to scrape a message.
#[derive(Debug, Serialize)]
pub struct ErrorOutput {
    pub schema_version: &'static str,
    /// Machine-readable: `unsupported`, `malformed`, `encrypted`,
    /// `resource_limit`, `io`, `internal`.
    pub kind: &'static str,
    pub message: String,
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

#[derive(Debug, Default, Serialize)]
pub struct Metadata {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub title: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub author: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub language: Option<String>,
    pub page_count: u32,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub custom: Option<std::collections::BTreeMap<String, String>>,
}

/// The full set is the schema's, not this binary's. `Slide` and `Sheet` are
/// unreachable here because anydoc's document model is a flat block flow with
/// no slide or sheet boundaries -- see the module docs in `native.rs`. They
/// stay because the schema defines them and another engine may yet produce
/// them.
#[allow(dead_code)]
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum PageKind {
    Paginated,
    Slide,
    Sheet,
    Section,
}

/// `Mixed` is a document-level classification in pdf-inspector; per page a PDF
/// resolves to text-based or scanned, so the shim never emits it. The router
/// may.
#[allow(dead_code)]
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum PageType {
    TextBased,
    Scanned,
    ImageBased,
    Mixed,
    Native,
}

#[derive(Debug, Serialize)]
pub struct Classification {
    #[serde(rename = "type")]
    pub kind: PageType,
    pub confidence: f64,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub reasons: Vec<String>,
}

#[derive(Debug, Serialize)]
pub struct Page {
    /// Always 1-indexed. pdf-inspector mixes 0- and 1-indexed page numbers
    /// across its API; every one of them is normalized here.
    pub number: u32,
    pub kind: PageKind,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub width: Option<f64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub height: Option<f64>,
    pub classification: Classification,
    pub blocks: Vec<Block>,
}

/// `Formula` has no producer yet: neither anydoc nor pdf-inspector detects
/// mathematical content. It is reserved for the OCR tier (PP-StructureV3 does
/// emit formulas) and kept here so the schema and its mirrors stay identical.
#[allow(dead_code)]
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum BlockType {
    Heading,
    Paragraph,
    List,
    Table,
    Image,
    Code,
    Quote,
    Rule,
    Formula,
}

#[derive(Debug, Serialize)]
pub struct Block {
    pub id: String,
    #[serde(rename = "type")]
    pub kind: BlockType,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub text: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub level: Option<u8>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub lang: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub list: Option<List>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub table: Option<Table>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub quote: Option<Vec<Block>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub inline: Option<Vec<Span>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub asset_ref: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub alt: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub bbox: Option<BBox>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub confidence: Option<f64>,
    pub provenance: Provenance,
}

impl Block {
    /// A block of `kind` with everything optional left unset.
    pub fn new(id: String, kind: BlockType, prov: Provenance) -> Block {
        Block {
            id,
            kind,
            text: None,
            level: None,
            lang: None,
            list: None,
            table: None,
            quote: None,
            inline: None,
            asset_ref: None,
            alt: None,
            bbox: None,
            confidence: None,
            provenance: prov,
        }
    }
}

#[derive(Debug, Default, Serialize)]
pub struct Span {
    pub text: String,
    #[serde(skip_serializing_if = "is_false")]
    pub bold: bool,
    #[serde(skip_serializing_if = "is_false")]
    pub italic: bool,
    #[serde(skip_serializing_if = "is_false")]
    pub strike: bool,
    #[serde(skip_serializing_if = "is_false")]
    pub code: bool,
    #[serde(skip_serializing_if = "is_false")]
    pub underline: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub href: Option<String>,
}

fn is_false(b: &bool) -> bool {
    !*b
}

#[derive(Debug, Serialize)]
pub struct List {
    pub ordered: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub marker: Option<&'static str>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub start: Option<u64>,
    pub items: Vec<ListItem>,
}

#[derive(Debug, Serialize)]
pub struct ListItem {
    pub blocks: Vec<Block>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub checked: Option<bool>,
}

#[derive(Debug, Serialize)]
pub struct Table {
    pub header_rows: usize,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub kind: Option<&'static str>,
    pub grid: Vec<Vec<Cell>>,
}

#[derive(Debug, Default, Serialize)]
pub struct Cell {
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub blocks: Vec<Block>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub row_span: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub col_span: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub covered_by: Option<CellRef>,
}

#[derive(Debug, Serialize)]
pub struct CellRef {
    pub row: usize,
    pub col: usize,
}

/// PDF user space: origin bottom-left, units are points.
#[derive(Debug, Clone, Copy, Serialize)]
pub struct BBox {
    pub x: f64,
    pub y: f64,
    pub width: f64,
    pub height: f64,
}

#[derive(Debug, Clone, Serialize)]
pub struct Provenance {
    pub engine: String,
    pub engine_version: String,
    pub method: String,
}

impl Provenance {
    pub fn new(engine: &str, version: &str, method: &str) -> Provenance {
        Provenance {
            engine: engine.to_string(),
            engine_version: version.to_string(),
            method: method.to_string(),
        }
    }
}

#[derive(Debug, Serialize)]
pub struct Asset {
    pub id: String,
    pub media_type: String,
    /// Filename relative to `--assets-dir`. The orchestrator moves the bytes
    /// into the blob store and rewrites this to a blob reference.
    pub blob_ref: String,
    pub size_bytes: u64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub origin: Option<String>,
}

// ---------------------------------------------------------------------------
// Block id allocation
// ---------------------------------------------------------------------------

/// Allocates stable, position-derived block ids of the form `p3-b12`.
///
/// Ids are derived from position rather than from content so that they stay
/// stable across engine upgrades that change text slightly, and so golden test
/// diffs point at the block that actually moved.
pub struct IdGen {
    page: u32,
    next: u32,
}

impl IdGen {
    pub fn new(page: u32) -> IdGen {
        IdGen { page, next: 0 }
    }

    pub fn next(&mut self) -> String {
        let id = format!("p{}-b{}", self.page, self.next);
        self.next += 1;
        id
    }
}
