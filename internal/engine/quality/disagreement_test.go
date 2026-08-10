package quality

import (
	"strings"
	"testing"
)

func TestIdenticalTextDoesNotDisagree(t *testing.T) {
	s := "Invoice number 4471 Amount due 1,250.00"
	if d := Disagreement(s, s); d != 0 {
		t.Errorf("Disagreement of a string with itself = %v, want 0", d)
	}
}

func TestWhitespaceIsNotDisagreement(t *testing.T) {
	a := "Invoice number 4471\nAmount due 1,250.00"
	b := "  Invoice   number 4471   Amount due 1,250.00  "
	if d := Disagreement(a, b); d != 0 {
		t.Errorf("Disagreement = %v; only whitespace differs", d)
	}
}

// The error these engines actually make: losing the space between words. That
// is a real misread and must not be normalized away.
func TestALostSpaceIsDisagreement(t *testing.T) {
	if d := Disagreement("Amount due 1,250.00", "Amountdue 1,250.00"); d == 0 {
		t.Error("a merged word should register as disagreement")
	}
}

func TestOneEngineReturningNothingIsTotalDisagreement(t *testing.T) {
	if d := Disagreement("", "Invoice number 4471"); d != 1 {
		t.Errorf("Disagreement = %v, want 1", d)
	}
	if d := Disagreement("Invoice number 4471", ""); d != 1 {
		t.Errorf("Disagreement = %v, want 1", d)
	}
	if d := Disagreement("", ""); d != 0 {
		t.Errorf("two empty extractions disagree about nothing, got %v", d)
	}
}

func TestDisagreementIsSymmetric(t *testing.T) {
	a := "11:15 to 11:20 a.m.—Hog flash—Chicago and St. Louis."
	b := "11:13to 11:20a.im.—-Ho8nasne Chicago and st..Louis."
	if x, y := Disagreement(a, b), Disagreement(b, a); x != y {
		t.Errorf("Disagreement(a,b)=%v but Disagreement(b,a)=%v", x, y)
	}
}

// The pair the whole policy exists for, taken from the real 1922 scan. OCR
// reported 0.938 confidence on the second one.
func TestTheRealMisreadClearsTheDefaultThreshold(t *testing.T) {
	vision := "11:15 to 11:20 a.m.—Hog flash—Chicago and St. Louis. " +
		"11:30 to 11:40 a.m.—Fruit and vegetable shipments."
	ocr := "11:13to 11:20a.im.—-Ho8nasne Chicago and st..Louis. " +
		"11:301011:40a.m.-Fruit and vegetable shipments."

	d := Disagreement(vision, ocr)
	if d <= DefaultDisagreement {
		t.Errorf("disagreement = %.3f, at or below the %.2f bar; this is the case "+
			"the probe exists to catch", d, DefaultDisagreement)
	}
}

// ...and the converse, from the pages where both tiers read correctly: a
// difference of one character in a page must not trip it.
func TestASingleCharacterDifferenceStaysUnderTheThreshold(t *testing.T) {
	a := strings.Repeat("The quarterly report shows steady growth. ", 6)
	b := a[:len(a)-10] + "growlh. "

	if d := Disagreement(a, b); d > DefaultDisagreement {
		t.Errorf("disagreement = %.3f over the %.2f bar for one wrong character",
			d, DefaultDisagreement)
	}
}

func TestDisagreementStaysInRange(t *testing.T) {
	pairs := [][2]string{
		{"", ""},
		{"a", ""},
		{"a", "b"},
		{strings.Repeat("x", 5000), strings.Repeat("y", 5000)},
		{strings.Repeat("x", 5000), strings.Repeat("x", 5000)},
		{"日本語のテキスト", "日本語のテキス"},
	}
	for _, p := range pairs {
		if d := Disagreement(p[0], p[1]); d < 0 || d > 1 {
			t.Errorf("Disagreement(%.10q, %.10q) = %v, outside 0..1", p[0], p[1], d)
		}
	}
}

// Multi-byte text must be compared by character, not by byte, or an engine
// reading Japanese would look like it disagreed with itself.
func TestComparisonIsByCharacterNotByte(t *testing.T) {
	a := "日本語のテキストがこのページに表示されます"
	b := "日本語のテキストがこのページに表示されまス"
	d := Disagreement(a, b)
	// One character in twenty-one.
	if d > 0.06 {
		t.Errorf("disagreement = %.3f for one character of twenty-one", d)
	}
	if d == 0 {
		t.Error("a changed character should register")
	}
}

// The cap bounds the work, and truncation can only make two texts look more
// alike -- it must never manufacture disagreement.
func TestTheLengthCapDoesNotInventDisagreement(t *testing.T) {
	shared := strings.Repeat("identical prose on both sides. ", 400)
	if len([]rune(shared)) <= MaxCompared {
		t.Fatalf("this test needs text longer than the %d cap, got %d",
			MaxCompared, len([]rune(shared)))
	}
	if d := Disagreement(shared, shared+"and then a divergent tail"); d != 0 {
		t.Errorf("disagreement = %v; the compared prefixes are identical", d)
	}
}

func BenchmarkDisagreementAtTheCap(b *testing.B) {
	x := strings.Repeat("the quick brown fox jumps over the lazy dog ", 200)
	y := strings.Repeat("the quick brown fux jumps ovar the lazy dog ", 200)
	for b.Loop() {
		Disagreement(x, y)
	}
}
