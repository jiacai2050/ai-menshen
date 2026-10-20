package menshen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigProviderWeightUsesIntValue(t *testing.T) {
	cfg := loadTestConfig(t, `
[providers.openai]
base_url = "https://api.openai.com/v1"
api_key = "sk-test"
weight = 2

[storage.sqlite]
path = "./test.db"
`)

	provider, ok := cfg.Providers["openai"]
	if !ok {
		t.Fatalf("Providers = %v, want provider openai", cfg.Providers)
	}
	if got := provider.Weight; got != 2 {
		t.Fatalf("Weight = %d, want 2", got)
	}
	if got := provider.Name; got != "openai" {
		t.Fatalf("Name = %q, want openai", got)
	}
}

func TestLoadConfigDefaultsWebListen(t *testing.T) {
	cfg := loadTestConfig(t, `
	[providers.openai]
	base_url = "https://api.openai.com/v1"
	api_key = "sk-test"
	weight = 1

	[storage.sqlite]
	path = "./test.db"
	`)

	if got := cfg.Web.Listen; got != ":8081" {
		t.Fatalf("Web.Listen = %q, want :8081", got)
	}
}

func TestLoadConfigRejectsNegativeProviderWeight(t *testing.T) {
	_, err := LoadConfig(writeTestConfig(t, `
[providers.openai]
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
[providers.openai]
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
[providers.openai]
base_url = "https://api.openai.com/v1"
api_key = "sk-test"

[storage.sqlite]
path = "./test.db"
`))
	if err == nil || !strings.Contains(err.Error(), "at least one provider with positive weight") {
		t.Fatalf("LoadConfig() error = %v, want explicit positive weight validation", err)
	}
}

func TestLoadConfigRejectsInvalidProviderName(t *testing.T) {
	for _, badName := range []string{`"my provider"`, `"my.provider"`, `"my@provider"`} {
		_, err := LoadConfig(writeTestConfig(t, `
[providers.`+badName+`]
base_url = "https://api.openai.com/v1"
api_key = "sk-test"
weight = 1

[storage.sqlite]
path = "./test.db"
`))
		if err == nil || !strings.Contains(err.Error(), "name must match") {
			t.Fatalf("LoadConfig() with name %s error = %v, want provider name validation", badName, err)
		}
	}
}

func TestLoadConfigRejectsDuplicateProviderName(t *testing.T) {
	_, err := LoadConfig(writeTestConfig(t, `
[providers.openai]
base_url = "https://api.openai.com/v1"
api_key = "sk-test"
weight = 1

[providers.openai]
base_url = "https://api.openai.com/v2"
api_key = "sk-test"
weight = 1

[storage.sqlite]
path = "./test.db"
`))
	if err == nil {
		t.Fatalf("LoadConfig() error = nil, want duplicate provider name rejection")
	}
}

func TestLoadConfigCapabilitiesDefaultAndValidation(t *testing.T) {
	// 1. Default to openai-completions
	cfg := loadTestConfig(t, `
[providers.openai]
base_url = "https://api.openai.com/v1"
api_key = "sk-test"
weight = 1

[storage.sqlite]
path = "./test.db"
`)
	provider := cfg.Providers["openai"]
	if len(provider.Capabilities) != 1 || provider.Capabilities[0] != CapOpenAICompletions {
		t.Fatalf("Capabilities want [%s], got %v", CapOpenAICompletions, provider.Capabilities)
	}

	// 2. Explicit capabilities
	cfg2 := loadTestConfig(t, `
[providers.claude]
base_url = "https://api.anthropic.com/v1"
api_key = "sk-test"
weight = 1
capabilities = ["anthropic-messages"]

[storage.sqlite]
path = "./test.db"
`)
	provider2 := cfg2.Providers["claude"]
	if len(provider2.Capabilities) != 1 || provider2.Capabilities[0] != CapAnthropicMessages {
		t.Fatalf("Capabilities want [%s], got %v", CapAnthropicMessages, provider2.Capabilities)
	}
	if !provider2.HasCapability(CapAnthropicMessages) {
		t.Error("expected HasCapability(CapAnthropicMessages) = true")
	}
	if provider2.HasCapability(CapOpenAICompletions) {
		t.Error("expected HasCapability(CapOpenAICompletions) = false")
	}

	// 3. Reject unknown capability
	_, err := LoadConfig(writeTestConfig(t, `
[providers.bad]
base_url = "https://api.example.com"
api_key = "sk-test"
weight = 1
capabilities = ["unsupported-cap"]

[storage.sqlite]
path = "./test.db"
`))
	if err == nil || !strings.Contains(err.Error(), "unknown capability") {
		t.Fatalf("LoadConfig() error = %v, want unknown capability error", err)
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
