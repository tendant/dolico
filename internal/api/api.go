// Package api is the HTTP surface.
//
// Structured JSON is the primary representation; Markdown is generated from it
// as a view. That ordering is deliberate: an API whose primary output is
// Markdown pushes every consumer into re-parsing prose to recover structure
// the pipeline already had.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tendant/dolico/internal/blob"
	"github.com/tendant/dolico/internal/cache"
	"github.com/tendant/dolico/internal/canonical"
	"github.com/tendant/dolico/internal/engine"
	"github.com/tendant/dolico/internal/jobs"
)

// Deps is what the API needs from the rest of the service.
type Deps struct {
	Store    *blob.Store
	Jobs     *jobs.Store
	Registry *engine.Registry
	// Vision is the escalation tier, which is deliberately not in the
	// registry — nothing selects it, the router calls it directly. It is
	// carried here anyway so that /v1/engines can report every engine that
	// may have touched a document rather than only the selectable ones.
	Vision         engine.Engine
	Cache          *cache.Cache
	Log            *slog.Logger
	MaxUploadBytes int64
	ShimPath       string
	// WaitTimeout bounds a `?wait=true` request.
	WaitTimeout time.Duration
}

// Server holds the API dependencies.
type Server struct{ deps Deps }

// New builds the server.
func New(deps Deps) *Server {
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	if deps.WaitTimeout <= 0 {
		deps.WaitTimeout = 5 * time.Minute
	}
	return &Server{deps: deps}
}

// Routes returns the HTTP handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/documents", s.uploadDocument)
	mux.HandleFunc("POST /v1/inspect", s.inspectDocument)
	mux.HandleFunc("GET /v1/jobs", s.listJobs)
	mux.HandleFunc("GET /v1/jobs/{id}", s.getJob)
	// "{id}.md" is not a legal ServeMux pattern -- a wildcard has to be a
	// whole path segment -- so the extension is dispatched inside the handler.
	mux.HandleFunc("GET /v1/documents/{id}", s.getDocument)
	mux.HandleFunc("DELETE /v1/documents/{id}", s.deleteDocument)
	mux.HandleFunc("GET /v1/documents/{id}/assets/{asset}", s.getAsset)
	mux.HandleFunc("GET /v1/engines", s.listEngines)
	mux.HandleFunc("GET /healthz", s.health)
	return s.withTrace(mux)
}

// withTrace stamps every response with a trace id and logs the request.
func (s *Server) withTrace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID := r.Header.Get("X-Trace-Id")
		if traceID == "" {
			traceID = jobs.NewID("trace")
		}
		w.Header().Set("X-Trace-Id", traceID)
		ctx := context.WithValue(r.Context(), traceKey{}, traceID)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		started := time.Now()
		next.ServeHTTP(rec, r.WithContext(ctx))
		s.deps.Log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(started).Milliseconds(),
			"trace_id", traceID)
	})
}

type traceKey struct{}

func traceID(ctx context.Context) string {
	if v, ok := ctx.Value(traceKey{}).(string); ok {
		return v
	}
	return ""
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

type uploadResponse struct {
	JobID      string `json:"job_id"`
	DocumentID string `json:"document_id"`
	SHA256     string `json:"sha256"`
	TraceID    string `json:"trace_id"`
	State      string `json:"state"`
}

// storedRequest asks for extraction of bytes the store already holds, named by
// the digest a prior inspection returned.
type storedRequest struct {
	SHA256    string `json:"sha256"`
	Filename  string `json:"filename"`
	MediaType string `json:"media_type"`
}

func (s *Server) uploadDocument(w http.ResponseWriter, r *http.Request) {
	// A JSON body means "extract what you already have". Inspection stores the
	// bytes, so a caller that inspected first and then decided to proceed
	// refers to the digest instead of sending the same hundred megabytes
	// twice.
	if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/json") {
		s.extractStored(w, r)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.deps.MaxUploadBytes)

	file, header, err := r.FormFile("file")
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.fail(w, r, http.StatusRequestEntityTooLarge, "upload_too_large",
				fmt.Sprintf("upload exceeds the %d byte limit", s.deps.MaxUploadBytes))
			return
		}
		s.fail(w, r, http.StatusBadRequest, "bad_request",
			"expected a multipart form with a 'file' field")
		return
	}
	defer file.Close()

	digest, size, err := s.deps.Store.Put(file)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "storage", err.Error())
		return
	}
	if size == 0 {
		s.fail(w, r, http.StatusBadRequest, "empty_upload", "the uploaded file is empty")
		return
	}

	job := &jobs.Job{
		TraceID:   traceID(r.Context()),
		Filename:  filepath.Base(header.Filename),
		MediaType: mediaType(header.Filename, header.Header.Get("Content-Type")),
		SHA256:    digest,
		SizeBytes: size,
		// Addressing the document by its content digest means uploading the
		// same bytes twice resolves to the same document, and the page cache
		// makes the second run nearly free.
		DocumentID: digest,
	}
	s.submitJob(w, r, job)
}

