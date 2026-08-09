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

// MeasuredCoverage is the share of a page's text that must come from blocks
// reporting their own confidence before that confidence is treated as a
// measurement of the page rather than a detail of some of it.
const MeasuredCoverage = 0.5

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
	measured, coverage := measuredConfidence(page.Blocks)
	if coverage > 0 {
		q.MeasuredConfidence = &measured
		q.MeasuredCoverage = coverage
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

	// A page whose text was measured is scored by its measurement.
	//
	// The three text signals can detect that extraction produced nothing, or
	// produced damage. What they cannot detect is a confident misread: OCR
	// emits no U+FFFD, and "Rcglon Unlts" is as word-like as "Region Units" to
	// any language-agnostic test. So on a measured page they are treated as a
	// ceiling that the measurement scales down, rather than as terms that can
	// vote the engine's own uncertainty away -- as a weighted term it can, and
	// the arithmetic is not close: with these weights no OCR page with text
	// could score below 0.55 however unsure the engine was.
	//
	// This is not a reversal of the package's rule against trusting engine
	// confidence. The rule is about *asserted* confidence -- a parser reading
	// DOCX XML is not 95% sure, it is reading a data structure, and it reports
	// no per-block confidence at all. Confidence that was measured is a
	// different thing, and the canonical model already tells them apart.
	if coverage >= MeasuredCoverage {
		ceiling := w.Density + w.Replacement + w.Words
		if ceiling <= 0 {
			ceiling = 1
		}
		q.Score = clamp01((w.Density*density+
			w.Replacement*(1-q.ReplacementRatio)+
			w.Words*q.WordRatio)/ceiling) * measured
		q.Score = clamp01(q.Score)
		return q
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

// measuredConfidence returns the length-weighted mean confidence of the blocks
// that report one, and the share of the page's characters they account for.
//
// Length-weighted because a page is not equally wrong everywhere: one badly
// read line among twenty good ones should move the page a twentieth, not a
// half. Coverage is returned separately because a mean over 5% of a page says
// nothing about the page.
func measuredConfidence(blocks []canonical.Block) (mean, coverage float64) {
	var weighted, scored, total float64
	var walk func([]canonical.Block)
	walk = func(bs []canonical.Block) {
		for _, blk := range bs {
			n := float64(len([]rune(blk.Text)))
			total += n
			if blk.Confidence != nil && n > 0 {
				weighted += *blk.Confidence * n
				scored += n
			}
			walk(blk.Quote)
			if blk.List != nil {
				for _, item := range blk.List.Items {
					walk(item.Blocks)
				}
			}
			if blk.Table != nil {
				for _, row := range blk.Table.Grid {
					for _, cell := range row {
						walk(cell.Blocks)
					}
				}
			}
		}
	}
	walk(blocks)

	if total == 0 || scored == 0 {
		return 0, 0
	}
	return clamp01(weighted / scored), clamp01(scored / total)
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
