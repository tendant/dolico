package quality

import (
	"strings"
	"testing"

	"github.com/tendant/dolico/internal/canonical"
)

func page(text string, confidence float64) canonical.Page {
	return canonical.Page{
		Number: 1,
		Kind:   canonical.PageKindPaginated,
		Classification: canonical.Classification{
			Type:       canonical.PageTypeTextBased,
			Confidence: confidence,
		},
		Blocks: []canonical.Block{{
			ID: "p1-b0", Type: canonical.BlockParagraph, Text: text,
		}},
	}
}

// measuredPage is what an OCR tier returns: blocks that report how sure the
// engine was of the characters it recognized.
func measuredPage(confidences map[string]float64) canonical.Page {
	p := canonical.Page{
		Number: 1,
		Kind:   canonical.PageKindPaginated,
		Classification: canonical.Classification{
			Type: canonical.PageTypeScanned, Confidence: 1.0,
		},
	}
	i := 0
	for text, conf := range confidences {
		c := conf
		p.Blocks = append(p.Blocks, canonical.Block{
			ID: "p1-ocr" + string(rune('0'+i)), Type: canonical.BlockParagraph,
			Text: text, Confidence: &c,
		})
		i++
	}
	return p
}

func TestCleanProseScoresHigh(t *testing.T) {
	p := page(strings.Repeat("The quarterly report shows steady growth across every region. ", 8), 1.0)
	q := Score(&p, DefaultWeights)
	if q.Score < 0.9 {
		t.Errorf("clean prose scored %.3f, expected > 0.9 (%+v)", q.Score, q)
	}
}

// The signal that matters most: a page that "extracted successfully" and is
// actually mojibake from a broken font encoding.
func TestMojibakeScoresLow(t *testing.T) {
	p := page(strings.Repeat("�������� ���� ��������� ", 10), 1.0)
	q := Score(&p, DefaultWeights)
	if q.Score >= 0.6 {
		t.Errorf("mojibake scored %.3f, expected below the 0.6 threshold (%+v)", q.Score, q)
	}
	if q.ReplacementRatio < 0.5 {
		t.Errorf("replacement ratio = %.3f, expected most characters to be replacements", q.ReplacementRatio)
	}
}

// The engine reports full confidence for both of the above. If the score
// tracked engine confidence, it would rate them the same.
func TestScoreIgnoresOptimisticEngineConfidence(t *testing.T) {
	good := page(strings.Repeat("Ordinary readable English sentences appear on this page. ", 8), 1.0)
	bad := page(strings.Repeat("�������� ���� ", 20), 1.0)

	gq, bq := Score(&good, DefaultWeights), Score(&bad, DefaultWeights)
	if bq.Score >= gq.Score {
		t.Errorf("garbage (%.3f) scored at or above clean text (%.3f) despite equal engine confidence",
			bq.Score, gq.Score)
	}
}

func TestEmptyPageScoresZero(t *testing.T) {
	p := canonical.Page{Number: 1, Blocks: nil}
	q := Score(&p, DefaultWeights)
	if q.Score != 0 {
		t.Errorf("empty page scored %.3f, want 0", q.Score)
	}
	if q.CharCount != 0 {
		t.Errorf("char count = %d, want 0", q.CharCount)
	}
}

func TestSparsePageScoresBelowDensePage(t *testing.T) {
	sparse := page("Chapter One", 1.0)
	dense := page(strings.Repeat("A full page of body text continues for some length here. ", 8), 1.0)
	if Score(&sparse, DefaultWeights).Score >= Score(&dense, DefaultWeights).Score {
		t.Error("a nearly-empty page should score below a full one")
	}
}

// Text inside tables and lists is still text; a page whose entire content is a
// table must not score as empty.
func TestTextInsideTablesAndListsCounts(t *testing.T) {
	cell := func(s string) canonical.Cell {
		return canonical.Cell{Blocks: []canonical.Block{{
			ID: "c", Type: canonical.BlockParagraph, Text: s,
		}}}
	}
	p := canonical.Page{
		Number:         1,
		Classification: canonical.Classification{Type: canonical.PageTypeTextBased, Confidence: 1},
		Blocks: []canonical.Block{
			{ID: "t", Type: canonical.BlockTable, Table: &canonical.Table{
				HeaderRows: 1,
				Grid: [][]canonical.Cell{
					{cell("Region"), cell("Revenue")},
					{cell("North America"), cell("fourteen thousand")},
				},
			}},
			{ID: "l", Type: canonical.BlockList, List: &canonical.List{
				Items: []canonical.ListItem{{Blocks: []canonical.Block{{
					ID: "li", Type: canonical.BlockParagraph, Text: "an item with real words in it",
				}}}},
			}},
		},
	}
	q := Score(&p, DefaultWeights)
	if q.CharCount == 0 {
		t.Fatal("table and list text was not counted")
	}
	if !strings.Contains(PlainText(&p), "North America") {
		t.Error("PlainText should reach into table cells")
	}
	if !strings.Contains(PlainText(&p), "an item with real words") {
		t.Error("PlainText should reach into list items")
	}
}