// extractStored starts a job for bytes already in the store.
func (s *Server) extractStored(w http.ResponseWriter, r *http.Request) {
	var req storedRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		s.fail(w, r, http.StatusBadRequest, "bad_request",
			"expected a JSON body with a 'sha256' field")
		return
	}
	if req.SHA256 == "" {
		s.fail(w, r, http.StatusBadRequest, "bad_request", "sha256 is required")
		return
	}
	// A digest naming bytes this store does not hold is not a 404 on some
	// resource the caller can go and fetch: it means whatever inspected those
	// bytes was a different deployment, or the blob has since been swept. Both
	// are fixed by uploading the file again, which is what the message says.
	if !s.deps.Store.Exists(req.SHA256) {
		s.fail(w, r, http.StatusNotFound, "not_found",
			fmt.Sprintf("no stored document with digest %s; upload it again", req.SHA256))
		return
	}
	f, err := s.deps.Store.Open(req.SHA256)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "storage", err.Error())
		return
	}
	info, err := f.Stat()
	f.Close()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "storage", err.Error())
		return
	}

	filename := filepath.Base(req.Filename)
	job := &jobs.Job{
		TraceID:  traceID(r.Context()),
		Filename: filename,
		// The filename is a detection hint for formats with no content
		// signature, so it is carried through from the inspection rather than
		// re-derived from a digest that has no extension.
		MediaType:  mediaType(filename, req.MediaType),
		SHA256:     req.SHA256,
		SizeBytes:  info.Size(),
		DocumentID: req.SHA256,
	}
	s.submitJob(w, r, job)
}

// submitJob queues a job and answers, either immediately or after it finishes.
func (s *Server) submitJob(w http.ResponseWriter, r *http.Request, job *jobs.Job) {
	if err := s.deps.Jobs.Submit(job); err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "queue_full",
			"the processing queue is full; retry shortly")
		return
	}

	if r.URL.Query().Get("wait") == "true" {
		ctx, cancel := context.WithTimeout(r.Context(), s.deps.WaitTimeout)
		defer cancel()
		done, err := s.deps.Jobs.Wait(ctx, job.ID)
		if err != nil {
			s.fail(w, r, http.StatusGatewayTimeout, "timeout",
				fmt.Sprintf("job %s did not finish within %s", job.ID, s.deps.WaitTimeout))
			return
		}
		if done.State == jobs.StateFailed {
			s.failJob(w, r, done)
			return
		}
		s.writeDocument(w, r, done.DocumentID)
		return
	}

	s.writeJSON(w, http.StatusAccepted, uploadResponse{
		JobID:      job.ID,
		DocumentID: job.DocumentID,
		SHA256:     job.SHA256,
		TraceID:    job.TraceID,
		State:      string(jobs.StateQueued),
	})
}

// inspectResponse is what a document is, without extracting it.
type inspectResponse struct {
	DocumentID string             `json:"document_id"`
	SHA256     string             `json:"sha256"`
	Filename   string             `json:"filename"`
	MediaType  string             `json:"media_type"`
	SizeBytes  int64              `json:"size_bytes"`
	Engine     string             `json:"engine"`
	PageCount  int                `json:"page_count"`
	PageKind   canonical.PageKind `json:"page_kind"`
	// PageTypes counts pages by classification, which is what a caller
	// deciding whether to proceed actually reasons about: fifty scanned pages
	// and fifty text pages cost different amounts of very different work.
	PageTypes map[string]int     `json:"page_types,omitempty"`
	Metadata  canonical.Metadata `json:"metadata"`
	TraceID   string             `json:"trace_id"`
}

