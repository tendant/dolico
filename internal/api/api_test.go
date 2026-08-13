package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tendant/dolico/internal/api"
	"github.com/tendant/dolico/internal/blob"
	"github.com/tendant/dolico/internal/cache"
	"github.com/tendant/dolico/internal/canonical"
	"github.com/tendant/dolico/internal/engine"
	"github.com/tendant/dolico/internal/engine/ocrstub"
	"github.com/tendant/dolico/internal/engine/quality"
	"github.com/tendant/dolico/internal/engine/router"
	"github.com/tendant/dolico/internal/engine/rustshim"
	"github.com/tendant/dolico/internal/jobs"
	"github.com/tendant/dolico/internal/render"
)

// This exercises the whole stack against the real shim and the real fixtures:
// upload, route per page, extract, score, render, store, serve. Anything less
// would not catch the integration bugs, which are the ones that actually
// happen.

type harness struct {
	server *httptest.Server
	root   string
	store  *blob.Store
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// stubVision stands in for the escalation tier. Only its identity matters
// here: the endpoint reports what it is, it does not call it.
type stubVision struct{ name, version string }

func (s stubVision) Name() string    { return s.name }
func (s stubVision) Version() string { return s.version }
func (s stubVision) Inspect(context.Context, canonical.Source, string) (*engine.Inspection, error) {
	return nil, engine.ErrUnsupported
}
func (s stubVision) Supports(*engine.Inspection) engine.SupportScore { return engine.SupportNone }
func (s stubVision) Extract(context.Context, *engine.ExtractRequest) (*engine.ExtractResult, error) {
	return nil, engine.ErrUnsupported
}

func newHarness(t *testing.T) *harness { return newHarnessWith(t, nil) }

func newHarnessWith(t *testing.T, vision engine.Engine) *harness {
	t.Helper()
	root := repoRoot(t)
	bin := filepath.Join(root, "rust/dolico-rs/target/release/dolico-rs")
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("shim not built (run `make build-rust`): %v", err)
	}

	dataDir := t.TempDir()
	store, err := blob.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	tempDir := filepath.Join(dataDir, "tmp")
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shim, err := rustshim.New(bin, 60*time.Second, tempDir)
	if err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	native, pdf := rustshim.Engines(shim)
	ocr := ocrstub.New()
	registry := engine.NewRegistry(native, pdf, ocr)
	pageCache := cache.New(0)
	rt := router.New(registry, ocr, pageCache, router.Options{
		OCRThreshold: 0.6, Weights: quality.DefaultWeights, Logger: log,
	})

	jobStore := jobs.NewStore(2, 32, func(ctx context.Context, job *jobs.Job) (int, string, error) {
		assetsDir, cleanup, err := store.TempDir("assets")
		if err != nil {
			return 0, "internal", err
		}
		defer cleanup()

		doc, err := rt.Process(ctx, router.Request{
			DocumentID: job.DocumentID,
			TraceID:    job.TraceID,
			Source: canonical.Source{
				Filename: job.Filename, MediaType: job.MediaType,
				SHA256: job.SHA256, SizeBytes: job.SizeBytes,
			},
			Path:      store.Path(job.SHA256),
			AssetsDir: assetsDir,
		})
		if err != nil {
			switch {
			case router.IsUnsupported(err):
				return 0, "unsupported", err
			case router.IsBadDocument(err):
				return 0, "malformed", err
			default:
				return 0, "internal", err
			}
		}
		for i := range doc.Assets {
			if store.Exists(doc.Assets[i].BlobRef) {
				continue
			}
			digest, size, err := store.PutFile(doc.Assets[i].BlobRef)
			if err != nil {
				continue
			}
			doc.Assets[i].BlobRef = digest
			doc.Assets[i].SizeBytes = size
		}
		data, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return 0, "internal", err
		}
		if err := store.WriteDerived(doc.ID, "canonical.json", data); err != nil {
			return 0, "internal", err
		}
		if err := store.WriteDerived(doc.ID, "document.md", []byte(render.Markdown(doc))); err != nil {
			return 0, "internal", err
		}
		return len(doc.Pages), "", nil
	}, log)

	srv := httptest.NewServer(api.New(api.Deps{
		Store: store, Jobs: jobStore, Registry: registry, Vision: vision,
		Cache: pageCache,
		Log:   log, MaxUploadBytes: 64 << 20, ShimPath: bin, WaitTimeout: 60 * time.Second,
	}).Routes())

	t.Cleanup(func() {
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = jobStore.Shutdown(ctx)
	})
	return &harness{server: srv, root: root, store: store}
}

