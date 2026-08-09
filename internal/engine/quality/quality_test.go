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
