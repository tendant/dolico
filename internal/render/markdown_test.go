package render

import (
	"strings"
	"testing"

	"github.com/tendant/dolico/internal/canonical"
)

func doc(pages ...canonical.Page) *canonical.Document {
	return &canonical.Document{
		SchemaVersion: canonical.SchemaVersion,
		ID:            "d",
		Pages:         pages,
	}
}

func paginated(blocks ...canonical.Block) canonical.Page {
	return canonical.Page{Number: 1, Kind: canonical.PageKindPaginated, Blocks: blocks}
}

func section(blocks ...canonical.Block) canonical.Page {
	return canonical.Page{Number: 1, Kind: canonical.PageKindSection, Blocks: blocks}
}

func block(t canonical.BlockType, text string) canonical.Block {
	return canonical.Block{ID: "b", Type: t, Text: text}
}

func TestHeadingLevels(t *testing.T) {
	h := block(canonical.BlockHeading, "Title")
	h.Level = 3
	got := Markdown(doc(section(h)))
	if !strings.Contains(got, "### Title") {
		t.Errorf("got %q", got)
	}
}

// Word outline levels run past 6; Markdown does not.
func TestHeadingLevelIsClamped(t *testing.T) {
	deep := block(canonical.BlockHeading, "Deep")
	deep.Level = 11
	if got := Markdown(doc(section(deep))); !strings.Contains(got, "###### Deep") {
		t.Errorf("got %q", got)
	}
	zero := block(canonical.BlockHeading, "Zero")
	zero.Level = 0
	if got := Markdown(doc(section(zero))); !strings.Contains(got, "# Zero") {
		t.Errorf("got %q", got)
	}
}

func TestInlineStyling(t *testing.T) {
	b := block(canonical.BlockParagraph, "bold italic code link")
	b.Inline = []canonical.Span{
		{Text: "bold", Bold: true},
		{Text: " "},
		{Text: "italic", Italic: true},
		{Text: " "},
		{Text: "code", Code: true},
		{Text: " "},
		{Text: "link", Href: "http://example.com"},
	}
	got := Markdown(doc(section(b)))
	for _, want := range []string{"**bold**", "*italic*", "`code`", "[link](http://example.com)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestPlainTextIsEscaped(t *testing.T) {
	got := Markdown(doc(section(block(canonical.BlockParagraph, "a*b_c[d]"))))
	if strings.Contains(got, "a*b") {
		t.Errorf("markup characters were not escaped: %q", got)
	}
	if !strings.Contains(got, `a\*b\_c\[d\]`) {
		t.Errorf("got %q", got)
	}
}

func TestCodeFenceOutgrowsInnerBackticks(t *testing.T) {
	b := block(canonical.BlockCode, "print(\"```\")")
	b.Lang = "python"
	got := Markdown(doc(section(b)))
	if !strings.Contains(got, "````python") {
		t.Errorf("fence should be longer than the inner backtick run: %q", got)
	}
	// The content must survive intact rather than terminating the block early.
	if !strings.Contains(got, "print(\"```\")") {
		t.Errorf("code body was mangled: %q", got)
	}
}

func TestCodeIsNotEscaped(t *testing.T) {
	got := Markdown(doc(section(block(canonical.BlockCode, "x := a*b"))))
	if !strings.Contains(got, "x := a*b") {
		t.Errorf("code should be verbatim: %q", got)
	}
}

func TestOrderedListRespectsStart(t *testing.T) {
	item := func(s string) canonical.ListItem {
		return canonical.ListItem{Blocks: []canonical.Block{block(canonical.BlockParagraph, s)}}
	}
	b := canonical.Block{ID: "l", Type: canonical.BlockList, List: &canonical.List{
		Ordered: true, Start: 3, Items: []canonical.ListItem{item("three"), item("four")},
	}}
	got := Markdown(doc(section(b)))
	if !strings.Contains(got, "3. three") || !strings.Contains(got, "4. four") {
		t.Errorf("got %q", got)
	}
}

func TestTaskListCheckboxes(t *testing.T) {
	yes, no := true, false
	b := canonical.Block{ID: "l", Type: canonical.BlockList, List: &canonical.List{
		Items: []canonical.ListItem{
			{Blocks: []canonical.Block{block(canonical.BlockParagraph, "done")}, Checked: &yes},
			{Blocks: []canonical.Block{block(canonical.BlockParagraph, "todo")}, Checked: &no},
		},
	}}
	got := Markdown(doc(section(b)))
	if !strings.Contains(got, "- [x] done") || !strings.Contains(got, "- [ ] todo") {
		t.Errorf("got %q", got)
	}
}

func TestNestedListIndents(t *testing.T) {
	inner := canonical.Block{ID: "il", Type: canonical.BlockList, List: &canonical.List{
		Items: []canonical.ListItem{{Blocks: []canonical.Block{block(canonical.BlockParagraph, "inner")}}},
	}}
	outer := canonical.Block{ID: "ol", Type: canonical.BlockList, List: &canonical.List{
		Items: []canonical.ListItem{{Blocks: []canonical.Block{
			block(canonical.BlockParagraph, "outer"), inner,
		}}},
	}}
	got := Markdown(doc(section(outer)))
	if !strings.Contains(got, "- outer") || !strings.Contains(got, "  - inner") {
		t.Errorf("nested list not indented: %q", got)
	}
}

func TestTableRendersWithHeaderAndSeparator(t *testing.T) {
	cell := func(s string) canonical.Cell {
		return canonical.Cell{Blocks: []canonical.Block{block(canonical.BlockParagraph, s)}}
	}
	b := canonical.Block{ID: "t", Type: canonical.BlockTable, Table: &canonical.Table{
		HeaderRows: 1,
		Grid: [][]canonical.Cell{
			{cell("Format"), cell("Engine")},
			{cell("DOCX"), cell("anydoc")},
		},
	}}
	got := Markdown(doc(section(b)))
	for _, want := range []string{"| Format | Engine |", "| --- | --- |", "| DOCX | anydoc |"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

// GFM has no way to express a table without a header, but degrading the table
// to paragraphs would lose more.
func TestHeaderlessTableStillRendersAsATable(t *testing.T) {
	cell := func(s string) canonical.Cell {
		return canonical.Cell{Blocks: []canonical.Block{block(canonical.BlockParagraph, s)}}
	}
	b := canonical.Block{ID: "t", Type: canonical.BlockTable, Table: &canonical.Table{
		HeaderRows: 0,
		Grid:       [][]canonical.Cell{{cell("a"), cell("b")}},
	}}
	got := Markdown(doc(section(b)))
	if !strings.Contains(got, "| --- | --- |") || !strings.Contains(got, "| a | b |") {
		t.Errorf("got %q", got)
	}
}

func TestCellContentCannotBreakOutOfTheTable(t *testing.T) {
	cell := canonical.Cell{Blocks: []canonical.Block{
		block(canonical.BlockParagraph, "line one\nline two"),
	}}
	b := canonical.Block{ID: "t", Type: canonical.BlockTable, Table: &canonical.Table{
		HeaderRows: 1, Grid: [][]canonical.Cell{{cell}},
	}}
	got := Markdown(doc(section(b)))
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(strings.TrimSpace(line), "|") {
			t.Errorf("cell content escaped the table: %q in %q", line, got)
		}
	}
}

func TestQuoteIsPrefixedOnEveryLine(t *testing.T) {
	b := canonical.Block{ID: "q", Type: canonical.BlockQuote, Quote: []canonical.Block{
		block(canonical.BlockParagraph, "first"),
		block(canonical.BlockParagraph, "second"),
	}}
	got := Markdown(doc(section(b)))
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, ">") {
			t.Errorf("unquoted line %q in %q", line, got)
		}
	}
}

