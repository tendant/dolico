// Package paddleocr is the OCR tier: an Engine backed by the Python service in
// python/ocr-service.
//
// It is the third engine in the pipeline and never the first. Like the stub it
// replaces, it declines to inspect documents -- the router hands it specific
// pages after pdf-inspector has classified them, or after a natively-extracted
// page scored badly enough to escalate.
//
// The service speaks the same canonical extract envelope the Rust shim writes,
// so the only real difference between the two engines is the transport: exec
// for one, HTTP for the other.
package paddleocr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tendant/dolico/internal/canonical"
	"github.com/tendant/dolico/internal/engine"
)

// Name is the fallback engine identifier, used before the service has told us
// which tier it is serving.
//
// The real name comes from the service, because it depends on the tier:
// "paddleocr" for text-line OCR, "pp-structurev3" for layout analysis. It has
// to be the actual tier, not a generic label, because it is recorded in block
// provenance and keyed into the page cache -- switching tiers must invalidate
// the pages the other one produced.
const Name = "paddleocr"

// StructureName is the identifier the layout-analysis tier reports.
const StructureName = "pp-structurev3"

// DefaultTimeout bounds one OCR request. OCR is slow -- seconds per page on
// CPU, and a multi-page escalation is a multiple of that -- so this is much
// more generous than any other call in the pipeline.
const DefaultTimeout = 10 * time.Minute

// Engine talks to the OCR service over HTTP.
type Engine struct {
	baseURL string
	client  *http.Client

	mu      sync.RWMutex
	version string
	name    string
	tier    string
}

// Option configures the engine.
type Option func(*Engine)

// WithHTTPClient replaces the default client.
func WithHTTPClient(c *http.Client) Option {
	return func(e *Engine) { e.client = c }
}

// WithTimeout sets the per-request timeout.
func WithTimeout(d time.Duration) Option {
	return func(e *Engine) { e.client.Timeout = d }
}

// New builds an engine against the service at baseURL and confirms it is
// reachable and ready.
//
// Readiness is checked now rather than at the first scanned page: the service
// loads its models at startup and reports 503 until they are there, so a
// service that is still warming up should delay this process starting rather
// than fail the first document that happens to need OCR.
func New(baseURL string, opts ...Option) (*Engine, error) {
	e := &Engine{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: DefaultTimeout},
	}
	for _, opt := range opts {
		opt(e)
	}
	if err := e.refreshVersion(context.Background()); err != nil {
		return nil, err
	}
	return e, nil
}

// Name is the tier the service is actually serving, fixed at construction.
func (e *Engine) Name() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.name == "" {
		return Name
	}
	return e.name
}

// Tier is "layout" or "text", for diagnostics.
func (e *Engine) Tier() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.tier
}

// Version is the OCR engine's own version, reported by the service. It
// participates in cache keys, so upgrading the OCR service invalidates exactly
// the pages it produced and leaves natively-extracted pages alone.
func (e *Engine) Version() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.version == "" {
		return "unknown"
	}
	return e.version
}

// BaseURL is the configured service address, for diagnostics.
func (e *Engine) BaseURL() string { return e.baseURL }

type versionResponse struct {
	SchemaVersion  string `json:"schema_version"`
	ServiceVersion string `json:"service_version"`
	Engine         string `json:"engine"`
	EngineVersion  string `json:"engine_version"`
	Tier           string `json:"tier"`
}

func (e *Engine) refreshVersion(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.baseURL+"/v1/version", nil)
	if err != nil {
		return fmt.Errorf("paddleocr: %w", err)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("paddleocr: cannot reach the OCR service at %s: %w", e.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("paddleocr: %s/v1/version returned %s", e.baseURL, resp.Status)
	}

	var payload versionResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return fmt.Errorf("paddleocr: cannot parse the version response: %w", err)
	}
	if payload.SchemaVersion != canonical.SchemaVersion {
		// A service built against a different schema would return JSON this
		// binary silently mis-reads, exactly as with the Rust shim.
		return fmt.Errorf(
			"paddleocr: schema mismatch: the service emits %q, this build expects %q",
			payload.SchemaVersion, canonical.SchemaVersion)
	}

	e.mu.Lock()
	e.version = payload.EngineVersion
	if payload.Engine != "" {
		e.name = payload.Engine
	}
	e.tier = payload.Tier
	e.mu.Unlock()
	return nil
}

// Inspect always declines: the OCR tier never decides what a document is.
func (e *Engine) Inspect(context.Context, canonical.Source, string) (*engine.Inspection, error) {
	return nil, fmt.Errorf("%w: %s does not inspect documents", engine.ErrUnsupported, Name)
}

// Supports always scores zero, for the same reason.
func (e *Engine) Supports(*engine.Inspection) engine.SupportScore { return engine.SupportNone }

