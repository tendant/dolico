package canonical

import "testing"

func pageWith(number int, reasons []string, blocks int) Page {
	p := Page{
		Number:         number,
		Kind:           PageKindPaginated,
		Classification: Classification{Type: PageTypeScanned, Reasons: reasons},
	}
	for i := range blocks {
		p.Blocks = append(p.Blocks, Block{
			ID: "b", Type: BlockParagraph, Text: "some text",
			Provenance: Provenance{Engine: "ocr", EngineVersion: "1"},
		})
		_ = i
	}
	return p
}

func TestACompleteDocumentIsNotIncomplete(t *testing.T) {
	d := Document{Pages: []Page{
		pageWith(1, []string{"text_operators"}, 3),
		pageWith(2, []string{"ocr", "layout_analysis"}, 5),
	}}
	if reason, ok := d.Incomplete(); ok {
		t.Errorf("reported incomplete because of %q", reason)
	}
}

// The case this exists for: a tier was down, the page came back empty, and the
// document was stored anyway. Uploading the same bytes again must redo it.
func TestAFailedTierMakesTheDocumentIncomplete(t *testing.T) {
	for _, reason := range []string{ReasonOCRFailed, ReasonNoOCREngine, ReasonNotExtracted} {
		d := Document{Pages: []Page{
			pageWith(1, []string{"text_operators"}, 3),
			pageWith(2, []string{"scanned", reason}, 0),
		}}
		got, ok := d.Incomplete()
		if !ok {
			t.Errorf("%s did not mark the document incomplete", reason)
			continue
		}
		if got != reason {
			t.Errorf("reason = %q, want %q", got, reason)
		}
	}
}

// The distinction the whole predicate rests on. An engine that read the page
// and found nothing has finished its job; storing that and serving it again is
// correct, and reprocessing it on every upload would be endless work for a
// blank page.
func TestAnEmptyPageTheEngineActuallyReadIsComplete(t *testing.T) {
	d := Document{Pages: []Page{pageWith(1, []string{"ocr", "no_text_found"}, 0)}}
	if reason, ok := d.Incomplete(); ok {
		t.Errorf("a genuinely blank page was treated as unfinished (%q)", reason)
	}
}

// Vision failures leave the OCR tier's text on the page, so they are a missed
// improvement rather than a hole. Reprocessing for them would spend the most
// expensive tier in the pipeline on pages that already have an answer.
func TestAFailedVisionEscalationDoesNotForceReprocessing(t *testing.T) {
	for _, reason := range []string{"vision_failed", "vision_empty"} {
		d := Document{Pages: []Page{pageWith(1, []string{"ocr", reason}, 4)}}
		if got, ok := d.Incomplete(); ok {
			t.Errorf("%s forced reprocessing (%q)", reason, got)
		}
	}
}

func TestTheFirstFailingPageIsReported(t *testing.T) {
	d := Document{Pages: []Page{
		pageWith(1, []string{"ocr"}, 2),
		pageWith(2, []string{ReasonNotExtracted}, 0),
		pageWith(3, []string{ReasonOCRFailed}, 0),
	}}
	got, ok := d.Incomplete()
	if !ok || got != ReasonNotExtracted {
		t.Errorf("Incomplete() = %q, %v; want the first failure, %q", got, ok, ReasonNotExtracted)
	}
}

func TestADocumentWithNoPagesIsNotIncomplete(t *testing.T) {
	// Nothing to be missing. Whether an empty document is useful is a separate
	// question from whether it should be redone.
	if reason, ok := (&Document{}).Incomplete(); ok {
		t.Errorf("an empty document reported %q", reason)
	}
}
