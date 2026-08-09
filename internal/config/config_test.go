package config_test

import (
	"strings"
	"testing"

	"github.com/tendant/dolico/internal/config"
)

func TestDefaultsRunWithNoEnvironment(t *testing.T) {
	c, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.VisionEnabled {
		t.Error("the vision tier should be off unless asked for")
	}
	if c.VisionThreshold >= c.OCRThreshold {
		t.Errorf("vision threshold %v is not below the OCR threshold %v",
			c.VisionThreshold, c.OCRThreshold)
	}
	if c.VisionMaxPages < 1 {
		t.Errorf("VisionMaxPages = %d", c.VisionMaxPages)
	}
}

func TestVisionEnabledAcceptsTheUsualSpellings(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"1", true}, {"true", true}, {"TRUE", true}, {"yes", true}, {"on", true},
		{"0", false}, {"false", false}, {"", false}, {"nonsense", false},
	} {
		t.Setenv("DOLICO_VISION_ENABLED", tc.value)
		c, err := config.Load()
		if err != nil {
			t.Fatalf("%q: %v", tc.value, err)
		}
		if c.VisionEnabled != tc.want {
			t.Errorf("%q -> %v, want %v", tc.value, c.VisionEnabled, tc.want)
		}
	}
}

// A vision bar at or above the OCR bar escalates every page OCR touched, which
// makes Tier 3 the default tier rather than the fallback. Refusing to start is
// better than quietly spending minutes and gigabytes per document.
func TestVisionThresholdMustBeBelowTheOCRThreshold(t *testing.T) {
	for _, value := range []string{"0.6", "0.75"} {
		t.Setenv("DOLICO_VISION_THRESHOLD", value)
		_, err := config.Load()
		if err == nil {
			t.Fatalf("threshold %s was accepted", value)
		}
		if !strings.Contains(err.Error(), "DOLICO_VISION_THRESHOLD") {
			t.Errorf("err = %v", err)
		}
	}
}

func TestVisionKnobsAreValidated(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"DOLICO_VISION_THRESHOLD", "1.5"},
		{"DOLICO_VISION_THRESHOLD", "-0.1"},
		{"DOLICO_VISION_THRESHOLD", "banana"},
		{"DOLICO_VISION_MAX_PAGES", "0"},
		{"DOLICO_VISION_MAX_PAGES", "-3"},
	} {
		t.Setenv(tc.key, tc.value)
		if _, err := config.Load(); err == nil {
			t.Errorf("%s=%s was accepted", tc.key, tc.value)
		}
	}
}

func TestVisionThresholdCanBeRaisedAlongsideTheOCRThreshold(t *testing.T) {
	t.Setenv("DOLICO_OCR_THRESHOLD", "0.8")
	t.Setenv("DOLICO_VISION_THRESHOLD", "0.7")
	c, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.VisionThreshold != 0.7 || c.OCRThreshold != 0.8 {
		t.Errorf("thresholds = %v / %v", c.VisionThreshold, c.OCRThreshold)
	}
}
