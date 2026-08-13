// Package blob is a content-addressed store on the local filesystem.
//
// Nothing here is durable. The store lives under a temp directory by default
// and there is no database: this build deliberately has no persistence, so
// that the canonical schema and the routing contracts can be settled before
// anything is committed to a durable shape. Swapping in S3/MinIO later means
// implementing Put/Open/Path against a bucket; nothing outside this package
// knows where bytes actually live.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrNotFound is returned when a digest or derived artifact is not in the
// store.
var ErrNotFound = errors.New("blob: not found")

// Store is a content-addressed blob store rooted at a directory.
type Store struct {
	root string
}

// New opens (and creates) a store rooted at dir.
func New(dir string) (*Store, error) {
	for _, sub := range []string{"blobs", "docs"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("blob: create %s: %w", sub, err)
		}
	}
	return &Store{root: dir}, nil
}

// Root is the store's base directory.
func (s *Store) Root() string { return s.root }

// Put streams r into the store and returns its SHA-256 digest and size.
//
// The content is written to a temporary file and renamed into place once the
// digest is known, so a concurrent reader never observes a partial blob and an
// interrupted write leaves no half-object behind. Storing the same bytes twice
// is a no-op, which is the whole point of addressing by content.
func (s *Store) Put(r io.Reader) (digest string, size int64, err error) {
	tmp, err := os.CreateTemp(filepath.Join(s.root, "blobs"), ".incoming-*")
	if err != nil {
		return "", 0, fmt.Errorf("blob: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		// On any failure the temp file is garbage; on success it has been
		// renamed away and this is a no-op.
		if err != nil {
			os.Remove(tmpName)
		}
	}()

	h := sha256.New()
	size, err = io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		return "", 0, fmt.Errorf("blob: write: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return "", 0, fmt.Errorf("blob: sync: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("blob: close: %w", err)
	}

	digest = hex.EncodeToString(h.Sum(nil))
	dest := s.Path(digest)
	if err = os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", 0, fmt.Errorf("blob: create shard: %w", err)
	}
	if err = os.Rename(tmpName, dest); err != nil {
		return "", 0, fmt.Errorf("blob: commit: %w", err)
	}
	return digest, size, nil
}

// PutFile stores the contents of a file.
func (s *Store) PutFile(path string) (digest string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("blob: open %s: %w", path, err)
	}
	defer f.Close()
	return s.Put(f)
}

// Path is where the blob with this digest lives. Digests are sharded by their
// first byte so no single directory accumulates every object.
func (s *Store) Path(digest string) string {
	if len(digest) < 2 {
		return filepath.Join(s.root, "blobs", "__", digest)
	}
	return filepath.Join(s.root, "blobs", digest[:2], digest)
}

// Open returns a reader for a stored blob.
func (s *Store) Open(digest string) (*os.File, error) {
	f, err := os.Open(s.Path(digest))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: blob %s", ErrNotFound, digest)
	}
	return f, err
}

// Exists reports whether a blob is present.
func (s *Store) Exists(digest string) bool {
	st, err := os.Stat(s.Path(digest))
	return err == nil && !st.IsDir()
}

// DocDir is the per-document directory holding derived artifacts: the
// canonical JSON, the Markdown view, and extracted assets.
func (s *Store) DocDir(docID string) string {
	return filepath.Join(s.root, "docs", safeSegment(docID))
}

