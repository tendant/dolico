package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newStore(t *testing.T, workers int, work Work) *Store {
	t.Helper()
	s := NewStore(workers, workers*8, work, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return s
}

func TestJobRunsAndCompletes(t *testing.T) {
	s := newStore(t, 2, func(_ context.Context, job *Job) (int, string, error) {
		job.DocumentID = "doc-" + job.SHA256
		return 3, "", nil
	})
	job := &Job{Filename: "a.pdf", SHA256: "abc"}
	if err := s.Submit(job); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done, err := s.Wait(ctx, job.ID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if done.State != StateDone {
		t.Errorf("state = %s, want done", done.State)
	}
	if done.Pages != 3 {
		t.Errorf("pages = %d, want 3", done.Pages)
	}
	if done.DocumentID != "doc-abc" {
		t.Errorf("document id = %q", done.DocumentID)
	}
	if done.FinishedAt == nil || done.StartedAt == nil {
		t.Error("timestamps should be set on completion")
	}
}

func TestFailedJobRecordsErrorAndKind(t *testing.T) {
	s := newStore(t, 1, func(context.Context, *Job) (int, string, error) {
		return 0, "unsupported", errors.New("no engine accepted it")
	})
	job := &Job{Filename: "x.bin"}
	if err := s.Submit(job); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done, _ := s.Wait(ctx, job.ID)
	if done.State != StateFailed {
		t.Fatalf("state = %s, want failed", done.State)
	}
	if done.ErrorKind != "unsupported" {
		t.Errorf("error kind = %q", done.ErrorKind)
	}
	if done.Error == "" {
		t.Error("error message should be recorded")
	}
}

// A panic in a parser must kill the job, not the worker: the pool would
// otherwise shrink silently until nothing processed at all.
func TestPanicFailsTheJobAndKeepsTheWorkerAlive(t *testing.T) {
	var calls atomic.Int32
	s := newStore(t, 1, func(_ context.Context, job *Job) (int, string, error) {
		if calls.Add(1) == 1 {
			panic("engine exploded")
		}
		return 1, "", nil
	})

	first := &Job{Filename: "boom.pdf"}
	if err := s.Submit(first); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done, _ := s.Wait(ctx, first.ID)
	if done.State != StateFailed {
		t.Fatalf("a panicking job should fail, got %s", done.State)
	}
	if done.ErrorKind != "internal" {
		t.Errorf("error kind = %q, want internal", done.ErrorKind)
	}

	// The same worker must still process the next job.
	second := &Job{Filename: "fine.pdf"}
	if err := s.Submit(second); err != nil {
		t.Fatal(err)
	}
	done2, err := s.Wait(ctx, second.ID)
	if err != nil {
		t.Fatalf("the worker did not survive the panic: %v", err)
	}
	if done2.State != StateDone {
		t.Errorf("second job state = %s, want done", done2.State)
	}
}

func TestWaitReturnsImmediatelyForAFinishedJob(t *testing.T) {
	s := newStore(t, 1, func(context.Context, *Job) (int, string, error) { return 1, "", nil })
	job := &Job{Filename: "a.pdf"}
	if err := s.Submit(job); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.Wait(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	// Second Wait on an already-terminal job must not block.
	quick, quickCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer quickCancel()
	if _, err := s.Wait(quick, job.ID); err != nil {
		t.Errorf("Wait on a finished job should return at once: %v", err)
	}
}

func TestWaitHonorsContextCancellation(t *testing.T) {
	release := make(chan struct{})
	s := newStore(t, 1, func(context.Context, *Job) (int, string, error) {
		<-release
		return 1, "", nil
	})
	defer close(release)

	job := &Job{Filename: "slow.pdf"}
	if err := s.Submit(job); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := s.Wait(ctx, job.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

func TestManyWaitersAreAllReleased(t *testing.T) {
	release := make(chan struct{})
	s := newStore(t, 1, func(context.Context, *Job) (int, string, error) {
		<-release
		return 1, "", nil
	})
	job := &Job{Filename: "a.pdf"}
	if err := s.Submit(job); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := range 5 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, errs[i] = s.Wait(ctx, job.ID)
		})
	}
	close(release)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("waiter %d: %v", i, err)
		}
	}
}

func TestUnknownJob(t *testing.T) {
	s := newStore(t, 1, func(context.Context, *Job) (int, string, error) { return 0, "", nil })
	if _, err := s.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get: err = %v, want ErrNotFound", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := s.Wait(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Wait: err = %v, want ErrNotFound", err)
	}
}

// A full queue is refused rather than blocking the HTTP handler.
func TestFullQueueIsRefusedNotBlocked(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	s := NewStore(1, 1, func(context.Context, *Job) (int, string, error) {
		<-release
		return 1, "", nil
	}, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})

	var refused bool
	for range 20 {
		if err := s.Submit(&Job{Filename: "x.pdf"}); errors.Is(err, ErrQueueFull) {
			refused = true
			break
		}
	}
	if !refused {
		t.Error("a saturated queue should refuse work rather than block")
	}
}

func TestRefusedJobIsRecordedAsFailed(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	s := NewStore(1, 1, func(context.Context, *Job) (int, string, error) {
		<-release
		return 1, "", nil
	}, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})

	var refusedID string
	for range 20 {
		job := &Job{Filename: "x.pdf"}
		if errors.Is(s.Submit(job), ErrQueueFull) {
			refusedID = job.ID
			break
		}
	}
	if refusedID == "" {
		t.Skip("queue never saturated")
	}
	got, err := s.Get(refusedID)
	if err != nil {
		t.Fatalf("a refused job should still be inspectable: %v", err)
	}
	if got.State != StateFailed || got.ErrorKind != "queue_full" {
		t.Errorf("state = %s, kind = %q", got.State, got.ErrorKind)
	}
}

func TestIDsAreUniqueAndPrefixed(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for range 1000 {
		id := NewID("job")
		if seen[id] {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = true
		if len(id) < 5 || id[:4] != "job_" {
			t.Fatalf("unexpected id shape: %s", id)
		}
	}
}

func TestListIsNewestFirst(t *testing.T) {
	s := newStore(t, 1, func(context.Context, *Job) (int, string, error) { return 1, "", nil })
	for i := range 3 {
		if err := s.Submit(&Job{Filename: string(rune('a' + i))}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	list := s.List()
	if len(list) != 3 {
		t.Fatalf("expected 3 jobs, got %d", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i].CreatedAt.After(list[i-1].CreatedAt) {
			t.Error("jobs should be listed newest first")
		}
	}
}

func TestStateTerminal(t *testing.T) {
	for state, want := range map[State]bool{
		StateQueued: false, StateInspecting: false, StateExtracting: false,
		StateStoring: false, StateDone: true, StateFailed: true,
	} {
		if state.Terminal() != want {
			t.Errorf("%s.Terminal() = %v, want %v", state, state.Terminal(), want)
		}
	}
}
