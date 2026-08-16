package llm_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/SigNoz/signoz/pkg/query-service/vigil/llm"
)

// TestLiveVendors exercises the chain against real endpoints.
//
// It is skipped unless a key is present in the environment, so the default
// `go test ./...` stays offline and credential-free. This is the only test in
// the package that proves the wire format is right rather than that our own
// httptest stub matches our own client — an integration everybody claims and
// few actually run.
//
//	VIGIL_GROQ_API_KEY=... go test -run TestLiveVendors -v ./pkg/query-service/vigil/llm/
func TestLiveVendors(t *testing.T) {
	cfgs := []llm.Config{}
	for _, v := range []struct{ name, env, base, model string }{
		{"featherless", "VIGIL_FEATHERLESS_API_KEY", "https://api.featherless.ai/v1", "Qwen/Qwen2.5-7B-Instruct"},
		{"groq", "VIGIL_GROQ_API_KEY", "https://api.groq.com/openai/v1", "llama-3.1-8b-instant"},
		{"venice", "VIGIL_VENICE_API_KEY", "https://api.venice.ai/api/v1", "qwen3-4b"},
	} {
		key := os.Getenv(v.env)
		if key == "" {
			t.Logf("skipping %s: %s unset", v.name, v.env)
			continue
		}
		cfgs = append(cfgs, llm.Config{
			Name:    v.name,
			APIKey:  key,
			BaseURL: v.base,
			Models:  map[llm.Role]string{llm.RoleFast: v.model},
			Timeout: 20 * time.Second,
			Retries: 1,
		})
	}
	if len(cfgs) == 0 {
		t.Skip("no vendor credentials in the environment")
	}

	// Each vendor is checked on its own, so a chain success cannot hide a
	// vendor that is quietly never reached.
	for _, cfg := range cfgs {
		t.Run(cfg.Name, func(t *testing.T) {
			p, err := llm.NewOpenAICompatible(newTestLogger(t), cfg)
			if err != nil {
				t.Fatalf("construction failed: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			resp, err := p.Complete(ctx, llm.Request{
				Role:        llm.RoleFast,
				System:      "You are a JSON API. Reply with exactly {\"ok\":true} and nothing else.",
				User:        "ping",
				MaxTokens:   32,
				Temperature: 0,
				JSONOnly:    true,
			})
			if err != nil {
				t.Fatalf("live call failed: %v", err)
			}
			if resp.Text == "" {
				t.Error("empty completion")
			}
			// ModelID comes from the response body, not the request, so this
			// asserts the vendor told us what it actually ran.
			if resp.ModelID == "" {
				t.Error("no model ID in the response")
			}
			t.Logf("%s: model=%s latency=%s tokens=%d/%d text=%q",
				cfg.Name, resp.ModelID, resp.Latency.Round(time.Millisecond),
				resp.PromptTokens, resp.CompletionTokens, resp.Text)
		})
	}
}
