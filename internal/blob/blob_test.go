package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestPutReturnsContentDigest(t *testing.T) {
	s := newStore(t)
	content := []byte("the quick brown fox")
	want := sha256.Sum256(content)

	digest, size, err := s.Put(strings.NewReader(string(content)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if digest != hex.EncodeToString(want[:]) {
		t.Errorf("digest = %s, want %s", digest, hex.EncodeToString(want[:]))
	}
	if size != int64(len(content)) {
		t.Errorf("size = %d, want %d", size, len(content))
	}
}

func TestPutIsIdempotentForIdenticalContent(t *testing.T) {
	s := newStore(t)
	d1, _, err := s.Put(strings.NewReader("same bytes"))
	if err != nil {
		t.Fatal(err)
	}
	d2, _, err := s.Put(strings.NewReader("same bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatalf("identical content produced different digests: %s vs %s", d1, d2)
	}

	// Storing twice must leave exactly one object, not two.
	var files int
	_ = filepath.Walk(filepath.Join(s.Root(), "blobs"), func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			files++
		}
		return nil
	})
	if files != 1 {
		t.Errorf("expected 1 stored object, found %d", files)
	}
}

func TestOpenRoundTrips(t *testing.T) {
	s := newStore(t)
	digest, _, err := s.Put(strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.Open(digest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	got, _ := io.ReadAll(f)
	if string(got) != "payload" {
		t.Errorf("read %q, want %q", got, "payload")
	}
}

func TestOpenMissingReportsNotFound(t *testing.T) {
	s := newStore(t)
	_, err := s.Open(strings.Repeat("0", 64))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestExists(t *testing.T) {
	s := newStore(t)
	digest, _, _ := s.Put(strings.NewReader("x"))
	if !s.Exists(digest) {
		t.Error("stored blob should exist")
	}
	if s.Exists(strings.Repeat("a", 64)) {
		t.Error("unstored digest should not exist")
	}
}

func TestNoPartialBlobOnFailedWrite(t *testing.T) {
	s := newStore(t)
	// A reader that fails midway must leave nothing behind, or a later reader
	// could observe a truncated object under a digest that does not match it.
	_, _, err := s.Put(io.MultiReader(
		strings.NewReader("first part"),
		&failingReader{},
	))
	if err == nil {
		t.Fatal("expected an error from the failing reader")
	}
	entries, _ := os.ReadDir(filepath.Join(s.Root(), "blobs"))
	for _, e := range entries {
		if !e.IsDir() {
			t.Errorf("leftover file after failed write: %s", e.Name())
		}
	}
}

type failingReader struct{}

func (*failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestDerivedRoundTrip(t *testing.T) {
	s := newStore(t)
	if err := s.WriteDerived("doc1", "canonical.json", []byte(`{"a":1}`)); err != nil {
		t.Fatalf("WriteDerived: %v", err)
	}
	got, err := s.ReadDerived("doc1", "canonical.json")
	if err != nil {
		t.Fatalf("ReadDerived: %v", err)
	}
	if string(got) != `{"a":1}` {
		t.Errorf("got %q", got)
	}
}

func TestDerivedMissingReportsNotFound(t *testing.T) {
	s := newStore(t)
	_, err := s.ReadDerived("nope", "canonical.json")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// Document ids and artifact names arrive from request paths, so a traversal
// attempt has to be neutralized rather than merely unusual.
func TestDerivedPathsCannotEscapeTheStore(t *testing.T) {
	s := newStore(t)
	for _, evil := range []string{
		"../../../../etc/passwd",
		"..",
		"a/../../b",
		`..\..\windows`,
		"/absolute",
	} {
		if err := s.WriteDerived(evil, "x.json", []byte("owned")); err != nil {
			t.Fatalf("WriteDerived(%q): %v", evil, err)
		}
		dir := s.DocDir(evil)
		if !strings.HasPrefix(filepath.Clean(dir), filepath.Clean(s.Root())) {
			t.Errorf("DocDir(%q) = %q escaped the store root %q", evil, dir, s.Root())
		}
	}
	// Same for the artifact name.
	if err := s.WriteDerived("doc", "../../escape.json", []byte("owned")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "..", "escape.json")); err == nil {
		t.Error("artifact name escaped the store")
	}
}

func TestPathShardsByDigestPrefix(t *testing.T) {
	s := newStore(t)
	digest := strings.Repeat("ab", 32)
	got := s.Path(digest)
	want := filepath.Join(s.Root(), "blobs", "ab", digest)
	if got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
	// A short or empty digest must not produce a path outside the shard tree.
	if !strings.HasPrefix(s.Path(""), filepath.Join(s.Root(), "blobs")) {
		t.Errorf("Path(\"\") = %q escaped blobs/", s.Path(""))
	}
}

func TestTempDirIsRemovedByCleanup(t *testing.T) {
	s := newStore(t)
	dir, cleanup, err := s.TempDir("assets")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("temp dir should exist: %v", err)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("temp dir should be gone after cleanup")
	}
}

func TestConcurrentPutsOfTheSameContent(t *testing.T) {
	s := newStore(t)
	const n = 16
	digests := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			d, _, err := s.Put(strings.NewReader("concurrent payload"))
			if err != nil {
				t.Errorf("Put: %v", err)
				return
			}
			digests[i] = d
		})
	}
	wg.Wait()
	for i, d := range digests {
		if d != digests[0] {
			t.Fatalf("goroutine %d got digest %s, want %s", i, d, digests[0])
		}
	}
	f, err := s.Open(digests[0])
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	got, _ := io.ReadAll(f)
	if string(got) != "concurrent payload" {
		t.Errorf("content corrupted by concurrent writes: %q", got)
	}
}
