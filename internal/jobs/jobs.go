// Package jobs is an in-memory job store and worker pool.
//
// The design calls for an in-process queue first and NATS later. This is that
// first step, and it is honest about its limits: jobs live in a map, the queue
// is a channel, and restarting the process loses everything in flight. What it
// establishes is the shape -- a job id handed back immediately, state
// observable while work proceeds, results addressed separately from the job --
// which does not change when the queue moves out of process.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned for an unknown job id.
var ErrNotFound = errors.New("jobs: not found")

// ErrQueueFull is returned when the queue cannot accept more work.
var ErrQueueFull = errors.New("jobs: queue is full")

// State is a job's position in the pipeline.
type State string

const (
	StateQueued     State = "queued"
	StateInspecting State = "inspecting"
	StateExtracting State = "extracting"
	StateStoring    State = "storing"
	StateDone       State = "done"
	StateFailed     State = "failed"
)

// Terminal reports whether no further transition will occur.
func (s State) Terminal() bool { return s == StateDone || s == StateFailed }

// Job is one document's processing record.
type Job struct {
	ID         string     `json:"id"`
	TraceID    string     `json:"trace_id"`
	DocumentID string     `json:"document_id"`
	Filename   string     `json:"filename"`
	MediaType  string     `json:"media_type"`
	SHA256     string     `json:"sha256"`
	SizeBytes  int64      `json:"size_bytes"`
	State      State      `json:"state"`
	Error      string     `json:"error,omitempty"`
	ErrorKind  string     `json:"error_kind,omitempty"`
	Pages      int        `json:"pages"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Work is what a worker runs for one job. It returns the number of pages
// produced, and an error kind the API can map to a status code.
type Work func(ctx context.Context, job *Job) (pages int, errKind string, err error)

// Store holds jobs and runs them on a fixed pool of workers.
type Store struct {
	mu   sync.RWMutex
	jobs map[string]*Job
	// waiters lets a synchronous caller block on completion without polling.
	waiters map[string][]chan struct{}

	queue  chan string
	work   Work
	log    *slog.Logger
	wg     sync.WaitGroup
	cancel context.CancelFunc
	closed sync.Once
}

// NewStore starts a pool of workers.
func NewStore(workers, queueDepth int, work Work, log *slog.Logger) *Store {
	if workers < 1 {
		workers = 1
	}
	if queueDepth < workers {
		queueDepth = workers * 8
	}
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Store{
		jobs:    make(map[string]*Job),
		waiters: make(map[string][]chan struct{}),
		queue:   make(chan string, queueDepth),
		work:    work,
		log:     log,
		cancel:  cancel,
	}
	for i := 0; i < workers; i++ {
		s.wg.Add(1)
		go s.worker(ctx, i)
	}
	return s
}

// Submit enqueues a job and returns immediately.
//
// A full queue is refused rather than blocking the HTTP handler: backpressure
// the client can see beats a request that hangs until it times out.
func (s *Store) Submit(job *Job) error {
	job.ID = newID("job")
	if job.TraceID == "" {
		job.TraceID = newID("trace")
	}
	job.State = StateQueued
	job.CreatedAt = time.Now().UTC()

	s.mu.Lock()
	s.jobs[job.ID] = job
	s.mu.Unlock()

	select {
	case s.queue <- job.ID:
		return nil
	default:
		s.fail(job.ID, "queue_full", ErrQueueFull)
		return ErrQueueFull
	}
}

// Get returns a copy of a job.
func (s *Store) Get(id string) (Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.jobs[id]
	if !ok {
		return Job{}, fmt.Errorf("%w: job %s", ErrNotFound, id)
	}
	return *job, nil
}

// List returns all jobs, newest first.
func (s *Store) List() []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, *j)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Wait blocks until the job reaches a terminal state, the context is
// cancelled, or the job is unknown. It is what backs `?wait=true`.
func (s *Store) Wait(ctx context.Context, id string) (Job, error) {
	s.mu.Lock()
	job, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return Job{}, fmt.Errorf("%w: job %s", ErrNotFound, id)
	}
	if job.State.Terminal() {
		out := *job
		s.mu.Unlock()
		return out, nil
	}
	done := make(chan struct{})
	s.waiters[id] = append(s.waiters[id], done)
	s.mu.Unlock()

	select {
	case <-done:
		return s.Get(id)
	case <-ctx.Done():
		return Job{}, ctx.Err()
	}
}

func (s *Store) worker(ctx context.Context, n int) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case id, ok := <-s.queue:
			if !ok {
				return
			}
			s.run(ctx, id, n)
		}
	}
}

func (s *Store) run(ctx context.Context, id string, worker int) {
	s.mu.Lock()
	job, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	job.State = StateInspecting
	job.StartedAt = &now
	snapshot := *job
	s.mu.Unlock()

	log := s.log.With("job_id", id, "trace_id", snapshot.TraceID, "worker", worker)
	log.Info("job started", "filename", snapshot.Filename, "sha256", snapshot.SHA256)

	// A panic in a parser or an engine must kill the job, not the worker: the
	// pool would otherwise shrink silently until nothing processed at all.
	var (
		pages   int
		errKind string
		err     error
	)
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("panic: %v", rec)
				errKind = "internal"
				log.Error("job panicked", "panic", rec)
			}
		}()
		pages, errKind, err = s.work(ctx, &snapshot)
	}()

	if err != nil {
		s.fail(id, errKind, err)
		log.Error("job failed", "error", err, "kind", errKind)
		return
	}
	s.complete(id, snapshot.DocumentID, pages)
	log.Info("job done", "pages", pages, "document_id", snapshot.DocumentID)
}

// SetState advances a running job's state, for progress reporting.
func (s *Store) SetState(id string, state State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if job, ok := s.jobs[id]; ok && !job.State.Terminal() {
		job.State = state
	}
}

func (s *Store) complete(id, documentID string, pages int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return
	}
	now := time.Now().UTC()
	job.State = StateDone
	job.DocumentID = documentID
	job.Pages = pages
	job.FinishedAt = &now
	s.notify(id)
}

func (s *Store) fail(id, kind string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return
	}
	now := time.Now().UTC()
	job.State = StateFailed
	job.Error = err.Error()
	job.ErrorKind = kind
	job.FinishedAt = &now
	s.notify(id)
}

// notify wakes everyone blocked in Wait. Callers must hold the lock.
func (s *Store) notify(id string) {
	for _, ch := range s.waiters[id] {
		close(ch)
	}
	delete(s.waiters, id)
}

// Shutdown stops the workers and waits for in-flight jobs, up to ctx's
// deadline.
func (s *Store) Shutdown(ctx context.Context) error {
	s.closed.Do(func() { close(s.queue) })
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// Deadline hit: cancel the workers' context so in-flight subprocesses
		// are killed rather than outliving the process that spawned them.
		s.cancel()
		return ctx.Err()
	}
}

// newID returns a short random identifier with a readable prefix.
func newID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; if it somehow does, a
		// timestamp-derived id is far better than a panic in a request path.
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

// NewID exposes id generation for callers that need one before submitting.
func NewID(prefix string) string { return newID(prefix) }
