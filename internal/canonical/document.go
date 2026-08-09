// Package canonical defines the canonical document model: the single
// representation every extraction engine normalizes into, and the only thing
// downstream consumers (Markdown rendering, chunking, indexing) read.
//
// Markdown is not the internal representation. It is generated from this model
// by internal/render as one view among several.
//
// This model is mirrored in rust/dolico-rs/src/canonical.rs and specified in
// schema/canonical-v1.json. The three must agree; TestCanonicalMatchesSchema
// and the golden tests are what enforce that.
package canonical

// SchemaVersion is the version of the canonical model itself. It is
// deliberately independent of engine versions and of PipelineVersion: an
// engine upgrade that produces better blocks does not change the schema, and a
// schema change must be visible to consumers even when no engine moved.
const SchemaVersion = "1.0"

// PipelineVersion identifies the orchestration behavior (routing rules,
// escalation thresholds, block assembly). It participates in cache keys, so
// bumping it invalidates cached page results without touching engine versions.
// Bumped to 2 when per-page quality began scoring a measured extraction by its
// measured confidence: the same bytes through the same engines can now escalate
// differently, so cached pages from version 1 are not the pages this pipeline
// would produce.
const PipelineVersion = "2"

// Document is the root of the canonical model.
type Document struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	Source        Source   `json:"source"`
	Metadata      Metadata `json:"metadata"`
	Pages         []Page   `json:"pages"`
	Assets        []Asset  `json:"assets,omitempty"`
	Trace         Trace    `json:"trace"`
}

