package paddleocr_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tendant/dolico/internal/canonical"
	"github.com/tendant/dolico/internal/engine"
	"github.com/tendant/dolico/internal/engine/paddleocr"
)

// fakeService stands in for the Python service. The transport and the failure
// classification are what these tests are about; the real service is covered
// separately below, and only when one is running.
type fakeService struct {
	schemaVersion string
	engineVersion string

	extractStatus int
	extractBody   any

	// captured from the last extract request
	gotPages    string
	gotDPI      string
	gotFilename string
	gotFileSize int
}

func (f *fakeService) start(t *testing.T) string {
	t.Helper()
	if f.schemaVersion == "" {
		f.schemaVersion = canonical.SchemaVersion
	}
	if f.engineVersion == "" {
		f.engineVersion = "3.4.1"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{
			"schema_version": f.schemaVersion,
			"engine":         "paddleocr",
			"engine_version": f.engineVersion,
		})
	})
	mux.HandleFunc("POST /v1/extract", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeJSON(w, 400, map[string]string{"kind": "malformed", "message": err.Error()})
			return
		}
		f.gotPages = r.FormValue("pages")
		f.gotDPI = r.FormValue("dpi")
		if file, header, err := r.FormFile("file"); err == nil {
			defer file.Close()
			f.gotFilename = header.Filename
			data, _ := io.ReadAll(file)
			f.gotFileSize = len(data)
		}

		status := f.extractStatus
		if status == 0 {
			status = 200
		}
		body := f.extractBody
		if body == nil {
			body = okResponse(f.schemaVersion, f.engineVersion)
		}
		writeJSON(w, status, body)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func okResponse(schema, version string) map[string]any {
	return map[string]any{
		"schema_version": schema,
		"engine":         "paddleocr",
		"engine_version": version,
		"metadata":       map[string]any{"page_count": 1},
		"duration_ms":    2500,
		"pages": []any{map[string]any{
			"number": 2,
			"kind":   "paginated",
			"width":  612.0,
			"height": 792.0,
			"classification": map[string]any{
				"type": "scanned", "confidence": 0.98, "reasons": []string{"ocr"},
			},
			"blocks": []any{map[string]any{
				"id":         "p2-ocr0",
				"type":       "paragraph",
				"text":       "SIGNED AGREEMENT",
				"confidence": 0.99,
				"bbox":       map[string]any{"x": 60.0, "y": 726.0, "width": 52.0, "height": 7.0},
				"provenance": map[string]any{
					"engine": "paddleocr", "engine_version": version,
					"method": "paddleocr/text-lines",
				},
			}},
		}},
	}
}

