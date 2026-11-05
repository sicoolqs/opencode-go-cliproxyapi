package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
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

const (
	diagGoogleCatalog = `{"data":[
		{"id":"glm-5.2","api":{"chat-completions":{}}},
		{"id":"gemini-4-argon","api":{"generate-content":{}}}
	]}`
	diagDuplicateCatalog = `{"data":[
		{"id":"glm-5.2","api":{"chat-completions":{}}},
		{"id":"dup","api":{"chat-completions":{}}},
		{"id":"dup","api":{"chat-completions":{}}}
	]}`
	diagCleanCatalog = `{"data":[{"id":"glm-5.2","api":{"chat-completions":{}}}]}`
)

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

// diagFixture builds a plugin Manager (the long-lived dedup owner) plus the
// config and recorder used to build fresh catalog managers.
func diagFixture(t *testing.T, body string) (config.Config, *Manager, *authRecorder) {
	t.Helper()
	cfg, err := config.Load([]byte("api-keys:\n  - value: sk-diag-key\n" +
		"catalog-url: https://catalog.test/models\n"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rec := &authRecorder{body: []byte(body)}
	return cfg, NewManager(NewHostBridge(rec.call)), rec
}

// TestCatalogDiagnosticsLoggedIndividually pins that each diagnostic reaches
// host.log with its own level and a self-explanatory message, and that a
// repeated refresh with unchanged diagnostics stays quiet.
func TestCatalogDiagnosticsLoggedIndividually(t *testing.T) {
	cfg, m, rec := diagFixture(t, diagGoogleCatalog)
	mgr := catalog.New(cfg, m.bridge)

	if err := m.refreshOnce(context.Background(), mgr, time.Second, cfg); err != nil {
		t.Fatalf("refreshOnce: %v", err)
	}
	logs := decodeLogs(t, rec.logCalls())
	if len(logs) != 1 {
		t.Fatalf("host.log calls = %d (%+v), want one info diagnostic", len(logs), logs)
	}
	info := logs[0]
	if info.Level != "info" {
		t.Errorf("level = %q, want info", info.Level)
	}
	if !strings.Contains(info.Message, "gemini-4-argon") ||
		!strings.Contains(info.Message, "generate-content") ||
		!strings.Contains(info.Message, "impact:") {
		t.Errorf("google fallback message incomplete: %q", info.Message)
	}
	if info.Fields["kind"] != string(catalog.DiagGoogleFallback) {
		t.Errorf("kind field = %v", info.Fields["kind"])
	}
	if info.Message == "catalog normalization warnings" {
		t.Errorf("generic summary message still emitted")
	}

	// Unchanged diagnostics must not re-log on the next refresh.
	before := len(rec.logCalls())
	if err := m.refreshOnce(context.Background(), mgr, time.Second, cfg); err != nil {
		t.Fatalf("second refreshOnce: %v", err)
	}
	if got := len(rec.logCalls()); got != before {
		t.Fatalf("unchanged refresh logged again: %d -> %d", before, got)
	}
}

// TestCatalogDiagnosticsSurviveReconfigure models CPA's startup sequence: each
// register/reconfigure builds a NEW catalog manager. The plugin-level dedup
// state must survive, so the same diagnostic is not replayed.
func TestCatalogDiagnosticsSurviveReconfigure(t *testing.T) {
	cfg, m, rec := diagFixture(t, diagGoogleCatalog)

	for cycle := 0; cycle < 5; cycle++ {
		// handleLifecycle constructs a fresh catalog manager every time.
		mgr := catalog.New(cfg, m.bridge)
		if err := m.refreshOnce(context.Background(), mgr, time.Second, cfg); err != nil {
			t.Fatalf("cycle %d refreshOnce: %v", cycle, err)
		}
	}
	logs := decodeLogs(t, rec.logCalls())
	if len(logs) != 1 {
		t.Fatalf("reconfigure cycles logged %d diagnostics, want exactly 1: %+v", len(logs), logs)
	}
}

// TestCatalogDiagnosticsRelogOnChange pins that changed content re-logs.
func TestCatalogDiagnosticsRelogOnChange(t *testing.T) {
	cfg, m, rec := diagFixture(t, diagCleanCatalog)
	if err := m.refreshOnce(context.Background(), catalog.New(cfg, m.bridge), time.Second, cfg); err != nil {
		t.Fatalf("clean refresh: %v", err)
	}
	if got := len(rec.logCalls()); got != 0 {
		t.Fatalf("clean catalog logged %d diagnostics", got)
	}

	rec.mu.Lock()
	rec.body = []byte(diagDuplicateCatalog)
	rec.mu.Unlock()
	if err := m.refreshOnce(context.Background(), catalog.New(cfg, m.bridge), time.Second, cfg); err != nil {
		t.Fatalf("changed refresh: %v", err)
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

// TestCatalogDiagnosticsRelogAfterDisappear pins that a diagnostic that
// disappears and later reappears is logged again.
func TestCatalogDiagnosticsRelogAfterDisappear(t *testing.T) {
	cfg, m, rec := diagFixture(t, diagGoogleCatalog)
	if err := m.refreshOnce(context.Background(), catalog.New(cfg, m.bridge), time.Second, cfg); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if got := len(rec.logCalls()); got != 1 {
		t.Fatalf("first refresh logs = %d, want 1", got)
	}

	rec.mu.Lock()
	rec.body = []byte(diagCleanCatalog)
	rec.mu.Unlock()
	if err := m.refreshOnce(context.Background(), catalog.New(cfg, m.bridge), time.Second, cfg); err != nil {
		t.Fatalf("clean refresh: %v", err)
	}
	if got := len(rec.logCalls()); got != 1 {
		t.Fatalf("clean refresh logged again: %d", got)
	}

	rec.mu.Lock()
	rec.body = []byte(diagGoogleCatalog)
	rec.mu.Unlock()
	if err := m.refreshOnce(context.Background(), catalog.New(cfg, m.bridge), time.Second, cfg); err != nil {
		t.Fatalf("reappear refresh: %v", err)
	}
	if got := len(rec.logCalls()); got != 2 {
		t.Fatalf("reappearing diagnostic logs = %d, want 2", got)
	}
}

// TestCatalogDiagnosticsConcurrentRefreshes pins that concurrent refreshes
// (each with its own catalog manager, as reconfigure can overlap a tick)
// cannot double-log the same diagnostic.
func TestCatalogDiagnosticsConcurrentRefreshes(t *testing.T) {
	cfg, m, rec := diagFixture(t, diagGoogleCatalog)

	const workers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			mgr := catalog.New(cfg, m.bridge)
			if err := m.refreshOnce(context.Background(), mgr, time.Second, cfg); err != nil {
				t.Errorf("concurrent refreshOnce: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	logs := decodeLogs(t, rec.logCalls())
	if len(logs) != 1 {
		t.Fatalf("concurrent refreshes logged %d diagnostics, want exactly 1: %+v", len(logs), logs)
	}
	if logs[0].Fields["kind"] != string(catalog.DiagGoogleFallback) {
		t.Fatalf("unexpected diagnostic: %+v", logs[0])
	}
}
