// Command dolico is the document processing API server.
//
// Everything runs in one process with no external dependencies: uploads land
// in a filesystem blob store, jobs live in memory, and extraction shells out
// to the dolico-rs binary. There is no database and nothing survives a
// restart -- see README.md for what that does and does not establish.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tendant/dolico/internal/api"
	"github.com/tendant/dolico/internal/blob"
	"github.com/tendant/dolico/internal/cache"
	"github.com/tendant/dolico/internal/canonical"
	"github.com/tendant/dolico/internal/config"
	"github.com/tendant/dolico/internal/engine"
	"github.com/tendant/dolico/internal/engine/imagedoc"
	"github.com/tendant/dolico/internal/engine/ocrstub"
	"github.com/tendant/dolico/internal/engine/paddleocr"
	"github.com/tendant/dolico/internal/engine/quality"
	"github.com/tendant/dolico/internal/engine/router"
	"github.com/tendant/dolico/internal/engine/rustshim"
	"github.com/tendant/dolico/internal/jobs"
	"github.com/tendant/dolico/internal/render"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	store, err := blob.New(cfg.DataDir)
	if err != nil {
		return err
	}
	shimTemp := filepath.Join(cfg.DataDir, "tmp")
	if err := os.MkdirAll(shimTemp, 0o755); err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}

	// The shim is verified at startup, not at first upload: a deployment
	// missing its extraction binary should fail loudly now rather than accept
	// traffic it cannot serve.
	shim, err := rustshim.New(cfg.ShimPath, cfg.ShimTimeout, shimTemp)
	if err != nil {
		return fmt.Errorf("%w\nbuild it with: make build", err)
	}
	native, pdf := rustshim.Engines(shim)
	ocr, err := ocrEngine(cfg, log)
	if err != nil {
		return err
	}
	// The image inspector carries no dependency of its own: it reads a magic
	// number and hands the page to whatever OCR tier is configured, so a
	// stubbed OCR means images come back as placeholders rather than text.
	registry := engine.NewRegistry(native, pdf, imagedoc.New(), ocr)
	pageCache := cache.New(50_000)
	vision := visionEngine(cfg, ocr, log)

	rt := router.New(registry, ocr, pageCache, router.Options{
		OCRThreshold:       cfg.OCRThreshold,
		VisionThreshold:    cfg.VisionThreshold,
		VisionMaxPages:     cfg.VisionMaxPages,
		VisionProbe:        cfg.VisionEnabled && cfg.VisionProbe,
		VisionDisagreement: cfg.VisionDisagreement,
		Weights:            quality.DefaultWeights,
		Logger:             log,
	}).WithVision(vision)

	jobStore := jobs.NewStore(cfg.Workers, cfg.Workers*16, processDocument(store, rt, log), log)

	server := &http.Server{
		Addr: cfg.Addr,
		Handler: api.New(api.Deps{
			Store:          store,
			Jobs:           jobStore,
			Registry:       registry,
			Vision:         vision,
			Cache:          pageCache,
			Log:            log,
			MaxUploadBytes: cfg.MaxUploadBytes,
			ShimPath:       shim.Binary(),
			WaitTimeout:    cfg.ShimTimeout + 30*time.Second,
		}).Routes(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	log.Info("starting",
		"addr", cfg.Addr,
		"data_dir", cfg.DataDir,
		"workers", cfg.Workers,
		"shim", shim.Binary(),
		"ocr_engine", ocr.Name(),
		"ocr_threshold", cfg.OCRThreshold,
		"schema_version", canonical.SchemaVersion,
		"pipeline_version", canonical.PipelineVersion)
	log.Warn("no persistence: blobs are under a temp directory and jobs are in memory")
	if ocr.Name() == ocrstub.Name {
		log.Warn("OCR is stubbed: scanned pages will not be read. " +
			"Start python/ocr-service and set DOLICO_OCR_URL for real OCR")
	}

	errs := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errs:
		return err
	case sig := <-stop:
		log.Info("shutting down", "signal", sig.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Error("http shutdown", "error", err)
	}
	if err := jobStore.Shutdown(ctx); err != nil {
		log.Error("worker shutdown", "error", err)
	}
	return nil
}

// ocrEngine returns the real OCR tier when one is configured, and the stub
// otherwise.
//
// The fallback is what keeps this a single-binary service with no Python
// requirement: `make run`, `make test` and `make e2e` all work with no OCR
// service present, and scanned pages come back visibly marked as unread rather
// than silently empty. Setting DOLICO_OCR_URL swaps in the real tier with no
// other change.
func ocrEngine(cfg *config.Config, log *slog.Logger) (engine.Engine, error) {
	if cfg.OCRURL == "" {
		return ocrstub.New(), nil
	}
	ocr, err := paddleocr.New(cfg.OCRURL,
		paddleocr.WithTimeout(cfg.OCRTimeout),
		paddleocr.WithConcurrency(cfg.OCRConcurrency),
		paddleocr.WithLogger(log))
	if err != nil {
		// Configuring an OCR service and then starting without it would mean
		// silently serving stub text under a configuration that says
		// otherwise. Refusing to start is the honest failure.
		return nil, fmt.Errorf("%w\nstart it with: make ocr", err)
	}
	log.Info("OCR tier connected",
		"url", ocr.BaseURL(), "engine", ocr.Name(), "tier", ocr.Tier(),
		"version", ocr.Version(), "concurrency", ocr.Concurrency())
	if ocr.Tier() == "text" {
		log.Warn("OCR is running text-line only: scanned tables will arrive as flat text. " +
			"Install the layout tier with `uv sync --extra structure` in python/ocr-service")
	}
	return ocr, nil
}

// visionEngine returns the third tier, or nil to run with two.
//
// Three things must all hold: the operator asked for it, an OCR service is
// configured (the vision tier is reached through it), and that service reports
// MinerU installed. Any of them missing is a normal two-tier run, not an
// error — Tier 3 is an optional escalation, not a dependency.
func visionEngine(cfg *config.Config, ocr engine.Engine, log *slog.Logger) engine.Engine {
	if !cfg.VisionEnabled {
		return nil
	}
	client, ok := ocr.(*paddleocr.Engine)
	if !ok {
		log.Warn("vision tier requested but no OCR service is configured; running with two tiers")
		return nil
	}
	v := paddleocr.NewVision(client)
	if v == nil {
		log.Warn("vision tier requested but the OCR service reports MinerU is not installed; " +
			"run `uv sync --extra vision` in python/ocr-service")
		return nil
	}
	// No version here: MinerU loads on first use, so the service cannot report
	// one yet. It appears in provenance once a page has actually been read.
	log.Info("vision tier connected",
		"engine", v.Name(),
		"threshold", cfg.VisionThreshold,
		"max_pages", cfg.VisionMaxPages,
		"probe", cfg.VisionProbe,
		"disagreement", cfg.VisionDisagreement)
	if cfg.VisionProbe {
		log.Info("vision probe on: every document with scanned pages pays one " +
			"vision call, and disagreement escalates the rest. " +
			"Set DOLICO_VISION_PROBE=0 to escalate only on the quality threshold")
	}
	return v
}

// processDocument is the work each job runs: route the document, render the
// Markdown view, commit both plus any assets to the store.
func processDocument(store *blob.Store, rt *router.Router, log *slog.Logger) jobs.Work {
	return func(ctx context.Context, job *jobs.Job) (int, string, error) {
		// The document id is the content digest, so identical bytes are
		// literally the same document. If it has already been processed by
		// this pipeline and engine set, there is nothing to redo -- this is
		// the content-hash caching the design calls for, at whole-document
		// granularity, with the page cache handling partial reuse underneath.
		if pages, ok := storedPageCount(store, job.DocumentID, log); ok {
			log.Info("document already processed",
				"document_id", job.DocumentID, "pages", pages, "trace_id", job.TraceID)
			return pages, "", nil
		}

		assetsDir, cleanup, err := store.TempDir("assets")
		if err != nil {
			return 0, "internal", err
		}
		defer cleanup()

		doc, err := rt.Process(ctx, router.Request{
			DocumentID: job.DocumentID,
			TraceID:    job.TraceID,
			Source: canonical.Source{
				Filename:  job.Filename,
				MediaType: job.MediaType,
				SHA256:    job.SHA256,
				SizeBytes: job.SizeBytes,
			},
			Path:      store.Path(job.SHA256),
			AssetsDir: assetsDir,
		})
		if err != nil {
			return 0, errorKind(err), err
		}

		// Assets arrive as files the shim wrote into the scratch directory.
		// Moving them into the content-addressed store now is what lets the
		// scratch directory be deleted and gives the API a reference it can
		// serve without trusting a path from a request.
		for i := range doc.Assets {
			ref := doc.Assets[i].BlobRef
			// A cached extraction returns refs that were already committed on
			// an earlier run; re-storing them would fail on a path that no
			// longer exists.
			if store.Exists(ref) {
				continue
			}
			digest, size, err := store.PutFile(ref)
			if err != nil {
				log.Error("store asset", "asset", doc.Assets[i].ID, "error", err)
				continue
			}
			doc.Assets[i].BlobRef = digest
			doc.Assets[i].SizeBytes = size
		}

		data, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return 0, "internal", fmt.Errorf("marshal canonical: %w", err)
		}
		if err := store.WriteDerived(doc.ID, "canonical.json", data); err != nil {
			return 0, "internal", err
		}
		if err := store.WriteDerived(doc.ID, "document.md", []byte(render.Markdown(doc))); err != nil {
			return 0, "internal", err
		}
		return len(doc.Pages), "", nil
	}
}