// upload posts a fixture and returns the response status and body.
func (h *harness) upload(t *testing.T, fixture, query string) (int, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.root, "testdata", fixture))
	if err != nil {
		t.Fatalf("fixture %s: %v", fixture, err)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", fixture)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	resp, err := http.Post(h.server.URL+"/v1/documents"+query, mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (h *harness) uploadDoc(t *testing.T, fixture string) canonical.Document {
	t.Helper()
	code, body := h.upload(t, fixture, "?wait=true")
	if code != http.StatusOK {
		t.Fatalf("upload %s: status %d, body %s", fixture, code, body)
	}
	var doc canonical.Document
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode %s: %v\n%s", fixture, err, body)
	}
	return doc
}

func (h *harness) get(t *testing.T, path string) (int, http.Header, []byte) {
	t.Helper()
	resp, err := http.Get(h.server.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, body
}

// Every fixture must survive the full pipeline and come out structurally
// valid. This is the golden test: it asserts the invariants the schema
// promises, on real documents.
func TestEveryFixtureProducesAValidDocument(t *testing.T) {
	h := newHarness(t)
	fixtures := []string{
		"sample.md", "sample.txt", "sample.csv",
		"sample.docx", "sample.xlsx", "sample.pptx",
		"text.pdf", "scanned.pdf", "mixed.pdf",
	}
	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			doc := h.uploadDoc(t, fixture)
			checkInvariants(t, doc)
			if len(doc.Pages) == 0 {
				t.Fatal("document has no pages")
			}
			var blocks int
			for _, p := range doc.Pages {
				blocks += len(p.Blocks)
			}
			if blocks == 0 {
				t.Error("document has no blocks at all")
			}
		})
	}
}

// checkInvariants asserts the structural promises of schema v1.
func checkInvariants(t *testing.T, doc canonical.Document) {
	t.Helper()
	if doc.SchemaVersion != canonical.SchemaVersion {
		t.Errorf("schema_version = %q, want %q", doc.SchemaVersion, canonical.SchemaVersion)
	}
	if doc.ID == "" {
		t.Error("document has no id")
	}
	if doc.Source.SHA256 == "" || len(doc.Source.SHA256) != 64 {
		t.Errorf("source sha256 = %q, want 64 hex characters", doc.Source.SHA256)
	}
	if doc.Trace.TraceID == "" {
		t.Error("document has no trace id")
	}
	if doc.Trace.PipelineVersion != canonical.PipelineVersion {
		t.Errorf("pipeline_version = %q", doc.Trace.PipelineVersion)
	}
	if len(doc.Trace.Engines) == 0 {
		t.Error("trace records no engine runs")
	}
	if doc.Metadata.PageCount != len(doc.Pages) {
		t.Errorf("metadata page_count = %d but there are %d pages", doc.Metadata.PageCount, len(doc.Pages))
	}

	assetIDs := make(map[string]bool, len(doc.Assets))
	for _, a := range doc.Assets {
		if a.ID == "" || a.BlobRef == "" {
			t.Errorf("asset %+v is missing an id or blob reference", a)
		}
		assetIDs[a.ID] = true
	}

	for i, page := range doc.Pages {
		if page.Number != i+1 {
			t.Errorf("page at index %d has number %d; pages must be 1-indexed and in order", i, page.Number)
		}
		if page.Kind == "" {
			t.Errorf("page %d has no kind", page.Number)
		}
		if c := page.Classification.Confidence; c < 0 || c > 1 {
			t.Errorf("page %d confidence = %v, outside 0..1", page.Number, c)
		}
		if page.Quality == nil {
			t.Errorf("page %d has no quality score", page.Number)
		} else if s := page.Quality.Score; s < 0 || s > 1 {
			t.Errorf("page %d quality = %v, outside 0..1", page.Number, s)
		}
		// A non-paginated source must not claim page geometry.
		if page.Kind != canonical.PageKindPaginated && (page.Width != nil || page.Height != nil) {
			t.Errorf("page %d of kind %s claims dimensions", page.Number, page.Kind)
		}
		checkBlocks(t, page, page.Blocks, map[string]bool{})
	}
}