func testDocument(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "doc.pdf")
	if err := os.WriteFile(path, []byte("%PDF-1.7\nfake body"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func request(t *testing.T, pages ...int) *engine.ExtractRequest {
	t.Helper()
	return &engine.ExtractRequest{
		Source: canonical.Source{Filename: "mixed.pdf", SHA256: "abc"},
		Path:   testDocument(t),
		Pages:  pages,
	}
}

func TestNewChecksReachabilityAndVersion(t *testing.T) {
	fake := &fakeService{engineVersion: "3.4.1"}
	e, err := paddleocr.New(fake.start(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.Name() != paddleocr.Name {
		t.Errorf("Name = %q", e.Name())
	}
	if e.Version() != "3.4.1" {
		t.Errorf("Version = %q, want 3.4.1", e.Version())
	}
}

// A service built against a different schema would return JSON this binary
// silently mis-reads, so it must refuse to start rather than produce documents
// with quietly missing fields.
func TestNewRejectsASchemaMismatch(t *testing.T) {
	fake := &fakeService{schemaVersion: "9.9"}
	_, err := paddleocr.New(fake.start(t))
	if err == nil {
		t.Fatal("expected a schema mismatch error")
	}
	if !strings.Contains(err.Error(), "schema mismatch") {
		t.Errorf("err = %v", err)
	}
}

func TestNewFailsWhenTheServiceIsUnreachable(t *testing.T) {
	_, err := paddleocr.New("http://127.0.0.1:1", paddleocr.WithTimeout(2*time.Second))
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if !strings.Contains(err.Error(), "cannot reach") {
		t.Errorf("err = %v", err)
	}
}

// The OCR tier never decides what a document is.
func TestInspectDeclinesAndSupportsScoresZero(t *testing.T) {
	e, err := paddleocr.New((&fakeService{}).start(t))
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Inspect(context.Background(), canonical.Source{}, "/tmp/x")
	if !errors.Is(err, engine.ErrUnsupported) {
		t.Errorf("Inspect err = %v, want ErrUnsupported", err)
	}
	if score := e.Supports(nil); score != engine.SupportNone {
		t.Errorf("Supports = %v, want SupportNone", score)
	}
}

func TestExtractSendsTheDocumentAndThePageList(t *testing.T) {
	fake := &fakeService{}
	e, err := paddleocr.New(fake.start(t))
	if err != nil {
		t.Fatal(err)
	}

	res, err := e.Extract(context.Background(), request(t, 2))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if fake.gotPages != "2" {
		t.Errorf("pages field = %q, want \"2\"", fake.gotPages)
	}
	if fake.gotFilename != "mixed.pdf" {
		t.Errorf("filename = %q", fake.gotFilename)
	}
	if fake.gotFileSize == 0 {
		t.Error("the document bytes were not sent")
	}
	if len(res.Pages) != 1 || res.Pages[0].Number != 2 {
		t.Fatalf("unexpected pages: %+v", res.Pages)
	}
	if res.DurationMS != 2500 {
		t.Errorf("duration = %d", res.DurationMS)
	}
}

func TestExtractSendsSeveralPagesCommaSeparated(t *testing.T) {
	fake := &fakeService{}
	e, _ := paddleocr.New(fake.start(t))
	if _, err := e.Extract(context.Background(), request(t, 1, 3, 7)); err != nil {
		t.Fatal(err)
	}
	if fake.gotPages != "1,3,7" {
		t.Errorf("pages field = %q, want \"1,3,7\"", fake.gotPages)
	}
}

// OCR is the expensive tier; a caller that forgot to name pages must not
// silently pay for the whole document.
func TestExtractRefusesAnEmptyPageList(t *testing.T) {
	e, _ := paddleocr.New((&fakeService{}).start(t))
	_, err := e.Extract(context.Background(), request(t))
	if !errors.Is(err, engine.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestExtractPassesDPIWhenConfigured(t *testing.T) {
	fake := &fakeService{}
	e, _ := paddleocr.New(fake.start(t))
	req := request(t, 1)
	req.Config = map[string]string{"dpi": "300"}
	if _, err := e.Extract(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if fake.gotDPI != "300" {
		t.Errorf("dpi field = %q", fake.gotDPI)
	}
}

func TestExtractPreservesBlocksAndProvenance(t *testing.T) {
	e, _ := paddleocr.New((&fakeService{}).start(t))
	res, err := e.Extract(context.Background(), request(t, 2))
	if err != nil {
		t.Fatal(err)
	}
	block := res.Pages[0].Blocks[0]
	if block.Provenance.Engine != "paddleocr" {
		t.Errorf("engine = %q", block.Provenance.Engine)
	}
	if block.Confidence == nil || *block.Confidence != 0.99 {
		t.Errorf("confidence = %v", block.Confidence)
	}
	if block.BBox == nil || block.BBox.Width != 52 {
		t.Errorf("bbox = %+v", block.BBox)
	}
	// Rendering means the OCR path genuinely knows the page size.
	if res.Pages[0].Width == nil || *res.Pages[0].Width != 612 {
		t.Errorf("page width = %v", res.Pages[0].Width)
	}
}

// Failures have to reach the router as the same error values the Rust shim
// produces, so one classification path serves both engines.
func TestErrorClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		kind   string
		want   error
	}{
		{"unsupported media", 415, "unsupported", engine.ErrUnsupported},
		{"malformed", 422, "malformed", engine.ErrMalformed},
		{"encrypted", 422, "encrypted", engine.ErrEncrypted},
		{"too large", 413, "resource_limit", engine.ErrMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeService{
				extractStatus: tc.status,
				extractBody: map[string]string{
					"schema_version": canonical.SchemaVersion,
					"kind":           tc.kind,
					"message":        "detail here",
				},
			}
			e, _ := paddleocr.New(fake.start(t))
			_, err := e.Extract(context.Background(), request(t, 1))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), "detail here") {
				t.Errorf("the service's message was lost: %v", err)
			}
		})
	}
}

// A broken OCR service is not a broken document: the router must keep the
// pages that extracted natively, so this must not look like ErrMalformed.
func TestServiceFailureIsNotADocumentFailure(t *testing.T) {
	fake := &fakeService{extractStatus: 500, extractBody: map[string]string{"detail": "boom"}}
	e, _ := paddleocr.New(fake.start(t))

	_, err := e.Extract(context.Background(), request(t, 1))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, sentinel := range []error{engine.ErrMalformed, engine.ErrUnsupported, engine.ErrEncrypted} {
		if errors.Is(err, sentinel) {
			t.Errorf("a 500 was classified as %v; it is a service failure, not a document one", sentinel)
		}
	}
}

func TestExtractRejectsAResponseWithTheWrongSchema(t *testing.T) {
	fake := &fakeService{}
	url := fake.start(t)
	e, err := paddleocr.New(url)
	if err != nil {
		t.Fatal(err)
	}
	// The service is upgraded underneath us between the version check and the
	// request.
	fake.extractBody = okResponse("9.9", "3.4.1")
	if _, err := e.Extract(context.Background(), request(t, 1)); err == nil {
		t.Fatal("expected a schema error")
	}
}

func TestExtractReportsAMissingDocument(t *testing.T) {
	e, _ := paddleocr.New((&fakeService{}).start(t))
	req := request(t, 1)
	req.Path = filepath.Join(t.TempDir(), "does-not-exist.pdf")
	if _, err := e.Extract(context.Background(), req); err == nil {
		t.Fatal("expected an error for a missing document")
	}
}

func TestExtractHonorsContextCancellation(t *testing.T) {
	// The handler is released explicitly rather than by waiting on the request
	// context: httptest.Server.Close blocks until every handler has returned,
	// and a handler parked on client disconnect can outlive the test.
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/version" {
			writeJSON(w, 200, map[string]any{
				"schema_version": canonical.SchemaVersion, "engine_version": "3.4.1",
			})
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer func() {
		close(release)
		slow.Close()
	}()

	e, err := paddleocr.New(slow.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := e.Extract(ctx, request(t, 1)); err == nil {
		t.Fatal("expected the request to be cancelled")
	}
}

func TestVersionTracksAnUpgradedService(t *testing.T) {
	fake := &fakeService{engineVersion: "3.4.1"}
	e, _ := paddleocr.New(fake.start(t))
	fake.extractBody = okResponse(canonical.SchemaVersion, "4.0.0")

	if _, err := e.Extract(context.Background(), request(t, 1)); err != nil {
		t.Fatal(err)
	}
	// The version participates in cache keys, so noticing the upgrade is what
	// stops stale pages being served after one.
	if e.Version() != "4.0.0" {
		t.Errorf("Version = %q, want 4.0.0", e.Version())
	}
}

// Runs only when a real OCR service is reachable, so `make test` stays green
// without Python installed. `make test-ocr` sets DOLICO_OCR_URL.
func TestAgainstTheRealService(t *testing.T) {
	url := os.Getenv("DOLICO_OCR_URL")
	if url == "" {
		t.Skip("set DOLICO_OCR_URL to test against a running OCR service")
	}
	e, err := paddleocr.New(url)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Extract(context.Background(), &engine.ExtractRequest{
		Source: canonical.Source{Filename: "scanned.pdf"},
		Path:   filepath.Join(root, "testdata", "scanned.pdf"),
		Pages:  []int{1},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Pages) != 1 {
		t.Fatalf("expected 1 page, got %d", len(res.Pages))
	}
	page := res.Pages[0]
	if page.Number != 1 {
		t.Errorf("page number = %d", page.Number)
	}
	if len(page.Blocks) == 0 {
		t.Fatal("real OCR returned no blocks for a page with visible text")
	}

	var text strings.Builder
	for _, b := range page.Blocks {
		text.WriteString(b.Text)
		text.WriteString(" ")
		if b.Confidence == nil {
			t.Errorf("block %s has no confidence; OCR should always report one", b.ID)
		}
		if b.BBox == nil {
			t.Errorf("block %s has no bounding box", b.ID)
		}
	}
	// The fixture renders these words as pixels; nothing but OCR can recover
	// them.
	for _, want := range []string{"INVOICE", "4471"} {
		if !strings.Contains(strings.ToUpper(text.String()), want) {
			t.Errorf("OCR output %q does not contain %q", text.String(), want)
		}
	}
}
