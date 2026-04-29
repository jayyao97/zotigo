package config_test

import (
	"testing"

	"github.com/jayyao97/zotigo/core/config"
)

func TestDefaultContextWindow(t *testing.T) {
	// Sanity band: below 100k over-warns frontier models; above 1M
	// never warns even when a small local model is the actual ceiling.
	if config.DefaultContextWindow < 100_000 || config.DefaultContextWindow > 1_000_000 {
		t.Errorf("DefaultContextWindow = %d outside the expected 100k–1M band",
			config.DefaultContextWindow)
	}
}
