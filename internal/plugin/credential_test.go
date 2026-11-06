package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/config"
)

// usageResponder answers catalog + credential probes. Probes are keyed by the
// Authorization header so each test can choose which keys are rejected.
func usageResponder(rejected map[string]bool, existing []pluginapi.HostAuthFileEntry) func(string, []byte) ([]byte, error) {
	return func(method string, payload []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return hostOK(hostAuthListResponse{Files: existing}), nil
		case pluginabi.MethodHostAuthSave:
			return hostOK(map[string]any{}), nil
		case pluginabi.MethodHostHTTPDo:
			var wire struct {
				URL     string      `json:"url"`
				Headers http.Header `json:"headers"`
			}
			_ = json.Unmarshal(payload, &wire)
			if strings.HasSuffix(wire.URL, "/usage") {
				auth := wire.Headers.Get("Authorization")
				key := strings.TrimPrefix(auth, "Bearer ")
				if rejected[key] {
					return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusUnauthorized}), nil
				}
				return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"usage":{}}`)}), nil
			}
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(testCatalogJSON)}), nil
		}
		return hostOK(map[string]any{}), nil
	}
}

type savedCredential struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Label    string `json:"label"`
	APIKey   string `json:"api_key"`
	Disabled bool   `json:"disabled"`
}

func savedCredentials(t *testing.T, f *fakeCaller) map[string]savedCredential {
	t.Helper()
	out := map[string]savedCredential{}
	for _, call := range f.callsOf(pluginabi.MethodHostAuthSave) {
		var wire pluginapi.HostAuthSaveRequest
		if err := json.Unmarshal(call.payload, &wire); err != nil {
			t.Fatalf("decode auth save: %v", err)
		}
		var rec savedCredential
		if err := json.Unmarshal(wire.JSON, &rec); err != nil {
			t.Fatalf("decode auth record: %v", err)
		}
		out[rec.APIKey] = rec
	}
	return out
}

func testConfig(t *testing.T, yamlText string) config.Config {
	t.Helper()
	cfg, err := config.Load([]byte(yamlText))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// A rejected key is persisted disabled; a healthy key stays enabled.
func TestMaterializeDisablesRejectedCredential(t *testing.T) {
	cfg := testConfig(t, "api-keys:\n  - value: sk-good\n    name: Good\n  - value: sk-bad\n    name: Bad\n")
	f := &fakeCaller{responder: usageResponder(map[string]bool{"sk-bad": true}, nil)}
	m := NewManager(NewHostBridge(f.call))

	if err := m.materializeAuthRecords(context.Background(), cfg); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	saved := savedCredentials(t, f)
	if !saved["sk-bad"].Disabled {
		t.Errorf("rejected credential not disabled: %+v", saved["sk-bad"])
	}
	if saved["sk-good"].Disabled {
		t.Errorf("healthy credential disabled: %+v", saved["sk-good"])
	}
	if saved["sk-bad"].ID != authIDFor("sk-bad") {
		t.Errorf("id = %q, want key hash id", saved["sk-bad"].ID)
	}

	// The ERROR log names the key fingerprint but never the key itself.
	var found bool
	for _, call := range f.callsOf(pluginabi.MethodHostLog) {
		blob := string(call.payload)
		if !strings.Contains(blob, "rejected by upstream") {
			continue
		}
		found = true
		if strings.Contains(blob, "sk-bad") {
			t.Fatalf("log leaks the rejected key: %s", blob)
		}
		if !strings.Contains(blob, keyFingerprint("sk-bad")) {
			t.Errorf("log missing key fingerprint: %s", blob)
		}
	}
	if !found {
		t.Error("missing rejected-credential error log")
	}
}

// With no healthy key left, the rejected credential stays enabled so clients
// receive a clear 401 instead of an opaque 503 empty pool.
func TestMaterializeKeepsLastRejectedCredentialEnabled(t *testing.T) {
	cfg := testConfig(t, "api-keys:\n  - value: sk-only\n")
	f := &fakeCaller{responder: usageResponder(map[string]bool{"sk-only": true}, nil)}
	m := NewManager(NewHostBridge(f.call))

	if err := m.materializeAuthRecords(context.Background(), cfg); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	saved := savedCredentials(t, f)
	if saved["sk-only"].Disabled {
		t.Errorf("last credential must stay enabled: %+v", saved["sk-only"])
	}
	var logged bool
	for _, call := range f.callsOf(pluginabi.MethodHostLog) {
		if strings.Contains(string(call.payload), "no healthy key remains") {
			logged = true
		}
	}
	if !logged {
		t.Error("missing last-key protection diagnostic")
	}
}

// A credential whose key was removed from config is disabled, not left to be
// scheduled; the file content is preserved apart from the disabled flag.
func TestMaterializeDisablesRemovedCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Personal.json")
	oldID := authIDFor("sk-old")
	raw := []byte(`{"type":"opencode-go","id":"` + oldID + `","label":"Personal","api_key":"sk-old","custom":"keep"}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	existing := []pluginapi.HostAuthFileEntry{{
		ID: oldID, Name: "Personal.json", Provider: ProviderID, Path: path,
	}}
	cfg := testConfig(t, "api-keys:\n  - value: sk-new\n")
	f := &fakeCaller{responder: usageResponder(map[string]bool{}, existing)}
	m := NewManager(NewHostBridge(f.call))

	if err := m.materializeAuthRecords(context.Background(), cfg); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// The fake host records AuthSave instead of touching the file; assert the
	// submitted record keeps unrelated fields and adds the disabled flag.
	var rewritten []byte
	for _, call := range f.callsOf(pluginabi.MethodHostAuthSave) {
		var wire pluginapi.HostAuthSaveRequest
		if err := json.Unmarshal(call.payload, &wire); err != nil {
			t.Fatalf("decode auth save: %v", err)
		}
		if wire.Name == "Personal.json" {
			rewritten = wire.JSON
		}
	}
	if rewritten == nil {
		t.Fatal("removed credential was not rewritten")
	}
	var persisted map[string]any
	if err := json.Unmarshal(rewritten, &persisted); err != nil {
		t.Fatalf("decode rewritten record: %v", err)
	}
	if persisted["disabled"] != true {
		t.Errorf("removed credential not disabled: %s", rewritten)
	}
	if persisted["custom"] != "keep" {
		t.Errorf("unrelated fields lost: %s", rewritten)
	}
	saved := savedCredentials(t, f)
	if saved["sk-new"].Disabled {
		t.Errorf("new credential disabled: %+v", saved["sk-new"])
	}
	var warned bool
	for _, call := range f.callsOf(pluginabi.MethodHostLog) {
		if strings.Contains(string(call.payload), "no longer configured") {
			warned = true
		}
	}
	if !warned {
		t.Error("missing removed-credential diagnostic")
	}
}

// An inconclusive probe (network failure) must not flip host-managed state.
func TestMaterializeInconclusiveProbePreservesState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Personal.json")
	id := authIDFor("sk-keep")
	raw := []byte(`{"type":"opencode-go","id":"` + id + `","label":"Personal","api_key":"sk-keep","disabled":true}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	existing := []pluginapi.HostAuthFileEntry{{ID: id, Name: "Personal.json", Provider: ProviderID, Path: path}}
	cfg := testConfig(t, "api-keys:\n  - value: sk-keep\n")
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			return hostOK(hostAuthListResponse{Files: existing}), nil
		}
		if method == pluginabi.MethodHostHTTPDo {
			return hostErr("upstream", "probe unavailable"), nil
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))

	if err := m.materializeAuthRecords(context.Background(), cfg); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if got := len(f.callsOf(pluginabi.MethodHostAuthSave)); got != 0 {
		t.Fatalf("inconclusive probe rewrote credentials: %d saves", got)
	}
}