func checkBlocks(t *testing.T, page canonical.Page, blocks []canonical.Block, seen map[string]bool) {
	t.Helper()
	for _, b := range blocks {
		if b.ID == "" {
			t.Errorf("page %d has a block with no id", page.Number)
		}
		if seen[b.ID] {
			t.Errorf("page %d has a duplicate block id %q", page.Number, b.ID)
		}
		seen[b.ID] = true

		if b.Type == "" {
			t.Errorf("block %s has no type", b.ID)
		}
		if b.Provenance.Engine == "" {
			t.Errorf("block %s has no provenance engine", b.ID)
		}
		if b.Provenance.Method == "" {
			t.Errorf("block %s has no provenance method", b.ID)
		}
		if b.Confidence != nil && (*b.Confidence < 0 || *b.Confidence > 1) {
			t.Errorf("block %s confidence = %v, outside 0..1", b.ID, *b.Confidence)
		}
		// A bounding box, when present, must be a real rectangle -- a
		// degenerate one is worse than none.
		if b.BBox != nil && (b.BBox.Width <= 0 || b.BBox.Height <= 0) {
			t.Errorf("block %s has a degenerate bbox %+v", b.ID, *b.BBox)
		}
		// Geometry only exists where the source paginates.
		if b.BBox != nil && page.Kind != canonical.PageKindPaginated {
			t.Errorf("block %s on a %s page has a bounding box", b.ID, page.Kind)
		}
		if b.Type == canonical.BlockHeading && b.Level < 1 {
			t.Errorf("heading %s has level %d", b.ID, b.Level)
		}

		checkBlocks(t, page, b.Quote, seen)
		if b.List != nil {
			for _, item := range b.List.Items {
				checkBlocks(t, page, item.Blocks, seen)
			}
		}
		if b.Table != nil {
			for _, row := range b.Table.Grid {
				for _, cell := range row {
					if cell.Covered != nil && len(cell.Blocks) > 0 {
						t.Errorf("block %s: a covered slot must not hold content", b.ID)
					}
					checkBlocks(t, page, cell.Blocks, seen)
				}
			}
		}
	}
}

// The design's central claim, verified end to end through the HTTP API.
func TestMixedPDFRoutesEachPageSeparately(t *testing.T) {
	h := newHarness(t)
	doc := h.uploadDoc(t, "mixed.pdf")

	if len(doc.Pages) != 2 {
		t.Fatalf("expected 2 pages, got %d", len(doc.Pages))
	}
	if got := doc.Pages[0].Classification.Type; got != canonical.PageTypeTextBased {
		t.Errorf("page 1 classified %s, want text_based", got)
	}
	if got := doc.Pages[0].Blocks[0].Provenance.Engine; got != rustshim.EnginePDF {
		t.Errorf("page 1 extracted by %s, want %s", got, rustshim.EnginePDF)
	}
	if got := doc.Pages[1].Blocks[0].Provenance.Engine; got != ocrstub.Name {
		t.Errorf("page 2 extracted by %s, want %s", got, ocrstub.Name)
	}
	// One OCR call, for one page.
	for _, run := range doc.Trace.Engines {
		if run.Engine == ocrstub.Name && !slices.Equal(run.Pages, []int{2}) {
			t.Errorf("OCR ran on pages %v, want only page 2", run.Pages)
		}
	}
}

func TestTextPDFNeverReachesOCR(t *testing.T) {
	h := newHarness(t)
	doc := h.uploadDoc(t, "text.pdf")
	for _, page := range doc.Pages {
		for _, b := range page.Blocks {
			if b.Provenance.Engine == ocrstub.Name {
				t.Fatalf("page %d of an all-text PDF was sent to OCR", page.Number)
			}
		}
	}
}

