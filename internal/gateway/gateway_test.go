package gateway

import "testing"

func TestDefaultModelForSupportedProvider(t *testing.T) {
	for _, provider := range []string{"openai", "cerebras", "fireworks", "openrouter"} {
		model, ok := DefaultModel(provider)
		if !ok || model == "" {
			t.Errorf("DefaultModel(%q) = %q, %v", provider, model, ok)
		}
	}
	if _, ok := DefaultModel("unknown"); ok {
		t.Error("unknown provider unexpectedly has a default model")
	}
}
