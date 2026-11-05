//go:build live

package catalog

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"opencode-go-cliproxyapi/internal/config"
)

// fetchLiveCatalog reads the production catalog anonymously. The plugin's
// Refresh sends a configured key, so this test feeds the same bytes through
// the decoder instead of exercising auth. Opt in with OPENCODE_LIVE=1.
func fetchLiveCatalog(t *testing.T) []byte {
	t.Helper()
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get("https://opencode.ai/inference/go/v1/models")
	if err != nil {
		t.Fatalf("fetch catalog: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("catalog status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	return body
}

func liveConsoleCfg() config.Config {
	return config.Config{
		BaseURL:          "https://opencode.ai/inference/go",
		CatalogURL:       "https://opencode.ai/inference/go/v1/models",
		ModelPrefix:      config.ModelPrefix{Enabled: true, Value: "opencode-go"},
		MaxResponseBytes: 16 << 20,
		Catalog:          config.Catalog{StaleWhileUnavailable: true},
		Protocols:        config.Protocols{ChatCompletions: true, Messages: true, Responses: true, Google: true},
		Endpoints: config.Endpoints{
			ChatCompletions: "https://opencode.ai/inference/go/openai/v1/chat/completions",
			Messages:        "https://opencode.ai/inference/go/anthropic/v1/messages",
			Responses:       "https://opencode.ai/inference/go/openai/v1/responses",
			Google:          "https://opencode.ai/inference/go/google/v1beta",
		},
	}
}

func TestLiveCatalogParsesIntoRoutableRecords(t *testing.T) {
	if os.Getenv("OPENCODE_LIVE") != "1" {
		t.Skip("set OPENCODE_LIVE=1 to probe the live Console Go catalog")
	}
	var env struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(fetchLiveCatalog(t), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if len(env.Data) == 0 {
		t.Fatal("catalog returned no models")
	}
	entries := make([]rawModel, 0, len(env.Data))
	for _, raw := range env.Data {
		var entry rawModel
		if err := json.Unmarshal(raw, &entry); err != nil {
			t.Fatalf("decode entry: %v", err)
		}
		entry.Raw = raw
		entries = append(entries, entry)
	}

	m := New(liveConsoleCfg(), nil)
	m.swap(entries)

	models := m.Models()
	if len(models) == 0 {
		t.Fatalf("no routable models; unsupported=%+v warnings=%v", m.Unsupported(), m.Warnings())
	}
	byID := map[string]ModelRecord{}
	for _, rec := range models {
		byID[rec.UpstreamID] = rec
	}
	glm, ok := byID["glm-5.2"]
	if !ok {
		t.Fatalf("glm-5.2 missing; got %d models", len(models))
	}
	if glm.DisplayName == "" || glm.DisplayName == glm.UpstreamID {
		t.Errorf("glm-5.2 display name not decoded: %q", glm.DisplayName)
	}
	if glm.ContextLimit <= 0 || glm.OutputLimit <= 0 {
		t.Errorf("glm-5.2 limits = %d/%d", glm.ContextLimit, glm.OutputLimit)
	}
	if glm.Protocol != RouteChatCompletions {
		t.Errorf("glm-5.2 protocol = %q", glm.Protocol)
	}
	if glm.EndpointURL == "" {
		t.Error("glm-5.2 endpoint URL empty")
	}
	if glm.Thinking == nil || len(glm.Thinking.Levels) == 0 {
		t.Errorf("glm-5.2 thinking not decoded: %+v", glm.Thinking)
	}
	if len(glm.Raw) == 0 {
		t.Error("glm-5.2 raw metadata not preserved")
	}
	for _, rec := range models {
		if rec.EndpointURL == "" {
			t.Errorf("%s: empty endpoint URL", rec.UpstreamID)
		}
		if rec.DisplayName == "" {
			t.Errorf("%s: empty display name", rec.UpstreamID)
		}
	}
	t.Logf("routable=%d unsupported=%d warnings=%d", len(models), len(m.Unsupported()), len(m.Warnings()))

	// Routing contract against real records: a Claude client prefers the
	// Anthropic endpoint under prefer-client-protocol, while native mode keeps
	// the model's own protocol.
	cfg := liveConsoleCfg()
	cfg.Routing = config.Routing{Mode: config.RoutingPreferClient, AllowTranslation: true}
	decision, eErr := ResolveRoute(cfg, glm, RouteMessages)
	if eErr != nil {
		t.Fatalf("prefer-client route: %v", eErr)
	}
	if decision.UpstreamProtocol != RouteMessages || decision.UpstreamURL != cfg.Endpoints.Messages {
		t.Errorf("prefer-client decision = %+v, want messages", decision)
	}
	cfg.Routing.Mode = config.RoutingNative
	native, eErr := ResolveRoute(cfg, glm, RouteMessages)
	if eErr != nil {
		t.Fatalf("native route: %v", eErr)
	}
	if native.UpstreamProtocol != glm.NativeProtocol {
		t.Errorf("native decision = %+v, want %s", native, glm.NativeProtocol)
	}
}
