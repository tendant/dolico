package rustshim_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tendant/dolico/internal/canonical"
	"github.com/tendant/dolico/internal/engine"
	"github.com/tendant/dolico/internal/engine/rustshim"
)

// These tests drive the real dolico-rs binary against the real fixtures. That
// is the point: the shim's whole job is to talk to two third-party Rust
// libraries correctly, and a mock of it would test nothing that matters.

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func newShim(t *testing.T) (*rustshim.Shim, string) {
	t.Helper()
	root := repoRoot(t)
	bin := filepath.Join(root, "rust/dolico-rs/target/release/dolico-rs")
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("shim not built (run `make build-rust`): %v", err)
	}
	s, err := rustshim.New(bin, 60*time.Second, t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, root
}

func source(t *testing.T, root, name string) (canonical.Source, string) {
	t.Helper()
	path := filepath.Join(root, "testdata", name)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return canonical.Source{Filename: name, SizeBytes: st.Size(), SHA256: name}, path
}

func TestVersionsMatchTheSchema(t *testing.T) {
	s, _ := newShim(t)
	versions, err := s.Versions()
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	for _, want := range []string{rustshim.EngineNative, rustshim.EnginePDF} {
		if versions[want] == "" {
			t.Errorf("no version reported for %s: %v", want, versions)
		}
	}
}

func TestInspectClassifiesPDFsPerPage(t *testing.T) {
	s, root := newShim(t)
	_, pdf := rustshim.Engines(s)

	cases := []struct {
		fixture string
		want    []canonical.PageType
	}{
		{"text.pdf", []canonical.PageType{canonical.PageTypeTextBased, canonical.PageTypeTextBased}},
		{"scanned.pdf", []canonical.PageType{canonical.PageTypeScanned}},
		// The case the whole design turns on: one text page, one scan.
		{"mixed.pdf", []canonical.PageType{canonical.PageTypeTextBased, canonical.PageTypeScanned}},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			src, path := source(t, root, tc.fixture)
			insp, err := pdf.Inspect(context.Background(), src, path)
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if insp.PageCount != len(tc.want) {
				t.Fatalf("page count = %d, want %d", insp.PageCount, len(tc.want))
			}
			for i, want := range tc.want {
				p := insp.Pages[i]
				if p.Number != i+1 {
					t.Errorf("page %d has number %d; pages must be 1-indexed", i, p.Number)
				}
				if p.Classification.Type != want {
					t.Errorf("page %d classified %s, want %s", i+1, p.Classification.Type, want)
				}
			}
		})
	}
}