// inspectDocument answers what a document is without extracting it.
//
// This exists because "how big is this" is a question worth being able to ask
// before paying for the answer. Inspection is the cheap step by design -- the
// PDF inspector reads structure without rendering, and the native engines read
// a container's manifest -- so a caller can refuse a 500-page document in
// milliseconds instead of discovering its size after the extraction it was
// trying to avoid.
//
// The bytes are stored, not discarded: the store is content-addressed, so the
// digest returned here is the document id, and extracting it afterwards costs
// one JSON call rather than a second upload of the same file.
func (s *Server) inspectDocument(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.deps.MaxUploadBytes)

	file, header, err := r.FormFile("file")
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.fail(w, r, http.StatusRequestEntityTooLarge, "upload_too_large",
				fmt.Sprintf("upload exceeds the %d byte limit", s.deps.MaxUploadBytes))
			return
		}
		s.fail(w, r, http.StatusBadRequest, "bad_request",
			"expected a multipart form with a 'file' field")
		return
	}
	defer file.Close()

	digest, size, err := s.deps.Store.Put(file)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "storage", err.Error())
		return
	}
	if size == 0 {
		s.fail(w, r, http.StatusBadRequest, "empty_upload", "the uploaded file is empty")
		return
	}

	src := canonical.Source{
		Filename:  filepath.Base(header.Filename),
		MediaType: mediaType(header.Filename, header.Header.Get("Content-Type")),
		SHA256:    digest,
		SizeBytes: size,
	}
	insp, eng, err := s.deps.Registry.Inspect(r.Context(), src, s.deps.Store.Path(digest))
	if err != nil {
		kind := engine.Kind(err)
		code := http.StatusInternalServerError
		switch kind {
		case "unsupported":
			code = http.StatusUnsupportedMediaType
		case "malformed", "encrypted":
			code = http.StatusUnprocessableEntity
		}
		s.fail(w, r, code, kind, err.Error())
		return
	}

	types := make(map[string]int, 4)
	for _, p := range insp.Pages {
		types[string(p.Classification.Type)]++
	}

	s.writeJSON(w, http.StatusOK, inspectResponse{
		DocumentID: digest,
		SHA256:     digest,
		Filename:   src.Filename,
		MediaType:  src.MediaType,
		SizeBytes:  size,
		Engine:     eng.Name(),
		PageCount:  insp.PageCount,
		PageKind:   insp.PageKind,
		PageTypes:  types,
		Metadata:   insp.Metadata,
		TraceID:    traceID(r.Context()),
	})
}

// deleteDocument removes a document and everything derived from it.
//
// This is what makes a retention policy elsewhere true. dolico holds the
// uploaded bytes and the extraction; whoever holds the customer relationship
// holds the reason to delete them, and knows when. So the decision lives there
// and the capability lives here.
//
// Idempotent by design: deleting something already gone answers 204. The
// caller is a sweep that retries, and it should be able to run twice without
// having to distinguish "I deleted it" from "it was already deleted" -- both
// mean the document is not on this disk.
func (s *Server) deleteDocument(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validDocumentID(id) {
		s.fail(w, r, http.StatusBadRequest, "bad_request", "not a document id")
		return
	}
	if err := s.deps.Store.Remove(id); err != nil {
		s.fail(w, r, http.StatusInternalServerError, "storage", err.Error())
		return
	}
	// The disk is not the only place the document lives.
	dropped := s.deps.Cache.Forget(id)
	s.deps.Log.Info("document deleted",
		"document_id", id, "cache_entries_dropped", dropped, "trace_id", traceID(r.Context()))
	w.WriteHeader(http.StatusNoContent)
}

// validDocumentID guards the path segment before it reaches the filesystem.
// Document ids are content digests, so anything else is a caller error rather
// than a document that happens to be missing.
func validDocumentID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.deps.Jobs.Get(r.PathValue("id"))
	if err != nil {
		s.fail(w, r, http.StatusNotFound, "not_found", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, job)
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"jobs": s.deps.Jobs.List()})
}

// getDocument serves the canonical JSON, or the Markdown view when the id
// carries a ".md" extension.
func (s *Server) getDocument(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if base, ok := strings.CutSuffix(id, ".md"); ok {
		s.writeMarkdown(w, r, base)
		return
	}
	s.writeDocument(w, r, strings.TrimSuffix(id, ".json"))
}