func TestScannedPDFReachesOCR(t *testing.T) {
	h := newHarness(t)
	doc := h.uploadDoc(t, "scanned.pdf")
	if len(doc.Pages) != 1 || len(doc.Pages[0].Blocks) == 0 {
		t.Fatalf("unexpected document shape: %+v", doc.Pages)
	}
	if got := doc.Pages[0].Blocks[0].Provenance.Engine; got != ocrstub.Name {
		t.Errorf("scanned page extracted by %s, want %s", got, ocrstub.Name)
	}
}

func TestAsyncFlowReportsJobStateThenServesTheDocument(t *testing.T) {
	h := newHarness(t)
	code, body := h.upload(t, "sample.docx", "")
	if code != http.StatusAccepted {
		t.Fatalf("status %d, body %s", code, body)
	}
	var accepted struct {
		JobID      string `json:"job_id"`
		DocumentID string `json:"document_id"`
		State      string `json:"state"`
	}
	if err := json.Unmarshal(body, &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted.JobID == "" || accepted.State != "queued" {
		t.Fatalf("unexpected accept payload: %s", body)
	}

	deadline := time.Now().Add(30 * time.Second)
	var job jobs.Job
	for time.Now().Before(deadline) {
		_, _, raw := h.get(t, "/v1/jobs/"+accepted.JobID)
		if err := json.Unmarshal(raw, &job); err != nil {
			t.Fatal(err)
		}
		if job.State == jobs.StateDone || job.State == jobs.StateFailed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job.State != jobs.StateDone {
		t.Fatalf("job ended in state %s: %s", job.State, job.Error)
	}
	if job.Pages < 1 {
		t.Errorf("job reports %d pages", job.Pages)
	}

	status, _, doc := h.get(t, "/v1/documents/"+job.DocumentID)
	if status != http.StatusOK {
		t.Fatalf("fetching the document returned %d: %s", status, doc)
	}
}

func TestMarkdownViewIsServedFromTheSameID(t *testing.T) {
	h := newHarness(t)
	doc := h.uploadDoc(t, "sample.docx")

	status, header, body := h.get(t, "/v1/documents/"+doc.ID+".md")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("content type = %q", ct)
	}
	if !strings.Contains(string(body), "# Engineering Handbook") {
		t.Errorf("markdown is missing the document heading:\n%s", body)
	}
}

func TestAssetIsServedWithItsMediaType(t *testing.T) {
	h := newHarness(t)
	doc := h.uploadDoc(t, "sample.pptx")
	if len(doc.Assets) == 0 {
		t.Fatal("expected the embedded image to be extracted")
	}
	asset := doc.Assets[0]

	status, header, body := h.get(t, "/v1/documents/"+doc.ID+"/assets/"+asset.ID)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	if got := header.Get("Content-Type"); got != asset.MediaType {
		t.Errorf("content type = %q, want %q", got, asset.MediaType)
	}
	if int64(len(body)) != asset.SizeBytes {
		t.Errorf("served %d bytes, expected %d", len(body), asset.SizeBytes)
	}
	if !bytes.HasPrefix(body, []byte("\x89PNG")) {
		t.Error("served bytes are not a PNG")
	}
}

// Re-uploading identical bytes must not degrade the stored document.
func TestReuploadIsStableAndKeepsAssets(t *testing.T) {
	h := newHarness(t)
	first := h.uploadDoc(t, "sample.pptx")
	second := h.uploadDoc(t, "sample.pptx")

	if first.ID != second.ID {
		t.Errorf("identical content produced different ids: %s vs %s", first.ID, second.ID)
	}
	if len(second.Assets) != len(first.Assets) {
		t.Errorf("re-upload changed the asset count: %d then %d", len(first.Assets), len(second.Assets))
	}
	if len(second.Pages) != len(first.Pages) {
		t.Errorf("re-upload changed the page count: %d then %d", len(first.Pages), len(second.Pages))
	}
}

func TestCorruptPDFIsUnprocessable(t *testing.T) {
	h := newHarness(t)
	code, body := h.upload(t, "corrupt.pdf", "?wait=true")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422; body %s", code, body)
	}
	var payload struct{ Kind string }
	_ = json.Unmarshal(body, &payload)
	if payload.Kind != "malformed" {
		t.Errorf("kind = %q, want malformed", payload.Kind)
	}
}