func TestNativeEngineDeclinesPDFs(t *testing.T) {
	s, root := newShim(t)
	native, _ := rustshim.Engines(s)
	src, path := source(t, root, "text.pdf")

	_, err := native.Inspect(context.Background(), src, path)
	if !errors.Is(err, engine.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestPDFEngineDeclinesNativeFormats(t *testing.T) {
	s, root := newShim(t)
	_, pdf := rustshim.Engines(s)
	src, path := source(t, root, "sample.docx")

	_, err := pdf.Inspect(context.Background(), src, path)
	if !errors.Is(err, engine.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

// Blobs are stored by digest, so the path has no extension. Markdown, plain
// text and CSV have no content signature either, and are undetectable without
// the original filename.
func TestSignaturelessFormatsAreDetectedByFilename(t *testing.T) {
	s, root := newShim(t)
	native, _ := rustshim.Engines(s)

	for _, fixture := range []string{"sample.md", "sample.txt", "sample.csv"} {
		t.Run(fixture, func(t *testing.T) {
			// Copy the fixture to an extensionless name, exactly as the blob
			// store would hold it.
			data, err := os.ReadFile(filepath.Join(root, "testdata", fixture))
			if err != nil {
				t.Fatal(err)
			}
			stored := filepath.Join(t.TempDir(), "9f86d081884c7d65")
			if err := os.WriteFile(stored, data, 0o644); err != nil {
				t.Fatal(err)
			}
			src := canonical.Source{Filename: fixture, SHA256: "9f86d081884c7d65"}

			insp, err := native.Inspect(context.Background(), src, stored)
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			res, err := native.Extract(context.Background(), &engine.ExtractRequest{
				Source: src, Path: stored, Inspection: insp,
			})
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if len(res.Pages) != 1 || len(res.Pages[0].Blocks) == 0 {
				t.Fatalf("expected one page with blocks, got %+v", res.Pages)
			}
		})
	}
}

func TestExtractOnlyTheRequestedPages(t *testing.T) {
	s, root := newShim(t)
	_, pdf := rustshim.Engines(s)
	src, path := source(t, root, "text.pdf")

	res, err := pdf.Extract(context.Background(), &engine.ExtractRequest{
		Source: src, Path: path, Pages: []int{2},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Pages) != 1 {
		t.Fatalf("expected 1 page, got %d", len(res.Pages))
	}
	// This is the 0-vs-1 indexed conversion. Asking for page 2 must return
	// page 2, not page 1 and not page 3.
	if res.Pages[0].Number != 2 {
		t.Errorf("requested page 2, got page %d", res.Pages[0].Number)
	}
	text := pageText(res.Pages[0])
	if !strings.Contains(text, "Appendix") {
		t.Errorf("page 2 should contain the appendix heading, got %q", text)
	}
}

func TestExtractedBlocksCarryProvenance(t *testing.T) {
	s, root := newShim(t)
	native, _ := rustshim.Engines(s)
	src, path := source(t, root, "sample.docx")

	res, err := native.Extract(context.Background(), &engine.ExtractRequest{Source: src, Path: path})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, page := range res.Pages {
		for _, b := range page.Blocks {
			if b.Provenance.Engine != rustshim.EngineNative {
				t.Errorf("block %s has engine %q, want %q", b.ID, b.Provenance.Engine, rustshim.EngineNative)
			}
			if b.Provenance.Method == "" {
				t.Errorf("block %s has no provenance method", b.ID)
			}
			if b.ID == "" {
				t.Error("block has no id")
			}
		}
	}
}

func TestNativePagesHaveNoFabricatedGeometry(t *testing.T) {
	s, root := newShim(t)
	native, _ := rustshim.Engines(s)
	src, path := source(t, root, "sample.docx")

	res, err := native.Extract(context.Background(), &engine.ExtractRequest{Source: src, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range res.Pages {
		if page.Width != nil || page.Height != nil {
			t.Errorf("a DOCX section must not claim page dimensions: %v x %v", page.Width, page.Height)
		}
		for _, b := range page.Blocks {
			if b.BBox != nil {
				t.Errorf("block %s has a bounding box, but DOCX has no geometry", b.ID)
			}
		}
	}
}

func TestPDFBlocksCarryBoundingBoxes(t *testing.T) {
	s, root := newShim(t)
	_, pdf := rustshim.Engines(s)
	src, path := source(t, root, "text.pdf")

	res, err := pdf.Extract(context.Background(), &engine.ExtractRequest{Source: src, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	var withBox int
	for _, page := range res.Pages {
		for _, b := range page.Blocks {
			if b.BBox == nil {
				continue
			}
			withBox++
			if b.BBox.Width <= 0 || b.BBox.Height <= 0 {
				t.Errorf("block %s has a degenerate box %+v; it should have been omitted", b.ID, *b.BBox)
			}
		}
	}
	if withBox == 0 {
		t.Error("no PDF block got a bounding box")
	}
}

func TestAssetsAreWrittenToDiskNotInlined(t *testing.T) {
	s, root := newShim(t)
	native, _ := rustshim.Engines(s)
	src, path := source(t, root, "sample.pptx")
	assetsDir := t.TempDir()

	res, err := native.Extract(context.Background(), &engine.ExtractRequest{
		Source: src, Path: path, AssetsDir: assetsDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Assets) == 0 {
		t.Fatal("expected the embedded image to be extracted")
	}
	for _, a := range res.Assets {
		st, err := os.Stat(a.BlobRef)
		if err != nil {
			t.Errorf("asset %s: bytes not on disk at %s: %v", a.ID, a.BlobRef, err)
			continue
		}
		if st.Size() == 0 {
			t.Errorf("asset %s is empty", a.ID)
		}
		if a.MediaType == "" {
			t.Errorf("asset %s has no media type", a.ID)
		}
	}
}

func TestCorruptPDFIsMalformedNotACrash(t *testing.T) {
	s, root := newShim(t)
	_, pdf := rustshim.Engines(s)
	src, path := source(t, root, "corrupt.pdf")

	_, err := pdf.Inspect(context.Background(), src, path)
	if !errors.Is(err, engine.ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
}

func TestUnknownFormatIsUnsupported(t *testing.T) {
	s, _ := newShim(t)
	native, _ := rustshim.Engines(s)

	path := filepath.Join(t.TempDir(), "mystery")
	if err := os.WriteFile(path, []byte{0x00, 0x01, 0x02, 0x03}, 0o644); err != nil {
		t.Fatal(err)
	}
	src := canonical.Source{Filename: "mystery", SHA256: "zz"}

	_, err := native.Inspect(context.Background(), src, path)
	if !errors.Is(err, engine.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

// The registry asks every engine to inspect the same input, so the shim has to
// answer the second and third questions without spawning again.
func TestRegistryPicksTheRightEngineForEachFormat(t *testing.T) {
	s, root := newShim(t)
	native, pdf := rustshim.Engines(s)
	reg := engine.NewRegistry(native, pdf)

	cases := map[string]string{
		"sample.docx": rustshim.EngineNative,
		"sample.md":   rustshim.EngineNative,
		"sample.csv":  rustshim.EngineNative,
		"text.pdf":    rustshim.EnginePDF,
		"scanned.pdf": rustshim.EnginePDF,
	}
	for fixture, want := range cases {
		t.Run(fixture, func(t *testing.T) {
			src, path := source(t, root, fixture)
			_, chosen, err := reg.Inspect(context.Background(), src, path)
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if chosen.Name() != want {
				t.Errorf("registry chose %s, want %s", chosen.Name(), want)
			}
		})
	}
}

func pageText(p canonical.Page) string {
	var b strings.Builder
	for _, blk := range p.Blocks {
		b.WriteString(blk.Text)
		b.WriteByte('\n')
	}
	return b.String()
}