func (s *Server) writeDocument(w http.ResponseWriter, r *http.Request, docID string) {
	data, err := s.deps.Store.ReadDerived(docID, "canonical.json")
	if err != nil {
		s.fail(w, r, http.StatusNotFound, "not_found",
			fmt.Sprintf("no canonical document for %s", docID))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) writeMarkdown(w http.ResponseWriter, r *http.Request, docID string) {
	data, err := s.deps.Store.ReadDerived(docID, "document.md")
	if err != nil {
		s.fail(w, r, http.StatusNotFound, "not_found",
			fmt.Sprintf("no markdown for %s", docID))
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) getAsset(w http.ResponseWriter, r *http.Request) {
	docID, assetID := r.PathValue("id"), r.PathValue("asset")

	data, err := s.deps.Store.ReadDerived(docID, "canonical.json")
	if err != nil {
		s.fail(w, r, http.StatusNotFound, "not_found", "unknown document")
		return
	}
	var doc canonical.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		s.fail(w, r, http.StatusInternalServerError, "corrupt", err.Error())
		return
	}
	for _, asset := range doc.Assets {
		if asset.ID != assetID {
			continue
		}
		// Serve by digest through the store rather than by joining a path from
		// the request: BlobRef is a digest precisely so a request cannot name
		// a file outside the store.
		f, err := s.deps.Store.Open(asset.BlobRef)
		if err != nil {
			s.fail(w, r, http.StatusNotFound, "not_found", "asset bytes are missing")
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", asset.MediaType)
		http.ServeContent(w, r, asset.ID, time.Time{}, f)
		return
	}
	s.fail(w, r, http.StatusNotFound, "not_found",
		fmt.Sprintf("document %s has no asset %s", docID, assetID))
}

func (s *Server) listEngines(w http.ResponseWriter, r *http.Request) {
	type engineInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	out := make([]engineInfo, 0)
	for _, e := range s.deps.Registry.All() {
		out = append(out, engineInfo{Name: e.Name(), Version: e.Version()})
	}
	// The vision tier is not in the registry, but it does produce pages, and
	// an endpoint that claims to list this service's engines while omitting
	// one that reads documents is both wrong and — because MinerU's license
	// requires an online service to say it uses MinerU — a compliance gap.
	if s.deps.Vision != nil {
		out = append(out, engineInfo{
			Name: s.deps.Vision.Name(), Version: s.deps.Vision.Version(),
		})
	}
	hits, misses, size := s.deps.Cache.Stats()
	s.writeJSON(w, http.StatusOK, map[string]any{
		"engines":          out,
		"schema_version":   canonical.SchemaVersion,
		"pipeline_version": canonical.PipelineVersion,
		"cache": map[string]any{
			"hits": hits, "misses": misses, "pages": size,
		},
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	// Liveness includes the shim: a service that accepts uploads it cannot
	// process is not healthy, and finding out at the first upload is worse
	// than finding out at the health check.
	status, detail := "ok", ""
	if st, err := os.Stat(s.deps.ShimPath); err != nil || st.IsDir() {
		status, detail = "degraded", fmt.Sprintf("shim not executable at %s", s.deps.ShimPath)
	}
	code := http.StatusOK
	if status != "ok" {
		code = http.StatusServiceUnavailable
	}
	s.writeJSON(w, code, map[string]any{
		"status": status, "detail": detail, "shim": s.deps.ShimPath,
	})
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

type errorResponse struct {
	Error   string `json:"error"`
	Kind    string `json:"kind"`
	TraceID string `json:"trace_id,omitempty"`
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, code int, kind, msg string) {
	s.writeJSON(w, code, errorResponse{Error: msg, Kind: kind, TraceID: traceID(r.Context())})
}

// failJob maps a failed job's error kind onto the status code that describes
// it: an unsupported format is the client's problem, an engine crash is ours.
func (s *Server) failJob(w http.ResponseWriter, r *http.Request, job jobs.Job) {
	code := http.StatusInternalServerError
	switch job.ErrorKind {
	case "unsupported":
		code = http.StatusUnsupportedMediaType
	case "malformed", "encrypted":
		code = http.StatusUnprocessableEntity
	}
	s.writeJSON(w, code, errorResponse{
		Error: job.Error, Kind: job.ErrorKind, TraceID: job.TraceID,
	})
}

func (s *Server) writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(payload); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		s.deps.Log.Error("write response", "error", err)
	}
}

// mediaType prefers the extension over the browser's guess: browsers report
// application/octet-stream for anything they do not recognize, which is most
// office formats.
func mediaType(filename, declared string) string {
	if ext := filepath.Ext(filename); ext != "" {
		if byExt := mime.TypeByExtension(strings.ToLower(ext)); byExt != "" {
			return byExt
		}
	}
	if declared != "" && declared != "application/octet-stream" {
		return declared
	}
	return "application/octet-stream"
}
