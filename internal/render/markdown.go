// Package render generates views of a canonical document.
//
// Markdown is a *view*. The canonical JSON is the representation; this package
// is a pure function over it with no knowledge of engines, formats or PDFs. If
// rendering needed to know where a block came from, the canonical model would
// be leaking extraction detail it should have normalized away.
package render

import (
	"fmt"
	"strings"

	"github.com/tendant/dolico/internal/canonical"
)

// Markdown renders a document as GitHub-flavored Markdown.
func Markdown(doc *canonical.Document) string {
	var b strings.Builder
	if doc.Metadata.Title != "" {
		fmt.Fprintf(&b, "# %s\n\n", escape(doc.Metadata.Title))
	}
	for i, page := range doc.Pages {
		// Page breaks are only meaningful where pages are real. Emitting a
		// rule between the synthetic sections of a DOCX would invent structure
		// the source does not have.
		if i > 0 && page.Kind == canonical.PageKindPaginated {
			b.WriteString("\n---\n\n")
		}
		writeBlocks(&b, page.Blocks, 0)
	}
	return strings.TrimLeft(collapseBlankLines(b.String()), "\n")
}

func writeBlocks(b *strings.Builder, blocks []canonical.Block, depth int) {
	for _, blk := range blocks {
		writeBlock(b, blk, depth)
	}
}

func writeBlock(b *strings.Builder, blk canonical.Block, depth int) {
	switch blk.Type {
	case canonical.BlockHeading:
		// Word outline levels run past 6; Markdown stops there.
		level := min(max(blk.Level, 1), 6)
		fmt.Fprintf(b, "%s %s\n\n", strings.Repeat("#", level), inlineText(blk))

	case canonical.BlockParagraph:
		if text := inlineText(blk); text != "" {
			b.WriteString(text)
			b.WriteString("\n\n")
		}

	case canonical.BlockCode:
		lang := blk.Lang
		// A fence must be longer than the longest backtick run inside it, or
		// code containing a fence terminates the block early.
		fence := strings.Repeat("`", max(3, longestBacktickRun(blk.Text)+1))
		fmt.Fprintf(b, "%s%s\n%s\n%s\n\n", fence, lang, blk.Text, fence)

	case canonical.BlockQuote:
		var inner strings.Builder
		writeBlocks(&inner, blk.Quote, depth)
		for line := range strings.SplitSeq(strings.TrimRight(inner.String(), "\n"), "\n") {
			if line == "" {
				b.WriteString(">\n")
				continue
			}
			b.WriteString("> ")
			b.WriteString(line)
			b.WriteByte('\n')
		}
		b.WriteString("\n")

	case canonical.BlockRule:
		b.WriteString("---\n\n")

	case canonical.BlockImage:
		alt := blk.Alt
		if alt == "" {
			alt = "image"
		}
		ref := blk.AssetRef
		if ref == "" {
			ref = "#"
		}
		fmt.Fprintf(b, "![%s](%s)\n\n", escape(alt), ref)

	case canonical.BlockFormula:
		fmt.Fprintf(b, "$$\n%s\n$$\n\n", blk.Text)

	case canonical.BlockList:
		if blk.List == nil {
			return
		}
		writeList(b, blk.List, depth)
		if depth == 0 {
			b.WriteString("\n")
		}

	case canonical.BlockTable:
		if blk.Table == nil {
			return
		}
		writeTable(b, blk.Table)
	}
}

func writeList(b *strings.Builder, list *canonical.List, depth int) {
	indent := strings.Repeat("  ", depth)
	n := max(list.Start, 1)
	for _, item := range list.Items {
		marker := "-"
		if list.Ordered {
			marker = fmt.Sprintf("%d.", n)
			n++
		}
		if item.Checked != nil {
			box := "[ ]"
			if *item.Checked {
				box = "[x]"
			}
			marker += " " + box
		}

		// The item's first paragraph goes on the marker line; anything else
		// nests underneath it.
		var head string
		rest := item.Blocks
		if len(rest) > 0 && (rest[0].Type == canonical.BlockParagraph || rest[0].Type == canonical.BlockHeading) {
			head = inlineText(rest[0])
			rest = rest[1:]
		}
		fmt.Fprintf(b, "%s%s %s\n", indent, marker, head)

		for _, sub := range rest {
			if sub.Type == canonical.BlockList && sub.List != nil {
				writeList(b, sub.List, depth+1)
				continue
			}
			var nested strings.Builder
			writeBlock(&nested, sub, depth+1)
			for line := range strings.SplitSeq(strings.TrimRight(nested.String(), "\n"), "\n") {
				if line == "" {
					b.WriteByte('\n')
					continue
				}
				fmt.Fprintf(b, "%s  %s\n", indent, line)
			}
		}
	}
}

