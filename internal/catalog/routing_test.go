package catalog

import (
	"strings"
	"testing"

	"opencode-go-cliproxyapi/internal/config"
)

// routeCfg builds a Console-shaped config with every protocol enabled.
func routeCfg(mode config.RoutingMode) config.Config {
	return config.Config{
		BaseURL:          "https://opencode.ai/inference/go",
		ModelPrefix:      config.ModelPrefix{Enabled: true, Value: "opencode-go"},
		MaxResponseBytes: 1 << 20,
		Catalog:          config.Catalog{StaleWhileUnavailable: true},
		Protocols:        config.Protocols{ChatCompletions: true, Messages: true, Responses: true, Google: true},
		Routing:          config.Routing{Mode: mode, AllowTranslation: true},
		Endpoints: config.Endpoints{
			ChatCompletions: "https://opencode.ai/inference/go/openai/v1/chat/completions",
			Messages:        "https://opencode.ai/inference/go/anthropic/v1/messages",
			Responses:       "https://opencode.ai/inference/go/openai/v1/responses",
		},
	}
}

// consoleRecord builds a Console catalog record with the given native protocol.
func consoleRecord(id string, native Route) ModelRecord {
	return ModelRecord{
		UpstreamID:         id,
		NativeProtocol:     native,
		Protocol:           native,
		SupportedProtocols: []Route{native},
		FromConsoleCatalog: true,
		OutputModes:        []string{"text"},
	}
}

// legacyRecord builds a pre-Console record (no api map): no cross-protocol
// guarantee.
func legacyRecord(id string, native Route) ModelRecord {
	return ModelRecord{
		UpstreamID:     id,
		NativeProtocol: native,
		Protocol:       native,
	}
}

func TestResolveRouteMatrix(t *testing.T) {
	tests := []struct {
		name   string
		mode   config.RoutingMode
		rec    ModelRecord
		client Route
		want   Route
	}{
		// T01-T03: Claude Code with a text model prefers messages.
		{"T01 claude+deepseek prefer-client", config.RoutingPreferClient, consoleRecord("deepseek-v4.1-flash", RouteChatCompletions), RouteMessages, RouteMessages},
		{"T02 claude+glm prefer-client", config.RoutingPreferClient, consoleRecord("glm-5.2", RouteChatCompletions), RouteMessages, RouteMessages},
		{"T03 claude+gpt prefer-client", config.RoutingPreferClient, consoleRecord("gpt-6-luna", RouteResponses), RouteMessages, RouteMessages},
		// T04-T05: matching client protocols stay native.
		{"T04 chat+glm prefer-client", config.RoutingPreferClient, consoleRecord("glm-5.2", RouteChatCompletions), RouteChatCompletions, RouteChatCompletions},
		{"T05 responses+gpt prefer-client", config.RoutingPreferClient, consoleRecord("gpt-6-luna", RouteResponses), RouteResponses, RouteResponses},
		// T06-T07: native and anthropic modes.
		{"T06 claude+glm native", config.RoutingNative, consoleRecord("glm-5.2", RouteChatCompletions), RouteMessages, RouteChatCompletions},
		{"T07 claude+glm anthropic", config.RoutingAnthropic, consoleRecord("glm-5.2", RouteChatCompletions), RouteMessages, RouteMessages},
		// Legacy catalogs keep native routing unless the model declares more.
		{"legacy claude+chat prefer-client", config.RoutingPreferClient, legacyRecord("glm-5.2", RouteChatCompletions), RouteMessages, RouteChatCompletions},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decision, eErr := ResolveRoute(routeCfg(tc.mode), tc.rec, tc.client)
			if eErr != nil {
				t.Fatalf("ResolveRoute: %v", eErr)
			}
			if decision.UpstreamProtocol != tc.want {
				t.Fatalf("upstream = %q, want %q (reason %q)", decision.UpstreamProtocol, tc.want, decision.Reason)
			}
			if decision.ConversionNeeded != (tc.client != tc.want) {
				t.Errorf("conversion = %t for client %q upstream %q", decision.ConversionNeeded, tc.client, tc.want)
			}
			if decision.UpstreamURL == "" {
				t.Error("upstream URL empty")
			}
			if decision.NativeProtocol != tc.rec.NativeProtocol {
				t.Errorf("native = %q, want %q", decision.NativeProtocol, tc.rec.NativeProtocol)
			}
		})
	}
}

