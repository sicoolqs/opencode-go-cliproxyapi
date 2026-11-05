package catalog

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/config"
)

// consoleCatalog mirrors the live /inference/go/v1/models shape: name,
// limit, modalities, capabilities.reasoning and api.
const consoleCatalog = `{"object":"list","data":[
 {"id":"glm-5.2","name":"GLM 5.2","limit":{"context":1000000,"output":131072},
  "modalities":{"input":["text"],"output":["text"]},
  "capabilities":{"reasoning":{"supported":true,"effort":["high","max"]}},
  "api":{"chat-completions":{"base_url":"https://opencode.ai/inference/go/openai/v1"}}},
 {"id":"minimax-m3.1-flash","name":"MiniMax M3.1 Flash","limit":{"context":1000000,"output":512000},
  "modalities":{"input":["text","image/png"],"output":["text"]},
  "capabilities":{"reasoning":{"supported":true,"effort":["none","low","high"]}},
  "api":{"chat-completions":{"base_url":"https://opencode.ai/inference/go/openai/v1"}}},
 {"id":"claude-haiku-5-5","name":"Claude Haiku 5.5","limit":{"context":1000000,"output":128000},
  "capabilities":{"reasoning":{"supported":true,"effort":["low","medium","high"]}},
  "api":{"messages":{"base_url":"https://opencode.ai/inference/go/anthropic/v1"}}},
 {"id":"gpt-6-luna","name":"GPT 6 Luna","limit":{"context":1050000,"output":128000},
  "capabilities":{"reasoning":{"supported":true,"effort":["none","high"]}},
  "api":{"responses":{"base_url":"https://opencode.ai/inference/go/openai/v1"}}},
 {"id":"gemini-4-argon","name":"Gemini 4 Argon","limit":{"context":1048576,"output":65536},
  "capabilities":{"reasoning":{"supported":true,"effort":["low","high"]}},
  "api":{"generate-content":{"base_url":"https://opencode.ai/inference/go/google/v1beta"}}},
 {"id":"mystery-model","name":"Mystery","api":{"telepathy":{}}}
]}`

func consoleCfg() config.Config {
	cfg := testCfg()
	cfg.BaseURL = "https://opencode.ai/inference/go"
	cfg.Endpoints = config.Endpoints{
		ChatCompletions: "https://opencode.ai/inference/go/openai/v1/chat/completions",
		Messages:        "https://opencode.ai/inference/go/anthropic/v1/messages",
		Responses:       "https://opencode.ai/inference/go/openai/v1/responses",
		Google:          "https://opencode.ai/inference/go/google/v1beta",
	}
	cfg.Protocols.Google = true
	return cfg
}

func TestConsoleCatalogMetadataAndRouting(t *testing.T) {
	m := newManager(consoleCfg(), &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(consoleCatalog)}})
	mustRefresh(t, m)

	glm := findModel(t, m.Models(), "glm-5.2")
	if glm.DisplayName != "GLM 5.2" {
		t.Errorf("DisplayName = %q, want the catalog name", glm.DisplayName)
	}
	if glm.ContextLimit != 1000000 || glm.OutputLimit != 131072 {
		t.Errorf("limits = %d/%d", glm.ContextLimit, glm.OutputLimit)
	}
	if glm.Protocol != RouteChatCompletions {
		t.Errorf("glm protocol = %q", glm.Protocol)
	}
	if glm.EndpointURL != "https://opencode.ai/inference/go/openai/v1/chat/completions" {
		t.Errorf("glm endpoint = %q", glm.EndpointURL)
	}
	if glm.Thinking == nil || len(glm.Thinking.Levels) != 2 || glm.Thinking.Levels[0] != "high" {
		t.Errorf("glm thinking = %+v", glm.Thinking)
	}
	if len(glm.Raw) == 0 {
		t.Error("raw metadata was not preserved")
	}

	// api is authoritative: minimax-m3.1-flash is chat-completions even
	// though the legacy prefix table routes "minimax" to messages.
	mm := findModel(t, m.Models(), "minimax-m3.1-flash")
	if mm.Protocol != RouteChatCompletions {
		t.Errorf("minimax protocol = %q, want chat-completions from api", mm.Protocol)
	}
	if len(mm.InputModes) != 2 || mm.InputModes[1] != "image/png" {
		t.Errorf("minimax input modes = %v", mm.InputModes)
	}
	if mm.Thinking == nil || !mm.Thinking.ZeroAllowed {
		t.Errorf("minimax thinking = %+v, want ZeroAllowed for none", mm.Thinking)
	}

	claude := findModel(t, m.Models(), "claude-haiku-5-5")
	if claude.Protocol != RouteMessages || claude.EndpointURL != "https://opencode.ai/inference/go/anthropic/v1/messages" {
		t.Errorf("claude route = %q %q", claude.Protocol, claude.EndpointURL)
	}
	luna := findModel(t, m.Models(), "gpt-6-luna")
	if luna.Protocol != RouteResponses || luna.EndpointURL != "https://opencode.ai/inference/go/openai/v1/responses" {
		t.Errorf("luna route = %q %q", luna.Protocol, luna.EndpointURL)
	}

	// Google-native models fall back to chat-completions with a diagnostic
	// instead of being dropped.
	gem := findModel(t, m.Models(), "gemini-4-argon")
	if gem.Protocol != RouteChatCompletions {
		t.Errorf("gemini route = %q, want chat-completions fallback", gem.Protocol)
	}
	foundFallback := false
	for _, w := range m.Warnings() {
		if strings.Contains(w, "generate-content") {
			foundFallback = true
		}
	}
	if !foundFallback {
		t.Errorf("warnings = %v, want a generate-content fallback diagnostic", m.Warnings())
	}

	// Unknown protocol: excluded with a diagnostic, never silently dropped.
	foundMystery := false
	for _, u := range m.Unsupported() {
		if u.UpstreamID == "mystery-model" && strings.Contains(u.Reason, "telepathy") {
			foundMystery = true
		}
	}
	if !foundMystery {
		t.Errorf("unsupported = %+v, want mystery-model with protocol diagnostics", m.Unsupported())
	}
}

func TestConsoleCatalogLegacyFieldsStillDecode(t *testing.T) {
	legacy := `{"data":[{"id":"glm-5.2","display_name":"Legacy Name","context_length":4096,
		"max_output_tokens":1024,"input_modalities":["text"],
		"thinking":{"levels":["low","high"],"zero_allowed":true}}]}`
	m := newManager(testCfg(), &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(legacy)}})
	mustRefresh(t, m)
	rec := findModel(t, m.Models(), "glm-5.2")
	if rec.DisplayName != "Legacy Name" || rec.ContextLimit != 4096 || rec.OutputLimit != 1024 {
		t.Errorf("legacy metadata = %+v", rec)
	}
	if rec.Thinking == nil || len(rec.Thinking.Levels) != 2 || !rec.Thinking.ZeroAllowed {
		t.Errorf("legacy thinking = %+v", rec.Thinking)
	}
}
