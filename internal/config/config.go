// Package config reads the service configuration from the environment.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/tendant/dolico/internal/engine/quality"
)

// Config is the whole service configuration. Every field has a working
// default, so `dolico` with no environment set runs.
type Config struct {
	// Addr is the HTTP listen address.
	Addr string
	// DataDir holds blobs and derived artifacts. It defaults under the system
	// temp directory because nothing here is durable: this build has no
	// persistence, and putting it somewhere that looks permanent would invite
	// treating it as such.
	DataDir string
	// Workers is the size of the in-process extraction worker pool.
	Workers int
	// ShimPath is the dolico-rs binary.
	ShimPath string
	// ShimTimeout bounds a single shim invocation. A malformed document that
	// sends a parser into a pathological path must not wedge a worker.
	ShimTimeout time.Duration
	// OCRThreshold is the page quality score below which a text-extracted page
	// is re-extracted by the OCR tier. Range 0..1.
	OCRThreshold float64
	// MaxUploadBytes caps a single upload.
	MaxUploadBytes int64
	// OCRURL is the PaddleOCR service. When empty the stub OCR tier is used
	// instead, which keeps the service runnable -- and the tests and the e2e
	// sweep passing -- with no Python installed.
	OCRURL string
	// OCRTimeout bounds a single OCR request. OCR is seconds per page, so this
	// is far more generous than the shim timeout.
	OCRTimeout time.Duration
	// OCRConcurrency is how many OCR requests may be in flight at once. Zero
	// means match the number of worker processes the service reports, which
	// is almost always right: more requests than workers only queue on the far
	// side while paying to upload the document again.
	OCRConcurrency int
	// VisionEnabled turns on the third tier. Off by default because it needs
	// several gigabytes of model weights the other tiers do not, not because
	// it is slow -- measured warm it is about as fast as the OCR tier, and it
	// reads better. It stays a fallback for pipeline reasons rather than model
	// ones; see docs/vision-tier-design.md.
	VisionEnabled bool
	// VisionThreshold is the page quality below which an OCR result is
	// re-read by the vision tier. Lower than OCRThreshold on purpose.
	VisionThreshold float64
	// VisionMaxPages bounds vision escalation per document.
	VisionMaxPages int
	// VisionProbe asks the vision tier about one page of every document that
	// used OCR, and escalates the rest when the two tiers disagree about it.
	// On by default once the vision tier is enabled at all: the threshold
	// alone only catches OCR that knows it struggled, and the measured case
	// is OCR misreading half a page at 0.938 confidence. Costs one vision
	// call per document with scanned pages.
	VisionProbe bool
	// VisionDisagreement is how far apart the two tiers must be on the probed
	// page before the OCR tier is distrusted for the whole document.
	VisionDisagreement float64
}

