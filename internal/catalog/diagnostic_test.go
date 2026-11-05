package catalog

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/config"
)

// diagnosticCfg is a Console-shaped config with an override and every
// protocol enabled.
func diagnosticCfg() config.Config {
	cfg := testCfg()
	cfg.BaseURL = "https://opencode.ai/inference/go"
	cfg.Protocols = config.Protocols{ChatCompletions: true, Messages: true, Responses: true, Google: true}
	cfg.Endpoints = config.Endpoints{
		ChatCompletions: "https://opencode.ai/inference/go/openai/v1/chat/completions",
		Messages:        "https://opencode.ai/inference/go/anthropic/v1/messages",
		Responses:       "https://opencode.ai/inference/go/openai/v1/responses",
		Google:          "https://opencode.ai/inference/go/google/v1beta",
	}
	cfg.RouteOverrides = map[string]config.RouteOverride{
		"override-model": {Protocol: "messages", Endpoint: "/anthropic/v1/messages"},
	}
	return cfg
}

const diagnosticCatalog = `{"data":[
 {"id":"glm-5.2","api":{"chat-completions":{}}},
 {"id":"gemini-4-argon","api":{"generate-content":{}}},
 {"id":"override-model","api":{"chat-completions":{}}},
 {"id":"dup","api":{"chat-completions":{}}},
 {"id":"dup","api":{"chat-completions":{}}},
 {"id":"no-route-model"}
]}`

func findDiag(t *testing.T, diags []Diagnostic, kind DiagnosticKind, model string) Diagnostic {
	t.Helper()
	for _, d := range diags {
		if d.Kind == kind && d.ModelID == model {
			return d
		}
	}
	t.Fatalf("diagnostic %s for %q not found in %+v", kind, model, diags)
	return Diagnostic{}
}

// TestDiagnosticsClassification pins the kind/level/message contract for each
// diagnostic type.
func TestDiagnosticsClassification(t *testing.T) {
	m := newManager(diagnosticCfg(), &fakeClient{resp: pluginapi.HTTPResponse{
		StatusCode: 200,
		Body:       []byte(diagnosticCatalog),
	}})
	mustRefresh(t, m)
	diags := m.Diagnostics()

	google := findDiag(t, diags, DiagGoogleFallback, "gemini-4-argon")
	if google.Level != DiagnosticLevelInfo {
		t.Errorf("google fallback level = %q, want info", google.Level)
	}
	if !strings.Contains(google.Message, "gemini-4-argon") ||
		!strings.Contains(google.Message, "generate-content") ||
		!strings.Contains(google.Message, "impact:") {
		t.Errorf("google fallback message incomplete: %q", google.Message)
	}
	// The catalog must not claim a concrete fallback protocol.
	for _, claim := range []string{"defaulting to chat-completions", "defaulting to messages", "defaulting to responses"} {
		if strings.Contains(google.Message, claim) {
			t.Errorf("google fallback claims a fixed protocol: %q", google.Message)
		}
	}

	override := findDiag(t, diags, DiagRouteOverride, "override-model")
	if override.Level != DiagnosticLevelInfo {
		t.Errorf("override level = %q, want info", override.Level)
	}
	if !strings.Contains(override.Message, "messages") || !strings.Contains(override.Message, "impact:") {
		t.Errorf("override message incomplete: %q", override.Message)
	}

	dup := findDiag(t, diags, DiagDuplicateModel, "dup")
	if dup.Level != DiagnosticLevelWarn {
		t.Errorf("duplicate level = %q, want warn", dup.Level)
	}
	if !strings.Contains(dup.Message, `duplicate model "dup"`) || !strings.Contains(dup.Message, "impact:") {
		t.Errorf("duplicate message incomplete: %q", dup.Message)
	}

	unroutable := findDiag(t, diags, DiagUnroutable, "no-route-model")
	if unroutable.Level != DiagnosticLevelWarn {
		t.Errorf("unroutable level = %q, want warn", unroutable.Level)
	}
	if !strings.Contains(unroutable.Message, "no-route-model") ||
		!strings.Contains(unroutable.Message, "no route determined") ||
		!strings.Contains(unroutable.Message, "invalid_model") {
		t.Errorf("unroutable message incomplete: %q", unroutable.Message)
	}
}

// TestDiagnosticsShapeAnomaly checks the catalog-shape diagnostic level and
// message when the upstream response has no data field.
func TestDiagnosticsShapeAnomaly(t *testing.T) {
	m := newManager(diagnosticCfg(), &fakeClient{resp: pluginapi.HTTPResponse{
		StatusCode: 200,
		Body:       []byte(`{"object":"list"}`),
	}})
	mustRefresh(t, m)
	var shape *Diagnostic
	for i := range m.Diagnostics() {
		if m.Diagnostics()[i].Kind == DiagCatalogShape {
			d := m.Diagnostics()[i]
			shape = &d
			break
		}
	}
	if shape == nil {
		t.Fatalf("no catalog-shape diagnostic in %+v", m.Diagnostics())
	}
	if shape.Level != DiagnosticLevelWarn {
		t.Errorf("shape level = %q, want warn", shape.Level)
	}
	if !strings.Contains(shape.Message, `missing "data" field`) || !strings.Contains(shape.Message, "impact:") {
		t.Errorf("shape message incomplete: %q", shape.Message)
	}
}

// TestDiagnosticsCollision checks the id-collision diagnostic kind/level and
// that the loser stays visible through Unsupported().
func TestDiagnosticsCollision(t *testing.T) {
	cfg := diagnosticCfg()
	cfg.RouteOverrides = nil
	m := newManager(cfg, &fakeClient{resp: pluginapi.HTTPResponse{
		StatusCode: 200,
		Body:       []byte(`{"data":[{"id":"foo","api":{"chat-completions":{}}},{"id":"opencode-go/foo","api":{"chat-completions":{}}}]}`),
	}})
	mustRefresh(t, m)

	var collision *Diagnostic
	for _, d := range m.Diagnostics() {
		if d.Kind == DiagIDCollision {
			dd := d
			collision = &dd
		}
	}
	if collision == nil {
		t.Fatalf("no id-collision diagnostic in %+v", m.Diagnostics())
	}
	if collision.Level != DiagnosticLevelWarn {
		t.Errorf("collision level = %q, want warn", collision.Level)
	}
	if !strings.Contains(collision.Message, "collides with upstream id") ||
		!strings.Contains(collision.Message, "invalid_model") {
		t.Errorf("collision message incomplete: %q", collision.Message)
	}
	if len(m.Unsupported()) != 1 {
		t.Errorf("Unsupported() = %+v, want the collision loser", m.Unsupported())
	}
}
