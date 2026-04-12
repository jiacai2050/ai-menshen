package aimenshen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigProviderWeightUsesIntValue(t *testing.T) {
	cfg := loadTestConfig(t, `
[[providers]]
base_url = "https://api.openai.com/v1"
api_key = "sk-test"
weight = 2

[storage.sqlite]
path = "./test.db"
`)

	if got := cfg.Providers[0].Weight; got != 2 {
		t.Fatalf("Weight = %d, want 2", got)
	}
}

func TestLoadConfigRejectsNegativeProviderWeight(t *testing.T) {
	_, err := LoadConfig(writeTestConfig(t, `
[[providers]]
base_url = "https://api.openai.com/v1"
api_key = "sk-test"
weight = -1

[storage.sqlite]
path = "./test.db"
`))
	if err == nil || !strings.Contains(err.Error(), "weight must not be negative") {
		t.Fatalf("LoadConfig() error = %v, want negative weight validation", err)
	}
}

func TestLoadConfigRequiresPositiveProviderWeight(t *testing.T) {
	_, err := LoadConfig(writeTestConfig(t, `
[[providers]]
base_url = "https://api.openai.com/v1"
api_key = "sk-test"
weight = 0

[storage.sqlite]
path = "./test.db"
`))
	if err == nil || !strings.Contains(err.Error(), "at least one provider with positive weight") {
		t.Fatalf("LoadConfig() error = %v, want positive weight validation", err)
	}
}

func TestLoadConfigRequiresExplicitPositiveWeight(t *testing.T) {
	_, err := LoadConfig(writeTestConfig(t, `
[[providers]]
base_url = "https://api.openai.com/v1"
api_key = "sk-test"

[storage.sqlite]
path = "./test.db"
`))
	if err == nil || !strings.Contains(err.Error(), "at least one provider with positive weight") {
		t.Fatalf("LoadConfig() error = %v, want explicit positive weight validation", err)
	}
}

func loadTestConfig(t *testing.T, content string) Config {
	t.Helper()

	cfg, err := LoadConfig(writeTestConfig(t, content))
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	return cfg
}

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(strings.TrimSpace(content)), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	return path
}