// WriteDerived stores a derived artifact for a document.
func (s *Store) WriteDerived(docID, name string, data []byte) error {
	dir := s.DocDir(docID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("blob: create doc dir: %w", err)
	}
	path := filepath.Join(dir, safeSegment(name))
	// Write-then-rename here too: a reader polling for the result must never
	// see a truncated JSON document.
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("blob: temp: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("blob: write derived: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("blob: close derived: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("blob: commit derived: %w", err)
	}
	return nil
}

// Remove deletes a document: the uploaded bytes and everything derived from
// them.
//
// Idempotent, because the caller is a retention sweep rather than a user
// action. A sweep that failed halfway and runs again must be able to finish the
// job, and "it was already gone" is the outcome it wanted either way.
//
// Derived artifacts go first. They are the readable form -- the extracted text,
// the assets -- so if only one half can be deleted, that is the half worth
// losing. A leftover blob is bytes nobody can reach through the API; a leftover
// canonical.json is the document itself, still served.
func (s *Store) Remove(docID string) error {
	var errs []error
	if err := os.RemoveAll(s.DocDir(docID)); err != nil {
		errs = append(errs, fmt.Errorf("blob: remove derived: %w", err))
	}
	if err := os.Remove(s.Path(docID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("blob: remove blob: %w", err))
	}
	return errors.Join(errs...)
}

// Sweep deletes documents older than ttl and returns the ids that went.
//
// This is a backstop, not the retention policy. Deletion proper is explicit --
// whoever holds the customer relationship knows when a window closed and calls
// Remove. But that only ever reaches documents something still points at. A
// document whose owner's record was lost, or that was stored and then
// abandoned before anything recorded it, is unreachable and would otherwise
// live forever precisely because nothing knows it exists.
//
// So the age here must be set well above the longest window the caller
// enforces, and it is a caller's job to know that number: this package cannot
// see the sessions, the payments or the policy. Too short and it deletes a
// document someone is still entitled to; the failure is silent on this side
// and looks like a broken purchase on theirs.
//
// Age is the blob's modification time, which is when the bytes arrived. It is
// deliberately not access time: relatime means atime barely moves, so a
// document read every day can still look untouched, and a policy resting on
// that would be resting on a filesystem mount option.
func (s *Store) Sweep(ttl time.Duration, now time.Time) (removed []string, err error) {
	if ttl <= 0 {
		return nil, nil
	}
	cutoff := now.Add(-ttl)
	prefixes, err := os.ReadDir(filepath.Join(s.root, "blobs"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("blob: sweep: %w", err)
	}
	for _, prefix := range prefixes {
		if !prefix.IsDir() {
			continue
		}
		dir := filepath.Join(s.root, "blobs", prefix.Name())
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			err = errors.Join(err, readErr)
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			info, statErr := e.Info()
			if statErr != nil {
				// Unreadable is not the same as old. Left alone.
				continue
			}
			if !info.ModTime().Before(cutoff) {
				continue
			}
			if rmErr := s.Remove(e.Name()); rmErr != nil {
				err = errors.Join(err, rmErr)
				continue
			}
			removed = append(removed, e.Name())
		}
	}
	sort.Strings(removed)
	return removed, err
}

// ReadDerived loads a derived artifact.
func (s *Store) ReadDerived(docID, name string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(s.DocDir(docID), safeSegment(name)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s/%s", ErrNotFound, docID, name)
	}
	return data, err
}

// TempDir creates a scratch directory for one job, for the shim to write
// assets and JSON into before they are committed to the store.
func (s *Store) TempDir(prefix string) (string, func(), error) {
	base := filepath.Join(s.root, "tmp")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", nil, fmt.Errorf("blob: create tmp: %w", err)
	}
	dir, err := os.MkdirTemp(base, safeSegment(prefix)+"-*")
	if err != nil {
		return "", nil, fmt.Errorf("blob: temp dir: %w", err)
	}
	return dir, func() { os.RemoveAll(dir) }, nil
}

// safeSegment strips anything that could escape the store's directory. Ids and
// artifact names reach here from request paths, so a name like "../../etc" has
// to become inert rather than merely unusual.
func safeSegment(s string) string {
	s = strings.ReplaceAll(s, "\\", "/")
	s = filepath.Base(filepath.Clean("/" + s))
	if s == "." || s == "/" || s == "" {
		return "_"
	}
	return s
}
