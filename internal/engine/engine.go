// Package engine defines the extraction engine contract and the registry that
// the router selects from.
//
// The contract is deliberately narrow: an engine inspects bytes, says how well
// it can handle them, and extracts some or all pages. Everything else --
// caching, routing, escalation, storage -- lives outside, so adding PaddleOCR
// or a vision model later means implementing three methods and registering,
// with no change to the router.
package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/tendant/dolico/internal/canonical"
)

// Errors engines return so the router can distinguish "not my job" from
// "this document is broken" from "I failed".
var (
	// ErrUnsupported means the engine cannot handle this input at all. The
	// router treats it as a routing miss, not a failure.
	ErrUnsupported = errors.New("engine: unsupported input")
	// ErrMalformed means the input is structurally unusable. Retrying with
	// another engine of the same class will not help.
	ErrMalformed = errors.New("engine: malformed input")
	// ErrEncrypted means the document is password-protected.
	ErrEncrypted = errors.New("engine: encrypted input")
)

// SupportScore is how well an engine believes it can handle an input, 0..100.
// The router picks the highest scorer. Scores are advisory; an engine that
// returns a high score and then fails is a bug in that engine.
type SupportScore int

const (
	// SupportNone means the engine must not be selected.
	SupportNone SupportScore = 0
	// SupportFallback is a last-resort engine that will produce something.
	SupportFallback SupportScore = 10
	// SupportGeneric means the engine handles this class of input adequately.
	SupportGeneric SupportScore = 50
	// SupportNative means the engine is the purpose-built handler for it.
	SupportNative SupportScore = 90
)

// Inspection is the cheap, pre-extraction look at a document: enough to route
// on, and nothing more. For PDFs this is the per-page classification that
// makes per-page routing possible; for native formats it is just the format.
type Inspection struct {
	Source    canonical.Source
	PageCount int
	PageKind  canonical.PageKind
	// Pages is the per-page classification, 1-indexed by Page.Number. It is
	// empty for formats where the distinction is meaningless, in which case
	// the router treats the whole document as one unit.
	Pages []canonical.Page
	// Metadata is whatever the inspection cheaply revealed (title, and so on).
	Metadata canonical.Metadata
	// Engine names the engine that produced this inspection.
	Engine string
}

// ExtractRequest asks an engine for some or all pages of a document.
type ExtractRequest struct {
	Source canonical.Source
	// Path is the document's location in the blob store. Engines read from
	// disk rather than from memory so that a subprocess engine can be handed
	// a path instead of a pipe, and so a 400MB PDF is never held twice.
	Path string
	// Pages lists 1-indexed pages to extract. Nil means the whole document.
	// This is the hook that makes "OCR only the pages that need it" possible.
	Pages []int
	// AssetsDir is where the engine writes extracted binary assets. Asset
	// bytes are never returned inline.
	AssetsDir string
	// Inspection is the prior inspection result, when one exists, so the
	// engine need not redo the work.
	Inspection *Inspection
	Config     map[string]string
}

// ExtractResult is what an engine produces: pages in the canonical model, plus
// whatever assets it wrote out.
type ExtractResult struct {
	Pages    []canonical.Page
	Assets   []canonical.Asset
	Metadata canonical.Metadata
	// DurationMS is the engine's own measure of how long it took.
	DurationMS int64
}

// Engine is the contract every extraction backend implements.
type Engine interface {
	// Name is a stable identifier recorded in Provenance.Engine.
	Name() string
	// Version identifies the engine build. It participates in cache keys, so
	// upgrading an engine invalidates exactly the pages that engine produced.
	Version() string
	// Inspect performs the cheap pre-extraction look. Implementations must not
	// do full extraction here.
	Inspect(ctx context.Context, src canonical.Source, path string) (*Inspection, error)
	// Supports scores this engine's fitness for an already-inspected input.
	Supports(insp *Inspection) SupportScore
	// Extract produces canonical pages.
	Extract(ctx context.Context, req *ExtractRequest) (*ExtractResult, error)
}

// Registry holds the available engines and answers "who should handle this".
type Registry struct {
	engines []Engine
}

// NewRegistry builds a registry over the given engines, in no particular
// order; selection is by score, not by registration order.
func NewRegistry(engines ...Engine) *Registry {
	return &Registry{engines: append([]Engine(nil), engines...)}
}

// Add registers an engine.
func (r *Registry) Add(e Engine) { r.engines = append(r.engines, e) }

// All returns the registered engines, sorted by name for stable output.
func (r *Registry) All() []Engine {
	out := append([]Engine(nil), r.engines...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// ByName returns the named engine.
func (r *Registry) ByName(name string) (Engine, error) {
	for _, e := range r.engines {
		if e.Name() == name {
			return e, nil
		}
	}
	return nil, fmt.Errorf("engine %q not registered", name)
}

// Inspect asks every engine to inspect the input and returns the inspection
// from the highest-scoring one.
//
// Engines that return ErrUnsupported are skipped silently: that is a routing
// miss, which is expected and normal. Any other error is collected and
// returned only if no engine succeeds, so one broken engine cannot mask a
// working one.
func (r *Registry) Inspect(ctx context.Context, src canonical.Source, path string) (*Inspection, Engine, error) {
	var (
		best      *Inspection
		bestEng   Engine
		bestScore = SupportNone
		errs      []error
	)
	for _, e := range r.engines {
		insp, err := e.Inspect(ctx, src, path)
		if err != nil {
			if !errors.Is(err, ErrUnsupported) {
				errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			}
			continue
		}
		if score := e.Supports(insp); score > bestScore {
			best, bestEng, bestScore = insp, e, score
		}
	}
	if best == nil {
		if len(errs) > 0 {
			return nil, nil, errors.Join(errs...)
		}
		return nil, nil, fmt.Errorf("%w: no engine accepted %q (%s)", ErrUnsupported, src.Filename, src.MediaType)
	}
	return best, bestEng, nil
}