// storedPageCount reports whether this document has already been processed by
// the current schema and pipeline, and how many pages it has.
//
// A stored document from an older schema or pipeline is treated as absent so
// it gets reprocessed: serving a document built by different rules than the
// ones currently in force is worse than doing the work again.
func storedPageCount(store *blob.Store, docID string, log *slog.Logger) (int, bool) {
	data, err := store.ReadDerived(docID, "canonical.json")
	if err != nil {
		return 0, false
	}
	var doc canonical.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return 0, false
	}
	if doc.SchemaVersion != canonical.SchemaVersion ||
		doc.Trace.PipelineVersion != canonical.PipelineVersion {
		return 0, false
	}
	// A document stored while a tier was down is a record of that outage, not
	// a finished document. Without this the content hash makes the outage
	// permanent for those bytes: every later upload of the same file is served
	// the empty pages, and the only cures are deleting the document or bumping
	// the pipeline version.
	if reason, incomplete := doc.Incomplete(); incomplete {
		log.Info("reprocessing a document stored with missing pages",
			"document_id", docID, "reason", reason)
		return 0, false
	}
	if _, err := store.ReadDerived(docID, "document.md"); err != nil {
		// The JSON is there but the Markdown view is not: a previous run was
		// interrupted between the two writes. Redo it.
		return 0, false
	}
	return len(doc.Pages), true
}

// errorKind classifies a routing failure for the API's status mapping. The
// classification itself lives with the errors, in package engine, so the
// inspect endpoint and the extraction path cannot drift apart on what to call
// the same failure.
func errorKind(err error) string { return engine.Kind(err) }