// Source describes the bytes that were ingested.
type Source struct {
	Filename  string `json:"filename"`
	MediaType string `json:"media_type"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// Metadata holds document-level descriptive fields. Everything is optional:
// most formats supply only some of it.
type Metadata struct {
	Title     string            `json:"title,omitempty"`
	Author    string            `json:"author,omitempty"`
	Language  string            `json:"language,omitempty"`
	PageCount int               `json:"page_count"`
	Custom    map[string]string `json:"custom,omitempty"`
}

// PageKind records what a "page" means for this source. PDFs paginate; DOCX,
// Markdown and CSV do not, and inventing page geometry for them would be a
// lie. Consumers that need real geometry test BBox for nil rather than
// trusting a fabricated rectangle.
type PageKind string

const (
	// PageKindPaginated is a real, rendered page with geometry (PDF).
	PageKindPaginated PageKind = "paginated"
	// PageKindSlide is one slide of a presentation.
	PageKindSlide PageKind = "slide"
	// PageKindSheet is one worksheet of a spreadsheet.
	PageKindSheet PageKind = "sheet"
	// PageKindSection is a synthetic division of a flow document that has no
	// pagination of its own.
	PageKindSection PageKind = "section"
)

// Page is one unit of a document, as defined by PageKind.
type Page struct {
	Number         int            `json:"number"` // 1-indexed, always
	Kind           PageKind       `json:"kind"`
	Width          *float64       `json:"width,omitempty"`  // points
	Height         *float64       `json:"height,omitempty"` // points
	Classification Classification `json:"classification"`
	Blocks         []Block        `json:"blocks"`
	Quality        *Quality       `json:"quality,omitempty"`
}

// PageType is how a page was classified for routing purposes.
type PageType string

const (
	PageTypeTextBased  PageType = "text_based"
	PageTypeScanned    PageType = "scanned"
	PageTypeImageBased PageType = "image_based"
	PageTypeMixed      PageType = "mixed"
	// PageTypeNative is a page from a natively-parsed format, where the
	// text/scanned distinction does not apply at all.
	PageTypeNative PageType = "native"
)

// Classification is the routing decision input for a single page.
type Classification struct {
	Type       PageType `json:"type"`
	Confidence float64  `json:"confidence"` // 0..1
	// Reasons are machine-readable identifiers explaining the classification,
	// e.g. "no_text_operators", "gid_encoded_fonts". Passed through from the
	// engine so a human debugging a bad route can see why.
	Reasons []string `json:"reasons,omitempty"`
}

// BlockType enumerates the block kinds the model can represent.
type BlockType string

const (
	BlockHeading   BlockType = "heading"
	BlockParagraph BlockType = "paragraph"
	BlockList      BlockType = "list"
	BlockTable     BlockType = "table"
	BlockImage     BlockType = "image"
	BlockCode      BlockType = "code"
	BlockQuote     BlockType = "quote"
	BlockRule      BlockType = "rule"
	BlockFormula   BlockType = "formula"
)

// Block is one block-level piece of page content. Blocks nest: quotes contain
// blocks, list items contain blocks, table cells contain blocks.
type Block struct {
	ID   string    `json:"id"`
	Type BlockType `json:"type"`

	// Text is the block's plain text, present for heading, paragraph, code and
	// formula. Container blocks (list, table, quote) carry their content in
	// the dedicated field instead.
	Text string `json:"text,omitempty"`

	Level int    `json:"level,omitempty"` // heading depth, 1-based
	Lang  string `json:"lang,omitempty"`  // code block language hint

	List   *List   `json:"list,omitempty"`
	Table  *Table  `json:"table,omitempty"`
	Quote  []Block `json:"quote,omitempty"`
	Inline []Span  `json:"inline,omitempty"` // styled runs, when style was preserved

	AssetRef string `json:"asset_ref,omitempty"` // Asset.ID for image blocks
	Alt      string `json:"alt,omitempty"`

	// BBox is nil wherever the source has no geometry, which is every
	// non-PDF format and any PDF block whose text could not be matched back
	// to positioned glyphs. Never fabricated.
	BBox *BBox `json:"bbox,omitempty"`

	// Confidence is nil when the engine reports none. A native parser reading
	// DOCX XML is not "95% confident"; it is reading a data structure.
	Confidence *float64   `json:"confidence,omitempty"`
	Provenance Provenance `json:"provenance"`
}

// Span is a styled run of inline text.
type Span struct {
	Text      string `json:"text"`
	Bold      bool   `json:"bold,omitempty"`
	Italic    bool   `json:"italic,omitempty"`
	Strike    bool   `json:"strike,omitempty"`
	Code      bool   `json:"code,omitempty"`
	Underline bool   `json:"underline,omitempty"`
	Href      string `json:"href,omitempty"`
}

// List is an ordered or unordered list with its numbering already resolved.
type List struct {
	Ordered bool       `json:"ordered"`
	Marker  string     `json:"marker,omitempty"` // bullet, decimal, lower_alpha, ...
	Start   int        `json:"start,omitempty"`
	Items   []ListItem `json:"items"`
}

// ListItem is one entry of a List; it may nest further blocks.
type ListItem struct {
	Blocks  []Block `json:"blocks"`
	Checked *bool   `json:"checked,omitempty"` // task-list state, nil if not a task
}

// Table is a canonical grid. Rows may be ragged; merged cells are represented
// by an origin cell carrying spans plus Covered shadow slots pointing back at
// it, so grid[r][c] is always addressable.
type Table struct {
	HeaderRows int      `json:"header_rows"`
	Kind       string   `json:"kind,omitempty"` // "data" or "layout"
	Grid       [][]Cell `json:"grid"`
}

// Cell is one slot of a Table grid: either an origin cell holding content and
// span extents, or the shadow of a cell that spans into this position.
type Cell struct {
	Blocks  []Block  `json:"blocks,omitempty"`
	RowSpan int      `json:"row_span,omitempty"`
	ColSpan int      `json:"col_span,omitempty"`
	Covered *CellRef `json:"covered_by,omitempty"` // non-nil => shadow slot
}

// CellRef points at the origin cell that covers a shadow slot.
type CellRef struct {
	Row int `json:"row"`
	Col int `json:"col"`
}

// BBox is a rectangle in PDF user space: origin bottom-left, units are points.
type BBox struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// Provenance records which engine produced a block and how. This is what makes
// a mixed-engine document debuggable: every block says where it came from.
type Provenance struct {
	Engine        string `json:"engine"`
	EngineVersion string `json:"engine_version"`
	// Method is the specific code path, e.g. "anydoc/document-model",
	// "pdf-inspector/page-markdown", "ocr-stub/synthetic". It also carries the
	// reason a BBox is absent.
	Method string `json:"method"`
}

// Asset is an embedded binary the document referenced.
type Asset struct {
	ID        string `json:"id"`
	MediaType string `json:"media_type"`
	// BlobRef locates the bytes in the blob store. Bytes are never inlined
	// into the canonical JSON.
	BlobRef   string `json:"blob_ref"`
	SizeBytes int64  `json:"size_bytes"`
	Origin    string `json:"origin,omitempty"` // source part/stream, for provenance
}

// Quality is a per-page assessment computed by internal/engine/quality. It is
// deliberately not the engine's own confidence: engines are optimistic about
// their own output, and the whole point of scoring is to catch the cases where
// the engine is wrong. Engine confidence is one input among several.
type Quality struct {
	Score            float64  `json:"score"` // 0..1, the escalation input
	CharCount        int      `json:"char_count"`
	ReplacementRatio float64  `json:"replacement_ratio"` // share of U+FFFD
	WordRatio        float64  `json:"word_ratio"`        // share of plausible words
	EngineConfidence *float64 `json:"engine_confidence,omitempty"`
	// MeasuredConfidence is the length-weighted mean of the per-block
	// confidences on this page, present only when blocks reported any. It is
	// separate from EngineConfidence because the two mean different things: a
	// parser asserting certainty about a data structure it read, versus an
	// engine reporting how sure it is of characters it recognized.
	MeasuredConfidence *float64 `json:"measured_confidence,omitempty"`
	// MeasuredCoverage is the share of the page's characters that came from
	// blocks reporting a confidence. A mean over a sliver of a page is not a
	// statement about the page, so the score only uses it above a threshold.
	MeasuredCoverage float64 `json:"measured_coverage,omitempty"`
	// Escalated records that this page was re-extracted by another engine
	// because the first attempt scored below threshold.
	Escalated bool `json:"escalated,omitempty"`
}

// Trace carries the debugging provenance for the run as a whole.
type Trace struct {
	TraceID         string      `json:"trace_id"`
	PipelineVersion string      `json:"pipeline_version"`
	Engines         []EngineRun `json:"engines"`
}

// EngineRun is one engine invocation within a document's processing.
type EngineRun struct {
	Engine     string `json:"engine"`
	Version    string `json:"version"`
	Pages      []int  `json:"pages,omitempty"` // 1-indexed; empty means whole document
	DurationMS int64  `json:"duration_ms"`
	CacheHit   bool   `json:"cache_hit,omitempty"`
	Error      string `json:"error,omitempty"`
}