func TestUnknownFormatIsUnsupportedMediaType(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mystery.bin")
	if err := os.WriteFile(path, []byte{0, 1, 2, 3}, 0o644); err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("file", "mystery.bin")
	_, _ = part.Write([]byte{0, 1, 2, 3})
	mw.Close()

	resp, err := http.Post(h.server.URL+"/v1/documents?wait=true", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d, want 415; body %s", resp.StatusCode, out)
	}
}

func TestMissingDocumentIsNotFound(t *testing.T) {
	h := newHarness(t)
	if code, _, _ := h.get(t, "/v1/documents/"+strings.Repeat("0", 64)); code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
	if code, _, _ := h.get(t, "/v1/jobs/nope"); code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
}

func TestUploadWithoutAFileIsABadRequest(t *testing.T) {
	h := newHarness(t)
	resp, err := http.Post(h.server.URL+"/v1/documents", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestEveryResponseCarriesATraceID(t *testing.T) {
	h := newHarness(t)
	_, header, _ := h.get(t, "/healthz")
	if header.Get("X-Trace-Id") == "" {
		t.Error("no X-Trace-Id header")
	}
}

func TestEnginesEndpointReportsVersions(t *testing.T) {
	h := newHarness(t)
	_, _, body := h.get(t, "/v1/engines")
	var payload struct {
		Engines         []struct{ Name, Version string }
		SchemaVersion   string `json:"schema_version"`
		PipelineVersion string `json:"pipeline_version"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.SchemaVersion != canonical.SchemaVersion {
		t.Errorf("schema_version = %q", payload.SchemaVersion)
	}
	names := make(map[string]string, len(payload.Engines))
	for _, e := range payload.Engines {
		names[e.Name] = e.Version
	}
	for _, want := range []string{rustshim.EngineNative, rustshim.EnginePDF, ocrstub.Name} {
		if names[want] == "" {
			t.Errorf("engine %s missing or unversioned: %v", want, names)
		}
	}
}

// The vision tier is not in the registry — nothing selects it, the router
// calls it directly — but it does read documents, and MinerU's license
// requires a service built on it to say so. An engines endpoint that omits an
// engine that produced pages is wrong twice over.
func TestEnginesEndpointDisclosesTheVisionTier(t *testing.T) {
	h := newHarnessWith(t, stubVision{name: "mineru", version: "2.5.4"})
	_, _, body := h.get(t, "/v1/engines")

	var payload struct {
		Engines []struct{ Name, Version string }
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	for _, e := range payload.Engines {
		if e.Name == "mineru" {
			if e.Version != "2.5.4" {
				t.Errorf("version = %q, want 2.5.4", e.Version)
			}
			return
		}
	}
	t.Errorf("the vision tier is not listed: %s", body)
}

// ...and a two-tier deployment must not advertise a tier it does not have.
func TestEnginesEndpointOmitsAnAbsentVisionTier(t *testing.T) {
	h := newHarness(t)
	_, _, body := h.get(t, "/v1/engines")
	if strings.Contains(string(body), "mineru") {
		t.Errorf("a server with no vision tier advertised one: %s", body)
	}
}

func TestHealthzReportsShimStatus(t *testing.T) {
	h := newHarness(t)
	code, _, body := h.get(t, "/healthz")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	var payload struct{ Status, Shim string }
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Status != "ok" || payload.Shim == "" {
		t.Errorf("unexpected health payload: %s", body)
	}
}

// ---------------------------------------------------------------------------
// Inspection
//
// The point of the endpoint is that a caller can learn what a document is
// before paying for the extraction. So these tests are as much about what does
// *not* happen -- no extraction, no derived files -- as about the answer.
// ---------------------------------------------------------------------------

func (h *harness) inspect(t *testing.T, fixture string) (int, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.root, "testdata", fixture))
	if err != nil {
		t.Fatalf("fixture %s: %v", fixture, err)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", fixture)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	resp, err := http.Post(h.server.URL+"/v1/inspect", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

type inspection struct {
	DocumentID string         `json:"document_id"`
	SHA256     string         `json:"sha256"`
	Filename   string         `json:"filename"`
	MediaType  string         `json:"media_type"`
	SizeBytes  int64          `json:"size_bytes"`
	Engine     string         `json:"engine"`
	PageCount  int            `json:"page_count"`
	PageKind   string         `json:"page_kind"`
	PageTypes  map[string]int `json:"page_types"`
}

func TestInspectReportsPageCountWithoutExtracting(t *testing.T) {
	h := newHarness(t)
	code, body := h.inspect(t, "text.pdf")
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	var got inspection
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if got.PageCount != 2 {
		t.Errorf("page_count = %d, want 2", got.PageCount)
	}
	if got.Engine != "pdf-inspector" {
		t.Errorf("engine = %q, want pdf-inspector", got.Engine)
	}
	if got.PageTypes["text_based"] != 2 {
		t.Errorf("page_types = %v, want 2 text_based", got.PageTypes)
	}
	if got.DocumentID == "" || got.DocumentID != got.SHA256 {
		t.Errorf("document_id %q and sha256 %q should be the same digest", got.DocumentID, got.SHA256)
	}

	// Nothing was extracted: the document is not servable yet. That is the
	// whole economy of the endpoint -- if inspection quietly extracted, it
	// would cost exactly what it exists to avoid.
	if status, _, _ := h.get(t, "/v1/documents/"+got.DocumentID); status != http.StatusNotFound {
		t.Errorf("document is servable after inspection: status %d, want 404", status)
	}
}

func TestInspectSeparatesScannedFromTextPages(t *testing.T) {
	h := newHarness(t)
	code, body := h.inspect(t, "mixed.pdf")
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	var got inspection
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	// mixed.pdf is one text page and one scanned page. A caller deciding
	// whether to proceed cares which, because the two cost different work.
	if got.PageCount != 2 {
		t.Fatalf("page_count = %d, want 2", got.PageCount)
	}
	if got.PageTypes["text_based"] != 1 {
		t.Errorf("page_types = %v, want 1 text_based", got.PageTypes)
	}
	scanned := got.PageTypes["scanned"] + got.PageTypes["image_based"]
	if scanned != 1 {
		t.Errorf("page_types = %v, want 1 scanned or image_based", got.PageTypes)
	}
}

func TestInspectRejectsWhatCannotBeRead(t *testing.T) {
	h := newHarness(t)
	if code, body := h.inspect(t, "corrupt.pdf"); code != http.StatusUnprocessableEntity {
		t.Errorf("corrupt.pdf: status %d, want 422 (body %s)", code, body)
	}
}

func TestInspectThenExtractDoesNotResendTheBytes(t *testing.T) {
	h := newHarness(t)
	code, body := h.inspect(t, "text.pdf")
	if code != http.StatusOK {
		t.Fatalf("inspect: status %d, body %s", code, body)
	}
	var got inspection
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}

	// The second call carries a digest, not a document.
	ref, _ := json.Marshal(map[string]string{
		"sha256": got.SHA256, "filename": got.Filename, "media_type": got.MediaType,
	})
	resp, err := http.Post(h.server.URL+"/v1/documents?wait=true", "application/json", bytes.NewReader(ref))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("extract: status %d, body %s", resp.StatusCode, out)
	}
	var doc canonical.Document
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode document: %v", err)
	}
	if len(doc.Pages) != 2 {
		t.Errorf("pages = %d, want 2", len(doc.Pages))
	}
	if doc.ID != got.DocumentID {
		t.Errorf("document id = %q, want %q", doc.ID, got.DocumentID)
	}
	if doc.Source.Filename != "text.pdf" {
		t.Errorf("filename = %q, want text.pdf; the detection hint was lost", doc.Source.Filename)
	}
}

func TestExtractingAnUnknownDigestSaysToUploadItAgain(t *testing.T) {
	h := newHarness(t)
	ref, _ := json.Marshal(map[string]string{
		"sha256": strings.Repeat("a", 64), "filename": "ghost.pdf",
	})
	resp, err := http.Post(h.server.URL+"/v1/documents", "application/json", bytes.NewReader(ref))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404 (body %s)", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), "upload it again") {
		t.Errorf("body %s does not say what to do about it", out)
	}
}

// ---------------------------------------------------------------------------
// Deletion
//
// A retention policy is a promise about bytes on a disk. These tests are about
// whether the bytes are actually gone -- all of them, in every place the
// document lives.
// ---------------------------------------------------------------------------

func (h *harness) delete(t *testing.T, docID string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, h.server.URL+"/v1/documents/"+docID, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestDeleteRemovesTheDocumentAndTheUploadedBytes(t *testing.T) {
	h := newHarness(t)
	doc := h.uploadDoc(t, "text.pdf")

	if code := h.delete(t, doc.ID); code != http.StatusNoContent {
		t.Fatalf("delete: status %d, want 204", code)
	}

	// Nothing readable is left: not the canonical JSON, not the Markdown, and
	// not the file that was uploaded.
	if status, _, _ := h.get(t, "/v1/documents/"+doc.ID); status != http.StatusNotFound {
		t.Errorf("canonical JSON still served: status %d", status)
	}
	if status, _, _ := h.get(t, "/v1/documents/"+doc.ID+".md"); status != http.StatusNotFound {
		t.Errorf("markdown still served: status %d", status)
	}
	if h.store.Exists(doc.Source.SHA256) {
		t.Error("the uploaded bytes are still in the blob store")
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	h := newHarness(t)
	doc := h.uploadDoc(t, "sample.md")

	if code := h.delete(t, doc.ID); code != http.StatusNoContent {
		t.Fatalf("first delete: status %d, want 204", code)
	}
	// The caller is a sweep that retries. Deleting something already gone is
	// the outcome it wanted, not an error to handle.
	if code := h.delete(t, doc.ID); code != http.StatusNoContent {
		t.Errorf("second delete: status %d, want 204", code)
	}
}

func TestDeletedContentDoesNotComeBackFromTheCache(t *testing.T) {
	h := newHarness(t)
	doc := h.uploadDoc(t, "text.pdf")
	text := documentText(doc)
	if text == "" {
		t.Fatal("fixture produced no text to check against")
	}

	if code := h.delete(t, doc.ID); code != http.StatusNoContent {
		t.Fatalf("delete: status %d", code)
	}

	// Re-uploading the same bytes is the same digest, so a page cache that
	// kept the deleted pages would serve the old extraction straight back --
	// content that was supposed to be gone, returned without ever re-reading
	// the file. It has to be re-extracted instead.
	again := h.uploadDoc(t, "text.pdf")
	if again.ID != doc.ID {
		t.Fatalf("content addressing broke: %q then %q", doc.ID, again.ID)
	}
	if len(again.Trace.Engines) == 0 {
		t.Error("the document came back with no engine run recorded; it was not re-extracted")
	}
}

func TestDeleteRejectsSomethingThatIsNotADocumentID(t *testing.T) {
	h := newHarness(t)
	// A document id is a content digest. Anything else is a caller error, and
	// the point is that none of it reaches the filesystem.
	for _, id := range []string{"short", strings.Repeat("z", 64), "NOTHEX" + strings.Repeat("0", 58)} {
		if code := h.delete(t, id); code != http.StatusBadRequest {
			t.Errorf("delete %q: status %d, want 400", id, code)
		}
	}
	// Traversal never gets as far as the handler: ServeMux resolves the path
	// first and finds no route. Asserted anyway, because "the router happens
	// to stop it" is worth knowing if the routing ever changes.
	if code := h.delete(t, "../../etc/passwd"); code == http.StatusNoContent {
		t.Error("a traversal path was accepted as a document id")
	}
}

// documentText joins every block's text, for asserting on what a document says.
func documentText(doc canonical.Document) string {
	var b strings.Builder
	for _, p := range doc.Pages {
		for _, blk := range p.Blocks {
			b.WriteString(blk.Text)
			b.WriteByte(' ')
		}
	}
	return strings.TrimSpace(b.String())
}