// OCR output in an unexpected script must not be scored as garbage merely for
// being unexpected.
func TestNonLatinTextIsNotPenalized(t *testing.T) {
	p := page(strings.Repeat("日本語のテキストがこのページに表示されます。 ", 12), 1.0)
	q := Score(&p, DefaultWeights)
	if q.WordRatio < 0.9 {
		t.Errorf("word ratio = %.3f for Japanese text, expected near 1", q.WordRatio)
	}
	if q.Score < 0.6 {
		t.Errorf("Japanese text scored %.3f, expected above the threshold", q.Score)
	}
}

func TestControlCharactersCountAsDamage(t *testing.T) {
	p := page("text\x00with\x01embedded\x02control\x03bytes", 1.0)
	if Score(&p, DefaultWeights).ReplacementRatio == 0 {
		t.Error("embedded control bytes should register as damage")
	}
}

func TestNewlinesAndTabsAreNotDamage(t *testing.T) {
	p := page("line one\nline two\tcolumn", 1.0)
	if r := Score(&p, DefaultWeights).ReplacementRatio; r != 0 {
		t.Errorf("replacement ratio = %.3f for ordinary whitespace, want 0", r)
	}
}

func TestScoreStaysInRange(t *testing.T) {
	for _, text := range []string{
		"", "a", strings.Repeat("word ", 500), "\x00\x01\x02", "���",
	} {
		p := page(text, 1.0)
		if q := Score(&p, DefaultWeights); q.Score < 0 || q.Score > 1 {
			t.Errorf("Score(%q) = %.3f, outside 0..1", text, q.Score)
		}
	}
}

func TestZeroWeightsDoNotDivideByZero(t *testing.T) {
	p := page("some text here", 1.0)
	q := Score(&p, Weights{})
	if q.Score < 0 || q.Score > 1 {
		t.Errorf("score with zero weights = %.3f, outside 0..1", q.Score)
	}
}

// ---------------------------------------------------------------------------
// Measured confidence
//
// The failure these cover is the one the text signals structurally cannot see:
// OCR that returns well-formed, word-shaped, damage-free text that happens to
// be wrong. Nothing about "Rcglon Unlts Rcvcnuc" is detectable without either
// a dictionary or the engine's own uncertainty.
// ---------------------------------------------------------------------------

func TestAConfidentMisreadCanFallBelowTheVisionThreshold(t *testing.T) {
	// Word-shaped, no damage, no replacement characters -- and wrong.
	p := measuredPage(map[string]float64{
		"Rcglon Unlts Rcvcnuc Nortb l2O l4,4OO.OO Soutb 8b lO,32O.OO": 0.42,
	})
	q := Score(&p, DefaultWeights)
	if q.WordRatio < 0.9 {
		t.Fatalf("word ratio = %.3f; the point of this case is that the text looks fine", q.WordRatio)
	}
	if q.Score >= 0.35 {
		t.Errorf("score = %.3f; a page the engine was 42%% sure of should reach the vision tier (%+v)",
			q.Score, q)
	}
}

func TestAConfidentGoodReadStaysHigh(t *testing.T) {
	p := measuredPage(map[string]float64{
		strings.Repeat("Region Units Revenue North 120 14,400.00 ", 12): 0.97,
	})
	q := Score(&p, DefaultWeights)
	if q.Score < 0.9 {
		t.Errorf("score = %.3f; a page read well and confidently should not escalate (%+v)", q.Score, q)
	}
}

