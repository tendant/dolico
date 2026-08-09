// Package config reads the service configuration from the environment.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
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