// extractResponse is the canonical envelope, identical to the Rust shim's.
type extractResponse struct {
	SchemaVersion string             `json:"schema_version"`
	Engine        string             `json:"engine"`
	EngineVersion string             `json:"engine_version"`
	Metadata      canonical.Metadata `json:"metadata"`
	Pages         []canonical.Page   `json:"pages"`
	DurationMS    int64              `json:"duration_ms"`
}

type errorResponse struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Extract OCRs the requested pages.
func (e *Engine) Extract(ctx context.Context, req *engine.ExtractRequest) (*engine.ExtractResult, error) {
	if len(req.Pages) == 0 {
		// Guard rather than default to "everything": OCR is the expensive
		// tier, and a caller that forgot to say which pages would silently
		// pay for the whole document.
		return nil, fmt.Errorf("%w: %s extracts named pages only", engine.ErrUnsupported, Name)
	}

	body, contentType, err := buildForm(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/v1/extract", body)
	if err != nil {
		return nil, fmt.Errorf("paddleocr: %w", err)
	}
	httpReq.Header.Set("Content-Type", contentType)

	resp, err := e.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("paddleocr: request to %s failed: %w", e.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, classify(resp)
	}

	var payload extractResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("paddleocr: cannot parse the response: %w", err)
	}
	if payload.SchemaVersion != canonical.SchemaVersion {
		return nil, fmt.Errorf("paddleocr: response schema %q, expected %q",
			payload.SchemaVersion, canonical.SchemaVersion)
	}

	// The service reports the engine version it actually ran, which may differ
	// from what the version endpoint said if it was upgraded underneath us.
	if payload.EngineVersion != "" && payload.EngineVersion != e.Version() {
		e.mu.Lock()
		e.version = payload.EngineVersion
		e.mu.Unlock()
	}
	// A tier change mid-run would silently mix layout blocks and text lines
	// under one cache key, so it is a hard error rather than a surprise.
	if payload.Engine != "" && payload.Engine != e.Name() {
		return nil, fmt.Errorf(
			"paddleocr: the service switched tier from %q to %q; restart dolico to pick it up",
			e.Name(), payload.Engine)
	}

	return &engine.ExtractResult{
		Pages:      payload.Pages,
		Metadata:   payload.Metadata,
		DurationMS: payload.DurationMS,
	}, nil
}

// buildForm streams the document and the page list into a multipart body.
//
// The document is sent by value rather than by path. That costs an upload per
// OCR call, and buys a service that can run on another host or in another
// container with no shared volume -- which is where this is going.
func buildForm(req *engine.ExtractRequest) (io.Reader, string, error) {
	file, err := os.Open(req.Path)
	if err != nil {
		return nil, "", fmt.Errorf("paddleocr: cannot read the document: %w", err)
	}
	defer file.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	name := req.Source.Filename
	if name == "" {
		name = "document"
	}
	part, err := mw.CreateFormFile("file", name)
	if err != nil {
		return nil, "", fmt.Errorf("paddleocr: %w", err)
	}
	if _, err := io.Copy(part, file); err != nil {
		return nil, "", fmt.Errorf("paddleocr: %w", err)
	}

	pages := make([]string, len(req.Pages))
	for i, p := range req.Pages {
		pages[i] = strconv.Itoa(p)
	}
	if err := mw.WriteField("pages", strings.Join(pages, ",")); err != nil {
		return nil, "", fmt.Errorf("paddleocr: %w", err)
	}
	if dpi := req.Config["dpi"]; dpi != "" {
		if err := mw.WriteField("dpi", dpi); err != nil {
			return nil, "", fmt.Errorf("paddleocr: %w", err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", fmt.Errorf("paddleocr: %w", err)
	}
	return &body, mw.FormDataContentType(), nil
}

// classify maps an error response onto the engine errors the router routes on,
// using the same failure envelope the Rust shim emits.
func classify(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var payload errorResponse
	_ = json.Unmarshal(bytes.TrimSpace(raw), &payload)

	detail := payload.Message
	if detail == "" {
		detail = strings.TrimSpace(string(raw))
	}
	if detail == "" {
		detail = resp.Status
	}

	switch {
	case payload.Kind == "unsupported" || resp.StatusCode == http.StatusUnsupportedMediaType:
		return fmt.Errorf("%w: %s", engine.ErrUnsupported, detail)
	case payload.Kind == "encrypted":
		return fmt.Errorf("%w: %s", engine.ErrEncrypted, detail)
	case payload.Kind == "malformed" || payload.Kind == "resource_limit",
		resp.StatusCode == http.StatusUnprocessableEntity,
		resp.StatusCode == http.StatusRequestEntityTooLarge:
		return fmt.Errorf("%w: %s", engine.ErrMalformed, detail)
	default:
		// Anything else -- 500, 503, a proxy error page -- is the OCR tier
		// being broken rather than the document. The router keeps the pages
		// that extracted natively and records this in the trace.
		return fmt.Errorf("paddleocr: service returned %s: %s", resp.Status, detail)
	}
}
