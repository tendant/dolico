// Package quality scores an extracted page.
//
// The score deliberately is not the engine's own confidence. Engines are
// optimistic about their output -- a PDF with a broken font encoding extracts
// "text" with complete confidence and produces mojibake -- and the entire
// point of scoring is to catch the cases where the engine is wrong. Engine
// confidence is one input among several, and the weakest one.
//
// The router uses the score to decide whether to escalate a page to the next
// extraction tier.
package quality

import (
	"strings"
	"unicode"

	"github.com/tendant/dolico/internal/canonical"
)

// Weights control how the signals combine. They are named rather than inlined
// because they are guesses that want tuning against a real corpus, and a
// benchmark harness should be able to sweep them.
type Weights struct {
	Density     float64 // text was actually recovered
	Replacement float64 // absence of U+FFFD and control junk
	Words       float64 // output looks like language, not glyph soup
	Engine      float64 // what the engine thought
}

// DefaultWeights favors the signals computed from the text itself over the
// engine's self-report.
var DefaultWeights = Weights{
	Density:     0.30,
	Replacement: 0.25,
	Words:       0.30,
	Engine:      0.15,
}

// ExpectedChars is the character count at which the density signal saturates.
// A page with this much text has clearly extracted; less is proportionally
// more suspicious. Sparse pages -- a title page, a chapter divider -- are the
// known false positive, which is why density is only part of the score.
const ExpectedChars = 400

// Score assesses a page and returns the quality record to attach to it.
func Score(page *canonical.Page, w Weights) canonical.Quality {
	text := PlainText(page)
	runes := []rune(text)

	q := canonical.Quality{
		CharCount:        len(runes),
		ReplacementRatio: replacementRatio(runes),
		WordRatio:        wordRatio(text),
	}
	if page.Classification.Confidence > 0 || page.Classification.Type != canonical.PageTypeScanned {
		c := page.Classification.Confidence
		q.EngineConfidence = &c
	}

	// An image-only page legitimately has no text. Scoring it on density would
	// mark it as terrible extraction when the truth is there was nothing to
	// extract -- and it has already been classified for OCR by other means.
	if len(runes) == 0 {
		q.Score = 0
		return q
	}

	density := float64(len(runes)) / ExpectedChars
	if density > 1 {
		density = 1
	}
	engineConf := 0.0
	if q.EngineConfidence != nil {
		engineConf = *q.EngineConfidence
	}

	total := w.Density + w.Replacement + w.Words + w.Engine
	if total <= 0 {
		total = 1
	}
	q.Score = (w.Density*density +
		w.Replacement*(1-q.ReplacementRatio) +
		w.Words*q.WordRatio +
		w.Engine*engineConf) / total
	q.Score = clamp01(q.Score)
	return q
}

// PlainText concatenates every piece of text on a page, walking into lists,
// tables and quotes so that a page whose content is entirely inside a table
// is not scored as empty.
func PlainText(page *canonical.Page) string {
	var b strings.Builder
	writeBlocks(&b, page.Blocks)
	return b.String()
}

func writeBlocks(b *strings.Builder, blocks []canonical.Block) {
	for _, blk := range blocks {
		if blk.Text != "" {
			b.WriteString(blk.Text)
			b.WriteByte('\n')
		}
		writeBlocks(b, blk.Quote)
		if blk.List != nil {
			for _, item := range blk.List.Items {
				writeBlocks(b, item.Blocks)
			}
		}
		if blk.Table != nil {
			for _, row := range blk.Table.Grid {
				for _, cell := range row {
					writeBlocks(b, cell.Blocks)
				}
			}
		}
	}
}

// replacementRatio is the share of characters that are decoding failures. A
// broken CID font produces these in quantity, and they are the clearest signal
// that "successful" text extraction actually failed.
func replacementRatio(runes []rune) float64 {
	if len(runes) == 0 {
		return 0
	}
	bad := 0
	for _, r := range runes {
		if isDamage(r) {
			bad++
		}
	}
	return float64(bad) / float64(len(runes))
}

// wordRatio is the share of whitespace-separated tokens that look like words
// rather than glyph soup.
//
// This is intentionally language-agnostic: a token counts when it is mostly
// letters or is a plausible number. A real dictionary check would be better
// for English and worse for everything else, and OCR output in an unexpected
// script must not be scored as garbage merely for being unexpected.
func wordRatio(text string) float64 {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return 0
	}
	good := 0
	for _, f := range fields {
		if plausibleToken(f) {
			good++
		}
	}
	return float64(good) / float64(len(fields))
}

func plausibleToken(tok string) bool {
	var letters, digits, damage, punct int
	for _, r := range tok {
		switch {
		case isDamage(r):
			// Checked before the symbol case on purpose: U+FFFD is a Unicode
			// symbol, so classifying by category alone would score a token of
			// pure replacement characters as a perfectly good word -- exactly
			// the failure this scorer exists to catch.
			damage++
		case unicode.IsLetter(r):
			letters++
		case unicode.IsDigit(r):
			digits++
		case unicode.IsPunct(r) || unicode.IsSpace(r) || unicode.IsSymbol(r):
			// Punctuation attached to a word is normal.
			punct++
		default:
			damage++
		}
	}
	if damage > 0 {
		return false
	}
	if letters == 0 && digits == 0 {
		// Pure punctuation: a bullet or a rule. Not evidence either way, so
		// count it as fine rather than letting a list of dashes tank a good
		// page.
		return punct > 0
	}
	return true
}

// isDamage reports characters that only appear when decoding has gone wrong.
func isDamage(r rune) bool {
	return r == '�' || (unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\r')
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}