// The whole reason the branch exists: as a weighted term, engine confidence
// cannot pull any page with text below 0.55, so no threshold under the 0.60
// OCR bar could ever select one.
func TestConfidenceIsDecisiveNotAdvisory(t *testing.T) {
	unsure := measuredPage(map[string]float64{"Ordinary looking words on a page here": 0.20})
	sure := measuredPage(map[string]float64{"Ordinary looking words on a page here": 0.99})

	uq, sq := Score(&unsure, DefaultWeights), Score(&sure, DefaultWeights)
	if ratio := uq.Score / sq.Score; ratio > 0.3 {
		t.Errorf("unsure %.3f vs sure %.3f: confidence barely moved the score", uq.Score, sq.Score)
	}
}

// Length-weighted, so one bad line among many good ones is not a bad page.
func TestOneBadLineDoesNotCondemnAPage(t *testing.T) {
	p := measuredPage(map[string]float64{
		strings.Repeat("This line was read cleanly and at length. ", 8): 0.98,
		"snlppet":                                                       0.20,
	})
	q := Score(&p, DefaultWeights)
	if q.Score < 0.8 {
		t.Errorf("score = %.3f; one short bad line should barely move a page of good text (%+v)",
			q.Score, q)
	}
}

// A native page reports no per-block confidence, so nothing about its scoring
// changes -- which is what keeps the 0.60 OCR threshold meaning what it did.
func TestAPageWithNoMeasurementIsScoredAsBefore(t *testing.T) {
	p := page(strings.Repeat("The quarterly report shows steady growth. ", 12), 1.0)
	q := Score(&p, DefaultWeights)
	if q.MeasuredConfidence != nil {
		t.Errorf("a page whose blocks report nothing should carry no measurement: %+v", q)
	}
	// The old formula: density 1, no replacements, all words, confidence 1.
	if q.Score < 0.99 {
		t.Errorf("score = %.3f, want the unchanged additive result", q.Score)
	}
}

// Below the coverage bar the mean describes a sliver, not the page.
func TestASliverOfMeasuredTextDoesNotScoreThePage(t *testing.T) {
	conf := 0.10
	p := page(strings.Repeat("Ordinary readable text extracted from the document. ", 8), 1.0)
	p.Blocks = append(p.Blocks, canonical.Block{
		ID: "p1-b1", Type: canonical.BlockParagraph, Text: "cptn", Confidence: &conf,
	})

	q := Score(&p, DefaultWeights)
	if q.MeasuredCoverage >= MeasuredCoverage {
		t.Fatalf("coverage = %.3f; this case needs it below the bar", q.MeasuredCoverage)
	}
	if q.Score < 0.9 {
		t.Errorf("score = %.3f; a four-character caption should not condemn the page (%+v)", q.Score, q)
	}
	// It is still reported, because it is true and a consumer may want it.
	if q.MeasuredConfidence == nil || *q.MeasuredConfidence != conf {
		t.Errorf("the measurement should be recorded even when unused: %+v", q)
	}
}

// Text inside tables is where a scanned table's characters live, so the
// measurement has to walk into them or a table page looks unmeasured.
func TestMeasurementWalksIntoTables(t *testing.T) {
	conf := 0.30
	cell := func(text string) canonical.Cell {
		return canonical.Cell{RowSpan: 1, ColSpan: 1, Blocks: []canonical.Block{{
			ID: "c", Type: canonical.BlockParagraph, Text: text, Confidence: &conf,
		}}}
	}
	p := canonical.Page{
		Number:         1,
		Classification: canonical.Classification{Type: canonical.PageTypeScanned, Confidence: 1},
		Blocks: []canonical.Block{{
			ID: "p1-t0", Type: canonical.BlockTable,
			Table: &canonical.Table{Grid: [][]canonical.Cell{
				{cell("Rcglon"), cell("Unlts")},
				{cell("Nortb"), cell("l2O")},
			}},
		}},
	}

	q := Score(&p, DefaultWeights)
	if q.MeasuredCoverage < 1 {
		t.Errorf("coverage = %.3f; every character on this page came from a scored cell", q.MeasuredCoverage)
	}
	if q.Score >= 0.35 {
		t.Errorf("score = %.3f; a table read at 30%% confidence should escalate", q.Score)
	}
}

func TestMeasuredScoreStaysInRange(t *testing.T) {
	for _, conf := range []float64{-1, 0, 0.5, 1, 2} {
		p := measuredPage(map[string]float64{"some text here": conf})
		if q := Score(&p, DefaultWeights); q.Score < 0 || q.Score > 1 {
			t.Errorf("confidence %v gave score %.3f, outside 0..1", conf, q.Score)
		}
	}
}