// T08: an explicit override beats every mode.
func TestResolveRouteOverrideWins(t *testing.T) {
	cfg := routeCfg(config.RoutingPreferClient)
	cfg.RouteOverrides = map[string]config.RouteOverride{
		"glm-5.2": {Protocol: "responses", Endpoint: "/openai/v1/responses"},
	}
	decision, eErr := ResolveRoute(cfg, consoleRecord("glm-5.2", RouteChatCompletions), RouteMessages)
	if eErr != nil {
		t.Fatalf("ResolveRoute: %v", eErr)
	}
	if decision.UpstreamProtocol != RouteResponses {
		t.Fatalf("override ignored: %+v", decision)
	}
	if !strings.Contains(decision.Reason, "user override") {
		t.Errorf("reason = %q, want override diagnostic", decision.Reason)
	}
}

// T09: a protocol the model cannot serve falls back to native when
// translation is allowed, and fails explicitly when it is not.
func TestResolveRouteUnsupportedClientProtocol(t *testing.T) {
	rec := legacyRecord("glm-5.2", RouteChatCompletions)

	cfg := routeCfg(config.RoutingPreferClient)
	cfg.Routing.AllowTranslation = true
	decision, eErr := ResolveRoute(cfg, rec, RouteResponses)
	if eErr != nil {
		t.Fatalf("fallback: %v", eErr)
	}
	if decision.UpstreamProtocol != RouteChatCompletions {
		t.Fatalf("fallback upstream = %q, want native", decision.UpstreamProtocol)
	}

	cfg.Routing.AllowTranslation = false
	if _, eErr := ResolveRoute(cfg, rec, RouteResponses); eErr == nil {
		t.Fatal("expected an explicit error when translation is disabled")
	}
}

// Protocol switches are the allowed upstream set: prefer-client falls back to
// another enabled protocol instead of dropping the model.
func TestResolveRouteDisabledProtocolFallback(t *testing.T) {
	cfg := routeCfg(config.RoutingPreferClient)
	cfg.Protocols.Messages = false
	decision, eErr := ResolveRoute(cfg, consoleRecord("glm-5.2", RouteChatCompletions), RouteMessages)
	if eErr != nil {
		t.Fatalf("ResolveRoute: %v", eErr)
	}
	if decision.UpstreamProtocol != RouteChatCompletions {
		t.Fatalf("upstream = %q, want chat-completions fallback", decision.UpstreamProtocol)
	}
}

// anthropic mode cannot be satisfied without messages enabled.
func TestResolveRouteAnthropicRequiresMessages(t *testing.T) {
	cfg := routeCfg(config.RoutingAnthropic)
	cfg.Protocols.Messages = false
	if _, eErr := ResolveRoute(cfg, consoleRecord("glm-5.2", RouteChatCompletions), RouteMessages); eErr == nil {
		t.Fatal("anthropic mode must fail when messages is disabled")
	}
}

// Google-native models have no transport: prefer-client serves them through
// the client protocol, and native mode falls back with an explanatory reason.
func TestResolveRouteGoogleFallback(t *testing.T) {
	rec := consoleRecord("gemini-4-argon", RouteGoogle)
	rec.SupportedProtocols = []Route{RouteGoogle}

	preferred, eErr := ResolveRoute(routeCfg(config.RoutingPreferClient), rec, RouteMessages)
	if eErr != nil {
		t.Fatalf("prefer-client: %v", eErr)
	}
	if preferred.UpstreamProtocol != RouteMessages {
		t.Fatalf("prefer-client upstream = %q, want messages", preferred.UpstreamProtocol)
	}

	native, eErr := ResolveRoute(routeCfg(config.RoutingNative), rec, RouteMessages)
	if eErr != nil {
		t.Fatalf("native: %v", eErr)
	}
	if !IsTextProtocol(native.UpstreamProtocol) {
		t.Fatalf("native upstream = %q, want a text protocol", native.UpstreamProtocol)
	}
	if !strings.Contains(native.Reason, "no transport") {
		t.Errorf("reason = %q, want a transport diagnostic", native.Reason)
	}
}

// A non-text model is restricted to its native protocol: translation is not
// allowed, and disabling it surfaces an explicit error.
func TestResolveRouteNonTextModel(t *testing.T) {
	rec := consoleRecord("image-only", RouteResponses)
	rec.OutputModes = []string{"image"}

	cfg := routeCfg(config.RoutingPreferClient)
	decision, eErr := ResolveRoute(cfg, rec, RouteMessages)
	if eErr != nil {
		t.Fatalf("fallback to native: %v", eErr)
	}
	if decision.UpstreamProtocol != RouteResponses {
		t.Fatalf("upstream = %q, want native responses", decision.UpstreamProtocol)
	}

	cfg.Routing.AllowTranslation = false
	if _, eErr := ResolveRoute(cfg, rec, RouteMessages); eErr == nil {
		t.Fatal("non-text model must not be routed through another protocol")
	}
}