// Load reads configuration from the environment, applying defaults.
func Load() (*Config, error) {
	c := &Config{
		Addr:           env("DOLICO_ADDR", ":8080"),
		DataDir:        env("DOLICO_DATA_DIR", filepath.Join(os.TempDir(), "dolico")),
		Workers:        runtime.NumCPU(),
		ShimPath:       env("DOLICO_SHIM_PATH", ""),
		ShimTimeout:    120 * time.Second,
		OCRThreshold:   0.60,
		MaxUploadBytes: 256 << 20,
		OCRURL:          env("DOLICO_OCR_URL", ""),
		OCRTimeout:      10 * time.Minute,
		VisionEnabled:      envBool("DOLICO_VISION_ENABLED", false),
		VisionThreshold:    0.35,
		VisionMaxPages:     5,
		VisionProbe:        envBool("DOLICO_VISION_PROBE", true),
		VisionDisagreement: quality.DefaultDisagreement,
	}

	var err error
	if c.Workers, err = envInt("DOLICO_WORKERS", c.Workers); err != nil {
		return nil, err
	}
	if c.Workers < 1 {
		return nil, fmt.Errorf("DOLICO_WORKERS must be at least 1, got %d", c.Workers)
	}
	if c.OCRThreshold, err = envFloat("DOLICO_OCR_THRESHOLD", c.OCRThreshold); err != nil {
		return nil, err
	}
	if c.OCRThreshold < 0 || c.OCRThreshold > 1 {
		return nil, fmt.Errorf("DOLICO_OCR_THRESHOLD must be within 0..1, got %v", c.OCRThreshold)
	}
	if c.ShimTimeout, err = envDuration("DOLICO_SHIM_TIMEOUT", c.ShimTimeout); err != nil {
		return nil, err
	}
	if c.OCRTimeout, err = envDuration("DOLICO_OCR_TIMEOUT", c.OCRTimeout); err != nil {
		return nil, err
	}
	if c.OCRConcurrency, err = envInt("DOLICO_OCR_CONCURRENCY", c.OCRConcurrency); err != nil {
		return nil, err
	}
	if c.OCRConcurrency < 0 {
		return nil, fmt.Errorf("DOLICO_OCR_CONCURRENCY must not be negative, got %d", c.OCRConcurrency)
	}
	if c.VisionThreshold, err = envFloat("DOLICO_VISION_THRESHOLD", c.VisionThreshold); err != nil {
		return nil, err
	}
	if c.VisionThreshold < 0 || c.VisionThreshold > 1 {
		return nil, fmt.Errorf("DOLICO_VISION_THRESHOLD must be within 0..1, got %v", c.VisionThreshold)
	}
	if c.VisionThreshold >= c.OCRThreshold {
		// A vision bar at or above the OCR bar would escalate every page OCR
		// touched, which is not a fallback tier but a default one.
		return nil, fmt.Errorf(
			"DOLICO_VISION_THRESHOLD (%v) must be below DOLICO_OCR_THRESHOLD (%v)",
			c.VisionThreshold, c.OCRThreshold)
	}
	if c.VisionMaxPages, err = envInt("DOLICO_VISION_MAX_PAGES", c.VisionMaxPages); err != nil {
		return nil, err
	}
	if c.VisionMaxPages < 1 {
		return nil, fmt.Errorf("DOLICO_VISION_MAX_PAGES must be at least 1, got %d", c.VisionMaxPages)
	}
	if c.VisionDisagreement, err = envFloat(
		"DOLICO_VISION_DISAGREEMENT", c.VisionDisagreement); err != nil {
		return nil, err
	}
	if c.VisionDisagreement <= 0 || c.VisionDisagreement > 1 {
		// Zero would escalate every document that used OCR, since no two
		// engines agree to the character on every page.
		return nil, fmt.Errorf(
			"DOLICO_VISION_DISAGREEMENT must be within (0..1], got %v", c.VisionDisagreement)
	}
	maxUpload := c.MaxUploadBytes
	if maxUpload, err = envInt64("DOLICO_MAX_UPLOAD_BYTES", maxUpload); err != nil {
		return nil, err
	}
	if maxUpload < 1 {
		return nil, fmt.Errorf("DOLICO_MAX_UPLOAD_BYTES must be positive, got %d", maxUpload)
	}
	c.MaxUploadBytes = maxUpload

	if c.ShimPath == "" {
		c.ShimPath = findShim()
	}
	return c, nil
}

// findShim locates the dolico-rs binary: the release build inside the repo
// first, since that is what `make build` produces, then the debug build, then
// whatever is on PATH.
func findShim() string {
	candidates := []string{
		"rust/dolico-rs/target/release/dolico-rs",
		"rust/dolico-rs/target/debug/dolico-rs",
	}
	if exe, err := os.Executable(); err == nil {
		root := filepath.Dir(filepath.Dir(exe)) // bin/dolico -> repo root
		candidates = append(candidates,
			filepath.Join(root, "rust/dolico-rs/target/release/dolico-rs"),
		)
	}
	for _, c := range candidates {
		if abs, err := filepath.Abs(c); err == nil {
			if st, err := os.Stat(abs); err == nil && !st.IsDir() {
				return abs
			}
		}
	}
	return "dolico-rs"
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func envInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func envInt64(key string, def int64) (int64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func envFloat(key string, def float64) (float64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return f, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %s", key, d)
	}
	return d, nil
}