// writeTable renders the canonical grid as a GFM pipe table.
//
// Markdown has no merged cells. A cell spanning several columns is written
// once in its origin position and its shadow slots render empty, which is the
// closest lossless-in-appearance thing the format allows; the spans are still
// in the JSON for consumers that can use them.
func writeTable(b *strings.Builder, table *canonical.Table) {
	if len(table.Grid) == 0 {
		return
	}
	width := 0
	for _, row := range table.Grid {
		if len(row) > width {
			width = len(row)
		}
	}
	if width == 0 {
		return
	}

	cellText := func(row []canonical.Cell, i int) string {
		if i >= len(row) {
			return ""
		}
		var inner strings.Builder
		writeBlocks(&inner, row[i].Blocks, 0)
		// Newlines and pipes would break out of the cell.
		text := strings.TrimSpace(inner.String())
		text = strings.ReplaceAll(text, "\n", " ")
		text = strings.ReplaceAll(text, "|", "\\|")
		return strings.Join(strings.Fields(text), " ")
	}

	writeRow := func(row []canonical.Cell) {
		b.WriteString("|")
		for i := 0; i < width; i++ {
			b.WriteString(" ")
			b.WriteString(cellText(row, i))
			b.WriteString(" |")
		}
		b.WriteByte('\n')
	}

	headerRows := min(table.HeaderRows, len(table.Grid))

	if headerRows == 0 {
		// GFM requires a header row. An empty one keeps the table a table
		// rather than degrading it to paragraphs.
		b.WriteString("|" + strings.Repeat("  |", width) + "\n")
	}
	for i := 0; i < headerRows; i++ {
		writeRow(table.Grid[i])
	}
	b.WriteString("|" + strings.Repeat(" --- |", width) + "\n")
	for i := headerRows; i < len(table.Grid); i++ {
		writeRow(table.Grid[i])
	}
	b.WriteString("\n")
}

// inlineText renders a block's text, applying inline styling when the block
// carries spans and falling back to the plain text when it does not.
func inlineText(blk canonical.Block) string {
	if len(blk.Inline) == 0 {
		return escape(blk.Text)
	}
	var b strings.Builder
	for _, span := range blk.Inline {
		text := span.Text
		if text == "" {
			continue
		}
		if span.Code {
			// Nothing nests inside code.
			b.WriteString("`" + text + "`")
			continue
		}
		text = escape(text)
		if span.Strike {
			text = "~~" + text + "~~"
		}
		if span.Bold {
			text = "**" + text + "**"
		}
		if span.Italic {
			text = "*" + text + "*"
		}
		if span.Href != "" {
			text = "[" + text + "](" + span.Href + ")"
		}
		b.WriteString(text)
	}
	return b.String()
}

// escape neutralizes the characters that would otherwise be read as markup.
// Only the ones that actually cause trouble mid-text are escaped: escaping
// every punctuation mark makes the output unreadable for no benefit.
var escaper = strings.NewReplacer(
	`\`, `\\`,
	"`", "\\`",
	"*", `\*`,
	"_", `\_`,
	"[", `\[`,
	"]", `\]`,
	"<", `\<`,
	"|", `\|`,
)

func escape(s string) string { return escaper.Replace(s) }

func longestBacktickRun(s string) int {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
			continue
		}
		run = 0
	}
	return longest
}

// collapseBlankLines squeezes runs of blank lines down to one, so that empty
// blocks and nesting do not leave gaps through the output.
func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			blank++
			if blank > 1 {
				continue
			}
			out = append(out, "")
			continue
		}
		blank = 0
		out = append(out, line)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
}
