// Package cache memoizes extraction results.
//
// The cache key is the one the design calls for -- document hash, engine,
// engine version, pipeline version, configuration -- applied at *page*
// granularity rather than per document. That is what makes an engine upgrade
// cheap: bumping the OCR engine's version invalidates only the pages that
// engine produced, and a re-run reuses every natively-extracted page
// untouched.
//
// This build keeps entries in memory, so the cache is empty at startup and
// dies with the process. The keying is the durable part; moving the values
// into Postgres or Redis later does not change any caller.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/tendant/dolico/internal/canonical"
)

// Key identifies one cached page extraction.
type Key struct {
	DocumentHash  string
	Engine        string
	EngineVersion string
	Page          int
	// Config is the engine configuration this result was produced under. Two
	// runs with different options are different results.
	Config map[string]string
}

// String renders the key as a stable cache string. Config entries are sorted
// so that map iteration order cannot produce two spellings of one key.
func (k Key) String() string {
	var b strings.Builder
	b.WriteString(k.DocumentHash)
	b.WriteByte('|')
	b.WriteString(canonical.PipelineVersion)
	b.WriteByte('|')
	b.WriteString(k.Engine)
	b.WriteByte('@')
	b.WriteString(k.EngineVersion)
	b.WriteByte('|')
	b.WriteString(strconv.Itoa(k.Page))
	if len(k.Config) > 0 {
		names := make([]string, 0, len(k.Config))
		for name := range k.Config {
			names = append(names, name)
		}
		sort.Strings(names)
		h := sha256.New()
		for _, name := range names {
			h.Write([]byte(name))
			h.Write([]byte{0})
			h.Write([]byte(k.Config[name]))
			h.Write([]byte{0})
		}
		b.WriteByte('|')
		b.WriteString(hex.EncodeToString(h.Sum(nil))[:16])
	}
	return b.String()
}

// Cache is a concurrency-safe in-memory page cache.
type Cache struct {
	mu    sync.RWMutex
	pages map[string]canonical.Page
	// assets are cached per document-and-engine rather than per page, because
	// an engine emits them for the document as a whole. Without this, a run
	// that hits the page cache would return no assets and overwrite a
	// perfectly good stored document with an asset-less one.
	assets map[string][]canonical.Asset
	hits   int64
	misses int64
	// limit bounds the number of retained pages. Without persistence the
	// process would otherwise grow without bound across a long-running
	// session; when the limit is hit the cache is cleared rather than evicted
	// by LRU, which is crude but keeps the hot path lock-cheap and is honest
	// about being a placeholder for a real store.
	limit int
}

// New creates a cache retaining up to limit pages. A limit of zero or less
// means unbounded.
func New(limit int) *Cache {
	return &Cache{
		pages:  make(map[string]canonical.Page),
		assets: make(map[string][]canonical.Asset),
		limit:  limit,
	}
}

// assetKey identifies a document's assets as produced by one engine build.
// The page number is not part of it.
func assetKey(k Key) string {
	noPage := k
	noPage.Page = -1
	return noPage.String()
}

// GetAssets returns the assets an engine produced for a document.
func (c *Cache) GetAssets(k Key) ([]canonical.Asset, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	assets, ok := c.assets[assetKey(k)]
	if !ok {
		return nil, false
	}
	return append([]canonical.Asset(nil), assets...), true
}

// PutAssets records the assets an engine produced for a document. An engine
// that produced none still records that fact, so a later cache hit can tell
// "no assets" apart from "not cached".
func (c *Cache) PutAssets(k Key, assets []canonical.Asset) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.limit > 0 && len(c.assets) >= c.limit {
		c.assets = make(map[string][]canonical.Asset)
	}
	c.assets[assetKey(k)] = append([]canonical.Asset(nil), assets...)
}

// Get returns a cached page.
func (c *Cache) Get(k Key) (canonical.Page, bool) {
	c.mu.RLock()
	page, ok := c.pages[k.String()]
	c.mu.RUnlock()

	c.mu.Lock()
	if ok {
		c.hits++
	} else {
		c.misses++
	}
	c.mu.Unlock()
	if !ok {
		return canonical.Page{}, false
	}
	return clonePage(page), true
}

// Put stores a page.
func (c *Cache) Put(k Key, page canonical.Page) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.limit > 0 && len(c.pages) >= c.limit {
		c.pages = make(map[string]canonical.Page)
	}
	c.pages[k.String()] = clonePage(page)
}

// Forget drops everything cached for one document and reports how many entries
// went.
//
// Deleting a document has to reach in here as well as the disk. Page text is
// the document -- a cache that kept it after the bytes were deleted for
// retention would be quietly holding the thing that was supposed to be gone,
// and would serve it back on the next request for the same digest.
func (c *Cache) Forget(documentHash string) int {
	if documentHash == "" {
		return 0
	}
	prefix := documentHash + "|"
	c.mu.Lock()
	defer c.mu.Unlock()
	dropped := 0
	for key := range c.pages {
		if strings.HasPrefix(key, prefix) {
			delete(c.pages, key)
			dropped++
		}
	}
	for key := range c.assets {
		if strings.HasPrefix(key, prefix) {
			delete(c.assets, key)
			dropped++
		}
	}
	return dropped
}

// Stats reports hit and miss counts and the number of retained pages.
func (c *Cache) Stats() (hits, misses int64, size int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.hits, c.misses, len(c.pages)
}

// clonePage copies the parts of a page a caller could mutate. Handing out the
// stored value directly would let one request's post-processing -- attaching a
// quality score, say -- silently rewrite what the next request reads back.
func clonePage(p canonical.Page) canonical.Page {
	out := p
	out.Classification.Reasons = append([]string(nil), p.Classification.Reasons...)
	out.Blocks = cloneBlocks(p.Blocks)
	if p.Quality != nil {
		q := *p.Quality
		out.Quality = &q
	}
	if p.Width != nil {
		w := *p.Width
		out.Width = &w
	}
	if p.Height != nil {
		h := *p.Height
		out.Height = &h
	}
	return out
}

func cloneBlocks(blocks []canonical.Block) []canonical.Block {
	if blocks == nil {
		return nil
	}
	out := make([]canonical.Block, len(blocks))
	for i, b := range blocks {
		out[i] = b
		if b.BBox != nil {
			bb := *b.BBox
			out[i].BBox = &bb
		}
		if b.Confidence != nil {
			c := *b.Confidence
			out[i].Confidence = &c
		}
		out[i].Inline = append([]canonical.Span(nil), b.Inline...)
		out[i].Quote = cloneBlocks(b.Quote)
		if b.List != nil {
			l := *b.List
			l.Items = make([]canonical.ListItem, len(b.List.Items))
			for j, it := range b.List.Items {
				l.Items[j] = canonical.ListItem{Blocks: cloneBlocks(it.Blocks)}
				if it.Checked != nil {
					ch := *it.Checked
					l.Items[j].Checked = &ch
				}
			}
			out[i].List = &l
		}
		if b.Table != nil {
			t := *b.Table
			t.Grid = make([][]canonical.Cell, len(b.Table.Grid))
			for r, row := range b.Table.Grid {
				t.Grid[r] = make([]canonical.Cell, len(row))
				for c, cell := range row {
					t.Grid[r][c] = cell
					t.Grid[r][c].Blocks = cloneBlocks(cell.Blocks)
					if cell.Covered != nil {
						ref := *cell.Covered
						t.Grid[r][c].Covered = &ref
					}
				}
			}
			out[i].Table = &t
		}
	}
	return out
}