func TestImageBlock(t *testing.T) {
	b := canonical.Block{ID: "i", Type: canonical.BlockImage, Alt: "a chart", AssetRef: "a0"}
	if got := Markdown(doc(section(b))); !strings.Contains(got, "![a chart](a0)") {
		t.Errorf("got %q", got)
	}
}

// Page breaks are only meaningful where pages are real.
func TestPageBreakOnlyBetweenPaginatedPages(t *testing.T) {
	p1 := paginated(block(canonical.BlockParagraph, "one"))
	p2 := canonical.Page{Number: 2, Kind: canonical.PageKindPaginated,
		Blocks: []canonical.Block{block(canonical.BlockParagraph, "two")}}
	if got := Markdown(doc(p1, p2)); !strings.Contains(got, "\n---\n") {
		t.Errorf("expected a rule between PDF pages: %q", got)
	}

	s1 := section(block(canonical.BlockParagraph, "one"))
	s2 := canonical.Page{Number: 2, Kind: canonical.PageKindSection,
		Blocks: []canonical.Block{block(canonical.BlockParagraph, "two")}}
	if got := Markdown(doc(s1, s2)); strings.Contains(got, "\n---\n") {
		t.Errorf("synthetic sections should not get a page rule: %q", got)
	}
}

func TestTitleBecomesTopHeading(t *testing.T) {
	d := doc(section(block(canonical.BlockParagraph, "body")))
	d.Metadata.Title = "My Report"
	if got := Markdown(d); !strings.HasPrefix(got, "# My Report") {
		t.Errorf("got %q", got)
	}
}

func TestEmptyDocumentRendersEmpty(t *testing.T) {
	if got := Markdown(doc()); strings.TrimSpace(got) != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestNoRunsOfBlankLines(t *testing.T) {
	d := doc(section(
		block(canonical.BlockParagraph, ""),
		block(canonical.BlockParagraph, "text"),
		block(canonical.BlockParagraph, ""),
		block(canonical.BlockParagraph, "more"),
	))
	if got := Markdown(d); strings.Contains(got, "\n\n\n") {
		t.Errorf("consecutive blank lines in %q", got)
	}
}
