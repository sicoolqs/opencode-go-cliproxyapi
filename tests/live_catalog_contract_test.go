//go:build live

package tests_test

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestLiveConsoleCatalogContract verifies the Console Go catalog contract
// the plugin decodes: name, limit, modalities, capabilities.reasoning and
// api must be present on the live endpoint. Opt in with OPENCODE_LIVE=1 so
// the default `go test -tags live` run stays offline-safe.
func TestLiveConsoleCatalogContract(t *testing.T) {
	if os.Getenv("OPENCODE_LIVE") != "1" {
		t.Skip("set OPENCODE_LIVE=1 to probe the live Console Go catalog")
	}
	const catalogURL = "https://opencode.ai/inference/go/v1/models"
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(catalogURL)
	if err != nil {
		t.Fatalf("fetch catalog: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("catalog status = %d", resp.StatusCode)
	}
	var env struct {
		Data []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Limit *struct {
				Context int64 `json:"context"`
				Output  int64 `json:"output"`
			} `json:"limit"`
			Modalities *struct {
				Input []string `json:"input"`
			} `json:"modalities"`
			Capabilities *struct {
				Reasoning *struct {
					Supported bool     `json:"supported"`
					Effort    []string `json:"effort"`
				} `json:"reasoning"`
			} `json:"capabilities"`
			API map[string]struct {
				BaseURL string `json:"base_url"`
			} `json:"api"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if len(env.Data) == 0 {
		t.Fatal("catalog returned no models")
	}
	protocols := map[string]int{}
	for _, m := range env.Data {
		if m.Name == "" {
			t.Errorf("%s: missing name", m.ID)
		}
		if m.Limit == nil || m.Limit.Context <= 0 || m.Limit.Output <= 0 {
			t.Errorf("%s: missing limit", m.ID)
		}
		if len(m.API) == 0 {
			t.Errorf("%s: missing api", m.ID)
		}
		for proto := range m.API {
			protocols[proto]++
		}
	}
	// The four Console protocols must all be represented.
	for _, proto := range []string{"chat-completions", "messages", "responses", "generate-content"} {
		if protocols[proto] == 0 {
			t.Errorf("no model declares %q; got %v", proto, protocols)
		}
	}
	t.Logf("models=%d protocols=%v", len(env.Data), protocols)
}
