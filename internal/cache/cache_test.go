package cache

import (
	"testing"

	"github.com/tendant/dolico/internal/canonical"
)

func samplePage(text string) canonical.Page {
	return canonical.Page{
		Number: 1,
		Kind:   canonical.PageKindPaginated,
		Classification: canonical.Classification{
			Type: canonical.PageTypeTextBased, Confidence: 1, Reasons: []string{"ok"},
		},
		Blocks: []canonical.Block{{ID: "p1-b0", Type: canonical.BlockParagraph, Text: text}},
	}
}

func key() Key {
	return Key{DocumentHash: "abc", Engine: "pdf", EngineVersion: "1", Page: 1}
}

func TestRoundTrip(t *testing.T) {
	c := New(0)
	c.Put(key(), samplePage("hello"))
	got, ok := c.Get(key())
	if !ok {
		t.Fatal("expected a cache hit")
	}
	if got.Blocks[0].Text != "hello" {
		t.Errorf("text = %q", got.Blocks[0].Text)
	}
}

func TestMissOnUnknownKey(t *testing.T) {
	if _, ok := New(0).Get(key()); ok {
		t.Error("expected a miss on an empty cache")
	}
}

// Each component of the key must actually change the key, or an engine upgrade
// would silently serve stale pages.
func TestEveryKeyComponentChangesTheKey(t *testing.T) {
	base := key()
	variants := map[string]Key{
		"document hash":  {DocumentHash: "zzz", Engine: "pdf", EngineVersion: "1", Page: 1},
		"engine":         {DocumentHash: "abc", Engine: "ocr", EngineVersion: "1", Page: 1},
		"engine version": {DocumentHash: "abc", Engine: "pdf", EngineVersion: "2", Page: 1},
		"page":           {DocumentHash: "abc", Engine: "pdf", EngineVersion: "1", Page: 2},
	}
	for name, v := range variants {
		if v.String() == base.String() {
			t.Errorf("changing the %s did not change the cache key", name)
		}
	}
}

func TestPipelineVersionIsPartOfTheKey(t *testing.T) {
	if k := key().String(); !contains(k, canonical.PipelineVersion) {
		t.Errorf("key %q does not include the pipeline version", k)
	}
}

// Map iteration order must not produce two spellings of one key.
func TestConfigKeyIsOrderIndependent(t *testing.T) {
	a := Key{DocumentHash: "abc", Engine: "e", EngineVersion: "1", Page: 1,
		Config: map[string]string{"dpi": "300", "lang": "eng", "mode": "fast"}}
	b := Key{DocumentHash: "abc", Engine: "e", EngineVersion: "1", Page: 1,
		Config: map[string]string{"mode": "fast", "lang": "eng", "dpi": "300"}}
	if a.String() != b.String() {
		t.Errorf("key depends on map order:\n  %s\n  %s", a.String(), b.String())
	}
}

func TestDifferentConfigIsADifferentKey(t *testing.T) {
	a := Key{DocumentHash: "abc", Engine: "e", EngineVersion: "1", Page: 1,
		Config: map[string]string{"dpi": "300"}}
	b := Key{DocumentHash: "abc", Engine: "e", EngineVersion: "1", Page: 1,
		Config: map[string]string{"dpi": "600"}}
	if a.String() == b.String() {
		t.Error("different configuration produced the same key")
	}
}

// Handing out the stored value directly would let one caller's post-processing
// rewrite what the next caller reads back.
func TestStoredPagesAreIsolatedFromCallerMutation(t *testing.T) {
	c := New(0)
	page := samplePage("original")
	c.Put(key(), page)

	// Mutate the value we passed in.
	page.Blocks[0].Text = "mutated after Put"
	page.Classification.Reasons[0] = "tampered"

	got, _ := c.Get(key())
	if got.Blocks[0].Text != "original" {
		t.Errorf("mutating the source changed the cached page: %q", got.Blocks[0].Text)
	}
	if got.Classification.Reasons[0] != "ok" {
		t.Errorf("mutating the source changed cached reasons: %q", got.Classification.Reasons[0])
	}

	// Mutate what we got back.
	got.Blocks[0].Text = "mutated after Get"
	again, _ := c.Get(key())
	if again.Blocks[0].Text != "original" {
		t.Errorf("mutating a returned page changed the cache: %q", again.Blocks[0].Text)
	}
}

func TestNestedStructuresAreDeepCopied(t *testing.T) {
	c := New(0)
	page := canonical.Page{
		Number: 1,
		Blocks: []canonical.Block{{
			ID: "t", Type: canonical.BlockTable,
			Table: &canonical.Table{
				HeaderRows: 1,
				Grid: [][]canonical.Cell{{{Blocks: []canonical.Block{{
					ID: "c", Type: canonical.BlockParagraph, Text: "cell",
				}}}}},
			},
		}},
	}
	c.Put(key(), page)
	got, _ := c.Get(key())
	got.Blocks[0].Table.Grid[0][0].Blocks[0].Text = "tampered"

	again, _ := c.Get(key())
	if again.Blocks[0].Table.Grid[0][0].Blocks[0].Text != "cell" {
		t.Error("nested table cells share storage with the cache")
	}
}

// Assets belong to the document, not to any one page, and must survive a
// page-cache hit.
func TestAssetsRoundTripIndependentlyOfPages(t *testing.T) {
	c := New(0)
	k := Key{DocumentHash: "abc", Engine: "pdf", EngineVersion: "1"}
	assets := []canonical.Asset{{ID: "a0", MediaType: "image/png", BlobRef: "x", SizeBytes: 3}}
	c.PutAssets(k, assets)

	got, ok := c.GetAssets(k)
	if !ok || len(got) != 1 || got[0].ID != "a0" {
		t.Fatalf("GetAssets = %v, %v", got, ok)
	}
	// The page number must not affect asset lookup.
	withPage := k
	withPage.Page = 7
	if _, ok := c.GetAssets(withPage); !ok {
		t.Error("asset lookup should ignore the page number")
	}
}

// "No assets" and "not cached" are different answers.
func TestEmptyAssetsAreStillRecorded(t *testing.T) {
	c := New(0)
	k := Key{DocumentHash: "abc", Engine: "pdf", EngineVersion: "1"}
	if _, ok := c.GetAssets(k); ok {
		t.Fatal("expected a miss before anything is stored")
	}
	c.PutAssets(k, nil)
	if _, ok := c.GetAssets(k); !ok {
		t.Error("an engine that produced no assets should still record that")
	}
}

func TestStatsCountHitsAndMisses(t *testing.T) {
	c := New(0)
	c.Get(key()) // miss
	c.Put(key(), samplePage("x"))
	c.Get(key()) // hit
	c.Get(key()) // hit

	hits, misses, size := c.Stats()
	if hits != 2 || misses != 1 || size != 1 {
		t.Errorf("hits=%d misses=%d size=%d, want 2/1/1", hits, misses, size)
	}
}

func TestLimitBoundsRetention(t *testing.T) {
	c := New(2)
	for i := range 10 {
		k := key()
		k.Page = i
		c.Put(k, samplePage("x"))
	}
	if _, _, size := c.Stats(); size > 2 {
		t.Errorf("cache grew to %d pages despite a limit of 2", size)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
