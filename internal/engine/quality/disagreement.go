package quality

import "strings"

// MaxCompared bounds the text each side of a comparison contributes.
//
// Edit distance is quadratic, so an unusually long page could otherwise turn a
// cheap check into a noticeable one. At this cap a comparison is 16M cell
// updates, measured at ~31ms -- paid once per document, against an OCR tier
// that costs seconds per page. Truncation only makes two texts look more
// alike, so the effect of the cap is to under-report disagreement rather than
// to invent it.
const MaxCompared = 4000

// Disagreement measures how far two extractions of the same page are from each
// other: a normalized character edit distance, 0 for identical and 1 for
// nothing in common.
//
// This exists because a page's own text cannot say whether it is right. Both
// OCR tiers report high confidence on pages they misread, and every signal in
// Score() is computed from the output alone, so none of them can tell a good
// read from a confident wrong one. Two engines that disagree can.
//
// Whitespace is collapsed and everything else is kept, matching the benchmark's
// normalization -- an engine that reads "Amountdue" for "Amount due" made a
// real mistake, and normalizing that away would hide the exact class of error
// these engines make.
//
// Measured on this repository's corpus, comparing the OCR tier against the
// vision tier on the same page:
//
//	scanned.pdf         0.000   engines agree exactly
//	scanned-table.pdf   0.013
//	mixed.pdf p2        0.034
//	radio-1922.pdf      0.092   OCR misread it at 0.938 confidence
//	faded.pdf           1.000   OCR returned one character
//
// The gap between 0.034 and 0.092 is what DefaultDisagreement sits in.
func Disagreement(a, b string) float64 {
	ra := []rune(collapse(a))
	rb := []rune(collapse(b))
	if len(ra) > MaxCompared {
		ra = ra[:MaxCompared]
	}
	if len(rb) > MaxCompared {
		rb = rb[:MaxCompared]
	}

	switch {
	case len(ra) == 0 && len(rb) == 0:
		return 0
	case len(ra) == 0 || len(rb) == 0:
		// One engine produced nothing and the other produced something. That
		// is total disagreement, and it is the commonest interesting case.
		return 1
	}

	// Normalized by the longer side so the result is symmetric: which engine
	// is named "expected" is meaningless here, unlike in the benchmark where
	// one side is ground truth.
	longest := max(len(ra), len(rb))
	return clamp01(float64(levenshtein(ra, rb)) / float64(longest))
}

// DefaultDisagreement is the point above which two engines are taken to be
// reading different documents, so the one that was cheaper is not trusted.
//
// Chosen from the measurements above rather than from taste: the worst page
// where the tiers agreed scored 0.034, the best page where they genuinely
// disagreed scored 0.092. This sits between them, nearer the agreeing side --
// escalating a page that did not need it costs seconds, and missing one costs
// the document.
//
// Five pages is not a corpus. This is a default, not a constant of nature.
const DefaultDisagreement = 0.05

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// levenshtein over two rune slices, two rows at a time.
func levenshtein(a, b []rune) int {
	// Iterate over the shorter side so the rows stay small.
	if len(a) < len(b) {
		a, b = b, a
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
