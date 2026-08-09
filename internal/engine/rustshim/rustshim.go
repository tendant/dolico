// Package rustshim drives the dolico-rs binary as a subprocess.
//
// The Rust libraries are reached by exec rather than by cgo or an HTTP
// service: no ports, no lifecycle, no health checks, and a crash in a parser
// takes down one subprocess instead of the API. Process spawn costs a couple
// of milliseconds against extraction times in the tens, so it is not the
// bottleneck.
//
// The seam is deliberate. Everything that knows how to talk to the shim is the
// unexported runner below; flipping to a long-lived HTTP service later means
// replacing that one type, with the two Engine implementations unchanged.
package rustshim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tendant/dolico/internal/canonical"
	"github.com/tendant/dolico/internal/engine"
)

// Engine names, matching what the shim writes into block provenance.
const (
	EngineNative = "anydoc"
	EnginePDF    = "pdf-inspector"
)

// Exit codes are the shim's routing contract. See rust/dolico-rs/src/main.rs.
const (
	exitInternal    = 1
	exitUnsupported = 2
	exitMalformed   = 3
	exitEncrypted   = 4
)

// Shim is a handle on the dolico-rs binary, shared by the engines built from
// it.
type Shim struct {
	bin     string
	timeout time.Duration
	tempDir string

	versionOnce sync.Once
	versions    versionOutput
	versionErr  error

	// inspections memoizes inspect results by document digest. The registry
	// asks every engine to inspect the same input, so without this a
	// three-engine registry would spawn the shim three times to learn the same
	// thing.
	inspections sync.Map // digest -> *inspectOutput
}

// New returns a Shim for the binary at bin. It verifies the binary is
// executable now rather than at first upload, so a misconfigured deployment
// fails at startup.
func New(bin string, timeout time.Duration, tempDir string) (*Shim, error) {
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("rustshim: %q is not executable: %w", bin, err)
	}
	s := &Shim{bin: resolved, timeout: timeout, tempDir: tempDir}
	if _, err := s.Versions(); err != nil {
		return nil, err
	}
	return s, nil
}

type versionOutput struct {
	SchemaVersion string            `json:"schema_version"`
	ShimVersion   string            `json:"shim_version"`
	Engines       map[string]string `json:"engines"`
}

// Versions reports the shim and engine versions, queried once and cached.
func (s *Shim) Versions() (map[string]string, error) {
	s.versionOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, s.bin, "version").Output()
		if err != nil {
			s.versionErr = fmt.Errorf("rustshim: version: %w", err)
			return
		}
		if err := json.Unmarshal(out, &s.versions); err != nil {
			s.versionErr = fmt.Errorf("rustshim: parse version: %w", err)
			return
		}
		if s.versions.SchemaVersion != canonical.SchemaVersion {
			// A shim built against a different schema would produce JSON this
			// binary silently mis-reads. Refusing to start beats emitting
			// documents with quietly missing fields.
			s.versionErr = fmt.Errorf(
				"rustshim: schema mismatch: shim emits %q, this build expects %q; rebuild with `make build`",
				s.versions.SchemaVersion, canonical.SchemaVersion)
		}
	})
	if s.versionErr != nil {
		return nil, s.versionErr
	}
	return s.versions.Engines, nil
}

// Binary is the resolved path to the shim, for diagnostics.
func (s *Shim) Binary() string { return s.bin }

func (s *Shim) engineVersion(name string) string {
	if v, ok := s.versions.Engines[name]; ok {
		return v
	}
	return "unknown"
}

// ---------------------------------------------------------------------------
// Shim invocation
// ---------------------------------------------------------------------------

