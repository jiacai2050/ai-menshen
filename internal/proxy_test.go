package aimenshen

import "testing"

func TestPickProviderSkipsZeroWeightProviders(t *testing.T) {
	gateway := &Gateway{
		cfg: Config{
			Providers: []ProviderConfig{
				{BaseURL: "https://disabled.example", Weight: intPtr(0)},
				{BaseURL: "https://active.example", Weight: intPtr(2)},
			},
		},
	}

	provider := gateway.pickProvider()
	if provider.BaseURL != "https://active.example" {
		t.Fatalf("pickProvider() = %q, want active provider", provider.BaseURL)
	}
}

func TestPickProviderFallsBackToFirstProviderWhenAllZero(t *testing.T) {
	gateway := &Gateway{
		cfg: Config{
			Providers: []ProviderConfig{
				{BaseURL: "https://first.example", Weight: intPtr(0)},
				{BaseURL: "https://second.example", Weight: intPtr(0)},
			},
		},
	}

	provider := gateway.pickProvider()
	if provider.BaseURL != "https://first.example" {
		t.Fatalf("pickProvider() = %q, want first provider fallback", provider.BaseURL)
	}
}

func intPtr(v int) *int {
	return &v
}
