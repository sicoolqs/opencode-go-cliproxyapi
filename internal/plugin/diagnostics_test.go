package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"opencode-go-cliproxyapi/internal/catalog"
	"opencode-go-cliproxyapi/internal/config"
)

type capturedLog struct {
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields"`
}

func decodeLogs(t *testing.T, raw [][]byte) []capturedLog {
	t.Helper()
	out := make([]capturedLog, 0, len(raw))
	for _, payload := range raw {
		var entry capturedLog
		if err := json.Unmarshal(payload, &entry); err != nil {
			t.Fatalf("decode host.log payload: %v", err)
		}
		out = append(out, entry)
	}
	return out
}

// TestCatalogDiagnosticsLoggedIndividually pins that each diagnostic reaches
// host.log with its own level and a self-explanatory message, and that a
// repeated refresh with unchanged diagnostics stays quiet.
func TestCatalogDiagnosticsLoggedIndividually(t *testing.T) {
	cfg, err := config.Load([]byte("api-keys:\n  - value: sk-diag-key\n" +
		"catalog-url: https://catalog.test/models\n"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rec := &authRecorder{body: []byte(`{"data":[
		{"id":"glm-5.2","api":{"chat-completions":{}}},
		{"id":"gemini-4-argon","api":{"generate-content":{}}},
		{"id":"no-route-model"}
	]}`)}
	bridge := NewHostBridge(rec.call)
	mgr := catalog.New(cfg, bridge)

	if err := refreshOnce(context.Background(), mgr, bridge, time.Second, cfg); err != nil {
		t.Fatalf("refreshOnce: %v", err)
	}
	logs := decodeLogs(t, rec.logCalls())
	if len(logs) != 2 {
		t.Fatalf("host.log calls = %d (%+v), want one info + one warn", len(logs), logs)
	}

	byLevel := map[string]capturedLog{}
	for _, entry := range logs {
		byLevel[entry.Level] = entry
	}
	info, ok := byLevel["info"]
	if !ok {
		t.Fatalf("missing info diagnostic: %+v", logs)
	}
	if !strings.Contains(info.Message, "gemini-4-argon") ||
		!strings.Contains(info.Message, "generate-content") ||
		!strings.Contains(info.Message, "impact:") {
		t.Errorf("google fallback message incomplete: %q", info.Message)
	}
	if info.Fields["kind"] != string(catalog.DiagGoogleFallback) {
		t.Errorf("google fallback kind field = %v", info.Fields["kind"])
	}

	warn, ok := byLevel["warn"]
	if !ok {
		t.Fatalf("missing warn diagnostic: %+v", logs)
	}
	if !strings.Contains(warn.Message, "no-route-model") ||
		!strings.Contains(warn.Message, "excluded from the routable catalog") ||
		!strings.Contains(warn.Message, "invalid_model") {
		t.Errorf("unroutable message incomplete: %q", warn.Message)
	}
	if warn.Fields["kind"] != string(catalog.DiagUnroutable) {
		t.Errorf("unroutable kind field = %v", warn.Fields["kind"])
	}

	for _, entry := range logs {
		if entry.Message == "catalog normalization warnings" ||
			entry.Message == "unsupported models excluded from routable catalog" {
			t.Errorf("generic summary message still emitted: %q", entry.Message)
		}
	}

	// Unchanged diagnostics must not re-log on the next refresh.
	before := len(rec.logCalls())
	if err := refreshOnce(context.Background(), mgr, bridge, time.Second, cfg); err != nil {
		t.Fatalf("second refreshOnce: %v", err)
	}
	if got := len(rec.logCalls()); got != before {
		t.Fatalf("unchanged refresh logged again: %d -> %d", before, got)
	}
}

// TestCatalogDiagnosticsRelogOnChange pins that a changed diagnostic set is
// logged again.
func TestCatalogDiagnosticsRelogOnChange(t *testing.T) {
	cfg, err := config.Load([]byte("api-keys:\n  - value: sk-diag-key\n" +
		"catalog-url: https://catalog.test/models\n"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rec := &authRecorder{body: []byte(`{"data":[{"id":"glm-5.2","api":{"chat-completions":{}}}]}`)}
	bridge := NewHostBridge(rec.call)
	mgr := catalog.New(cfg, bridge)

	if err := refreshOnce(context.Background(), mgr, bridge, time.Second, cfg); err != nil {
		t.Fatalf("refreshOnce: %v", err)
	}
	if got := len(rec.logCalls()); got != 0 {
		t.Fatalf("clean catalog logged %d diagnostics", got)
	}

	rec.mu.Lock()
	rec.body = []byte(`{"data":[{"id":"glm-5.2","api":{"chat-completions":{}}},{"id":"dup","api":{"chat-completions":{}}},{"id":"dup","api":{"chat-completions":{}}}]}`)
	rec.mu.Unlock()
	if err := refreshOnce(context.Background(), mgr, bridge, time.Second, cfg); err != nil {
		t.Fatalf("second refreshOnce: %v", err)
	}
	logs := decodeLogs(t, rec.logCalls())
	if len(logs) != 1 || logs[0].Level != "warn" ||
		logs[0].Fields["kind"] != string(catalog.DiagDuplicateModel) {
		t.Fatalf("changed diagnostics = %+v, want one duplicate warn", logs)
	}
	if !strings.Contains(logs[0].Message, `duplicate model "dup"`) {
		t.Errorf("duplicate message incomplete: %q", logs[0].Message)
	}
}