// shimError is the failure envelope the shim writes to stdout.
type shimError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// run invokes the shim and decodes its JSON output into dest.
//
// Output goes to a temp file rather than a pipe: extraction JSON for a large
// document runs to megabytes, and a pipe would have to be drained concurrently
// with waiting on the process to avoid deadlocking on a full buffer.
func (s *Shim) run(ctx context.Context, dest any, args ...string) error {
	f, err := os.CreateTemp(s.tempDir, "shim-out-*.json")
	if err != nil {
		return fmt.Errorf("rustshim: temp output: %w", err)
	}
	outPath := f.Name()
	f.Close()
	defer os.Remove(outPath)

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, s.bin, append(args, "--out", outPath)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if runErr != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("rustshim: %s timed out after %s", args[0], s.timeout)
		}
		return s.classify(runErr, stdout.Bytes(), stderr.String(), args)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		return fmt.Errorf("rustshim: read output: %w", err)
	}
	if err := json.Unmarshal(data, dest); err != nil {
		return fmt.Errorf("rustshim: parse output: %w", err)
	}
	return nil
}

// classify turns a nonzero exit into the engine error the router routes on.
func (s *Shim) classify(runErr error, stdout []byte, stderr string, args []string) error {
	var payload shimError
	_ = json.Unmarshal(bytes.TrimSpace(stdout), &payload)
	detail := payload.Message
	if detail == "" {
		detail = strings.TrimSpace(stderr)
	}
	if detail == "" {
		detail = runErr.Error()
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		switch exitErr.ExitCode() {
		case exitUnsupported:
			return fmt.Errorf("%w: %s", engine.ErrUnsupported, detail)
		case exitMalformed:
			return fmt.Errorf("%w: %s", engine.ErrMalformed, detail)
		case exitEncrypted:
			return fmt.Errorf("%w: %s", engine.ErrEncrypted, detail)
		case exitInternal:
			return fmt.Errorf("rustshim: %s failed: %s", args[0], detail)
		}
	}
	return fmt.Errorf("rustshim: %s: %s", args[0], detail)
}

// ---------------------------------------------------------------------------
// Shim output types
// ---------------------------------------------------------------------------

type inspectOutput struct {
	SchemaVersion string             `json:"schema_version"`
	Engine        string             `json:"engine"`
	EngineVersion string             `json:"engine_version"`
	PageCount     int                `json:"page_count"`
	PageKind      canonical.PageKind `json:"page_kind"`
	Metadata      canonical.Metadata `json:"metadata"`
	Pages         []canonical.Page   `json:"pages"`
	DurationMS    int64              `json:"duration_ms"`
}

type extractOutput struct {
	SchemaVersion string             `json:"schema_version"`
	Engine        string             `json:"engine"`
	EngineVersion string             `json:"engine_version"`
	Metadata      canonical.Metadata `json:"metadata"`
	Pages         []canonical.Page   `json:"pages"`
	Assets        []canonical.Asset  `json:"assets"`
	DurationMS    int64              `json:"duration_ms"`
}

// inspect runs the shim's inspect subcommand, memoized by document digest.
func (s *Shim) inspect(ctx context.Context, src canonical.Source, path string) (*inspectOutput, error) {
	if cached, ok := s.inspections.Load(src.SHA256); ok {
		return cached.(*inspectOutput), nil
	}
	var out inspectOutput
	// The original filename is a detection hint: blobs are stored by digest,
	// so the path carries no extension, and Markdown, text and CSV have no
	// content signature to fall back on.
	args := []string{"inspect", "--in", path}
	if src.Filename != "" {
		args = append(args, "--name", src.Filename)
	}
	if err := s.run(ctx, &out, args...); err != nil {
		return nil, err
	}
	if out.SchemaVersion != canonical.SchemaVersion {
		return nil, fmt.Errorf("rustshim: inspect returned schema %q, expected %q",
			out.SchemaVersion, canonical.SchemaVersion)
	}
	s.inspections.Store(src.SHA256, &out)
	return &out, nil
}

func (s *Shim) toInspection(src canonical.Source, out *inspectOutput) *engine.Inspection {
	return &engine.Inspection{
		Source:    src,
		PageCount: out.PageCount,
		PageKind:  out.PageKind,
		Pages:     out.Pages,
		Metadata:  out.Metadata,
		Engine:    out.Engine,
	}
}

