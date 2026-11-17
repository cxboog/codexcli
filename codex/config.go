package codex

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// FallbackModel is used when codex's config names no model.
const FallbackModel = "gpt-5.6-terra"

// Config is the handful of codex settings this package honours, so that
// requests default to what the user's codex would do.
type Config struct {
	Model string
	// ReasoningEffort is empty when unset (the model's own default applies).
	ReasoningEffort string
	// ServiceTier is in wire form: "priority" for codex's "fast", else as set.
	ServiceTier string
}

// LoadConfig reads the top-level model, model_reasoning_effort and
// service_tier keys of $CODEX_HOME/config.toml. Missing file or keys are
// fine; Model always comes back non-empty.
func LoadConfig() Config {
	cfg := Config{Model: FallbackModel}
	home, err := CodexHome()
	if err != nil {
		return cfg
	}
	f, err := os.Open(filepath.Join(home, "config.toml"))
	if err != nil {
		return cfg
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			break // only top-level keys, which precede the first table
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) < 2 || (v[0] != '"' && v[0] != '\'') {
			continue
		}
		end := strings.IndexByte(v[1:], v[0])
		if end < 0 {
			continue
		}
		v = v[1 : 1+end]
		switch strings.TrimSpace(k) {
		case "model":
			if v != "" {
				cfg.Model = v
			}
		case "model_reasoning_effort":
			cfg.ReasoningEffort = v
		case "service_tier":
			cfg.ServiceTier = v
			if v == "fast" {
				cfg.ServiceTier = "priority"
			}
		}
	}
	return cfg
}