// extract runs the shim's extract subcommand and rewrites asset references
// from shim-relative filenames to absolute paths inside assetsDir.
func (s *Shim) extract(ctx context.Context, req *engine.ExtractRequest, pdf bool) (*engine.ExtractResult, error) {
	args := []string{"extract", "--in", req.Path}
	if req.Source.Filename != "" {
		args = append(args, "--name", req.Source.Filename)
	}
	if req.AssetsDir != "" {
		args = append(args, "--assets-dir", req.AssetsDir)
	}
	if pdf && len(req.Pages) > 0 {
		pages := make([]string, len(req.Pages))
		for i, p := range req.Pages {
			pages[i] = strconv.Itoa(p)
		}
		args = append(args, "--pages", strings.Join(pages, ","))
	}

	var out extractOutput
	if err := s.run(ctx, &out, args...); err != nil {
		return nil, err
	}
	if out.SchemaVersion != canonical.SchemaVersion {
		return nil, fmt.Errorf("rustshim: extract returned schema %q, expected %q",
			out.SchemaVersion, canonical.SchemaVersion)
	}
	for i := range out.Assets {
		out.Assets[i].BlobRef = filepath.Join(req.AssetsDir, out.Assets[i].BlobRef)
	}
	return &engine.ExtractResult{
		Pages:      out.Pages,
		Assets:     out.Assets,
		Metadata:   out.Metadata,
		DurationMS: out.DurationMS,
	}, nil
}

// ---------------------------------------------------------------------------
// Engines
// ---------------------------------------------------------------------------

// NativeEngine handles every format anydoc covers, plus Markdown and plain
// text.
type NativeEngine struct{ shim *Shim }

// PDFEngine handles PDFs, with per-page classification and extraction.
type PDFEngine struct{ shim *Shim }

// Engines builds both engines over one shim.
func Engines(s *Shim) (*NativeEngine, *PDFEngine) {
	return &NativeEngine{shim: s}, &PDFEngine{shim: s}
}

func (e *NativeEngine) Name() string    { return EngineNative }
func (e *NativeEngine) Version() string { return e.shim.engineVersion(EngineNative) }

func (e *NativeEngine) Inspect(ctx context.Context, src canonical.Source, path string) (*engine.Inspection, error) {
	out, err := e.shim.inspect(ctx, src, path)
	if err != nil {
		return nil, err
	}
	if out.Engine != EngineNative {
		// A PDF: the shim routed it to pdf-inspector, so this engine is not
		// the one. Not an error -- a routing miss.
		return nil, fmt.Errorf("%w: %s is handled by %s", engine.ErrUnsupported, src.Filename, out.Engine)
	}
	return e.shim.toInspection(src, out), nil
}

func (e *NativeEngine) Supports(insp *engine.Inspection) engine.SupportScore {
	if insp != nil && insp.Engine == EngineNative {
		return engine.SupportNative
	}
	return engine.SupportNone
}

func (e *NativeEngine) Extract(ctx context.Context, req *engine.ExtractRequest) (*engine.ExtractResult, error) {
	return e.shim.extract(ctx, req, false)
}

func (e *PDFEngine) Name() string    { return EnginePDF }
func (e *PDFEngine) Version() string { return e.shim.engineVersion(EnginePDF) }

func (e *PDFEngine) Inspect(ctx context.Context, src canonical.Source, path string) (*engine.Inspection, error) {
	out, err := e.shim.inspect(ctx, src, path)
	if err != nil {
		return nil, err
	}
	if out.Engine != EnginePDF {
		return nil, fmt.Errorf("%w: %s is handled by %s", engine.ErrUnsupported, src.Filename, out.Engine)
	}
	return e.shim.toInspection(src, out), nil
}

func (e *PDFEngine) Supports(insp *engine.Inspection) engine.SupportScore {
	if insp != nil && insp.Engine == EnginePDF {
		return engine.SupportNative
	}
	return engine.SupportNone
}

func (e *PDFEngine) Extract(ctx context.Context, req *engine.ExtractRequest) (*engine.ExtractResult, error) {
	return e.shim.extract(ctx, req, true)
}
