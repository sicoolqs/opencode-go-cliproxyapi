package plugin

import (
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

// countTokensResponder answers catalog, credential probes and the Anthropic
// count_tokens proxy.
func countTokensResponder(countBody string) func(string, []byte) ([]byte, error) {
	return wrapWithCatalog(multiRouteCatalog, func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			if method == pluginabi.MethodHostAuthList {
				return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{}}), nil
			}
			return hostOK(map[string]any{}), nil
		}
		var wire struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(payload, &wire)
		switch {
		case strings.HasSuffix(wire.URL, "/usage"):
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"usage":{}}`)}), nil
		case strings.HasSuffix(wire.URL, "/messages/count_tokens"):
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(countBody)}), nil
		}
		return hostOK(map[string]any{}), nil
	})
}

func TestCountTokensProxiesUpstream(t *testing.T) {
	f := &fakeCaller{responder: countTokensResponder(`{"input_tokens":42}`)}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	body := []byte(`{"model":"opencode-go/glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	raw, err := m.HandleCall(pluginabi.MethodExecutorCountTokens,
		execReqBody("opencode-go/glm-5.3", "claude", body, false))
	if err != nil {
		t.Fatalf("count_tokens: %v", err)
	}
	env := decodeEnv(t, raw)
	if !env.OK {
		t.Fatalf("envelope = %+v", env.Error)
	}
	var out pluginapi.ExecutorResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("result: %v", err)
	}
	if string(out.Payload) != `{"input_tokens":42}` {
		t.Fatalf("payload = %s", out.Payload)
	}

	var proxy map[string]any
	for _, call := range f.callsOf(pluginabi.MethodHostHTTPDo) {
		var wire map[string]any
		if json.Unmarshal(call.payload, &wire) == nil {
			if url, _ := wire["url"].(string); strings.HasSuffix(url, "/messages/count_tokens") {
				proxy = wire
			}
		}
	}
	if proxy == nil {
		t.Fatal("count_tokens was not proxied upstream")
	}
	if got := proxy["url"]; got != "https://opencode.ai/inference/go/anthropic/v1/messages/count_tokens" {
		t.Errorf("url = %v", got)
	}
	headers := proxy["headers"].(map[string]any)
	if _, ok := headers["X-Api-Key"]; !ok {
		t.Errorf("missing x-api-key header: %v", headers)
	}
	upstreamBody := string(wireBody(t, proxy, "body"))
	if !strings.Contains(upstreamBody, `"model":"glm-5.3"`) {
		t.Errorf("model not rewritten: %s", upstreamBody)
	}
}

func TestCountTokensRejectsNonClaudeFormat(t *testing.T) {
	f := &fakeCaller{responder: countTokensResponder(`{"input_tokens":1}`)}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	raw, _ := m.HandleCall(pluginabi.MethodExecutorCountTokens,
		execReqBody("opencode-go/glm-5.3", "openai", []byte(`{"model":"x","messages":[]}`), false))
	env := decodeEnv(t, raw)
	if env.OK || env.Error == nil || env.Error.Code != "unsupported_protocol_or_parameter" {
		t.Fatalf("envelope = %+v, want unsupported", env.Error)
	}
}

// A periodic refresh that gets a 401 disables the credential.
func TestRefreshAuthDisablesRejectedCredential(t *testing.T) {
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDo {
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusUnauthorized}), nil
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{Endpoints: config.Endpoints{Usage: "https://usage.test/usage"}}

	raw, err := m.HandleCall(pluginabi.MethodAuthRefresh, mustJSON(struct{ pluginapi.AuthRefreshRequest }{
		pluginapi.AuthRefreshRequest{AuthID: "id", AuthProvider: ProviderID, Attributes: map[string]string{"api_key": "sk-rejected"}},
	}))
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	var resp pluginapi.AuthRefreshResponse
	decodeResult(t, raw, &resp)
	if !resp.Auth.Disabled {
		t.Fatalf("rejected credential not disabled: %+v", resp.Auth)
	}
	if resp.Auth.Attributes["api_key"] != "sk-rejected" {
		t.Errorf("attributes lost: %+v", resp.Auth.Attributes)
	}
	if resp.NextRefreshAfter.IsZero() {
		t.Error("next refresh not scheduled")
	}
	var logged bool
	for _, call := range f.callsOf(pluginabi.MethodHostLog) {
		blob := string(call.payload)
		if strings.Contains(blob, "during refresh") && !strings.Contains(blob, "sk-rejected") {
			logged = true
		}
	}
	if !logged {
		t.Error("missing refresh rejection log")
	}
}

// The key-file poll applies additions and skips unchanged content.
func TestPollKeyFileOnceAppliesAndSkipsUnchanged(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "keys.yaml")
	if err := os.WriteFile(keyFile, []byte("api-keys:\n  - value: sk-file\n    name: File\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeCaller{responder: usageResponder(map[string]bool{}, nil)}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	yamlText := "api-keys:\n  - value: sk-base\n    name: Base\n" +
		"api-keys-file: " + keyFile + "\n" +
		"api-keys-file-poll: 10s\n"
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(yamlText)); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := len(f.callsOf(pluginabi.MethodHostAuthSave)); got != 2 {
		t.Fatalf("initial saves = %d, want 2 (inline + file)", got)
	}
	if m.pollKeyFileOnce() {
		t.Fatal("unchanged key file was reapplied")
	}

	if err := os.WriteFile(keyFile, []byte("api-keys:\n  - value: sk-file\n    name: File\n  - value: sk-extra\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !m.pollKeyFileOnce() {
		t.Fatal("changed key file was not applied")
	}
	saved := savedCredentials(t, f)
	if saved["sk-extra"].Disabled {
		t.Errorf("new file key disabled: %+v", saved["sk-extra"])
	}
	if saved["sk-extra"].ID != authIDFor("sk-extra") {
		t.Errorf("new file key id = %q", saved["sk-extra"].ID)
	}
	if m.pollKeyFileOnce() {
		t.Fatal("unchanged key file reapplied after change")
	}
}

// Applying a new key file set adds the new credential and retires the old one.
func TestApplyAPIKeysAddsAndRetires(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "Personal.json")
	oldID := authIDFor("sk-old")
	if err := os.WriteFile(oldPath, []byte(`{"type":"opencode-go","id":"`+oldID+`","label":"Personal","api_key":"sk-old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{{
				ID: oldID, Name: "Personal.json", Provider: ProviderID, Path: oldPath,
			}}}), nil
		case pluginabi.MethodHostHTTPDo:
			var wire struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(payload, &wire)
			if strings.HasSuffix(wire.URL, "/usage") {
				return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"usage":{}}`)}), nil
			}
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(testCatalogJSON)}), nil
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody("api-keys:\n  - value: sk-old\n    name: Personal\n")); err != nil {
		t.Fatalf("register: %v", err)
	}

	m.applyAPIKeys([]config.APIKey{{Value: "sk-new", Name: "New"}})

	saved := savedCredentials(t, f)
	if saved["sk-new"].Disabled {
		t.Errorf("new credential disabled: %+v", saved["sk-new"])
	}
	if !saved["sk-old"].Disabled {
		t.Errorf("retired credential not disabled: %+v", saved["sk-old"])
	}
	m.mu.RLock()
	keys := append([]config.APIKey(nil), m.cfg.APIKeys...)
	m.mu.RUnlock()
	if len(keys) != 1 || keys[0].Value != "sk-new" {
		t.Errorf("active keys = %+v", keys)
	}
}

// healthCheckResponder serves two pre-existing credential files on disk and a
// mutable rejection set for /usage probes, so runHealthCheckOnce's file
// reconciliation (setAuthDisabled) operates on real files. The AuthList
// entries re-read the files so the reported disabled state reflects what the
// reconciliation wrote — a real host reports file state the same way.
func healthCheckResponder(t *testing.T, dir string, rejected map[string]bool) func(string, []byte) ([]byte, error) {
	t.Helper()
	paths := map[string]string{
		"A.json": writeCredentialFile(t, dir, "A.json", "sk-a"),
		"B.json": writeCredentialFile(t, dir, "B.json", "sk-b"),
	}
	listFiles := func() []pluginapi.HostAuthFileEntry {
		files := make([]pluginapi.HostAuthFileEntry, 0, len(paths))
		for name, path := range paths {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read credential file: %v", err)
			}
			var rec savedCredential
			if json.Unmarshal(raw, &rec) != nil {
				t.Fatalf("decode credential file: %v", err)
			}
			files = append(files, pluginapi.HostAuthFileEntry{
				ID: rec.ID, Name: name, Provider: ProviderID, Path: path, Disabled: rec.Disabled,
			})
		}
		return files
	}
	return func(method string, payload []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return hostOK(hostAuthListResponse{Files: listFiles()}), nil
		case pluginabi.MethodHostAuthSave:
			// Persist like the real host: write the record to the file so a
			// later AuthList reflects it.
			var wire pluginapi.HostAuthSaveRequest
			if json.Unmarshal(payload, &wire) == nil {
				if path, ok := paths[wire.Name]; ok {
					if err := os.WriteFile(path, wire.JSON, 0o600); err != nil {
						t.Fatalf("persist credential file: %v", err)
					}
				}
			}
			return hostOK(map[string]any{}), nil
		case pluginabi.MethodHostHTTPDo:
			var wire struct {
				URL     string      `json:"url"`
				Headers http.Header `json:"headers"`
			}
			_ = json.Unmarshal(payload, &wire)
			if strings.HasSuffix(wire.URL, "/usage") {
				key := strings.TrimPrefix(wire.Headers.Get("Authorization"), "Bearer ")
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

func writeCredentialFile(t *testing.T, dir, name, key string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	record := `{"type":"opencode-go","id":"` + authIDFor(key) + `","label":"` + strings.TrimSuffix(name, ".json") + `","api_key":"` + key + `"}`
	if err := os.WriteFile(path, []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The self-driven health check disables a key that turned invalid, keeps the
// last credential enabled when every key is rejected, and re-enables a key
// that recovers.
func TestRunHealthCheckOnceReconcilesCredentials(t *testing.T) {
	rejected := map[string]bool{}
	f := &fakeCaller{responder: healthCheckResponder(t, t.TempDir(), rejected)}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	yamlText := "api-keys:\n  - value: sk-a\n    name: A\n  - value: sk-b\n    name: B\n"
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(yamlText)); err != nil {
		t.Fatalf("register: %v", err)
	}

	// sk-a turns invalid while sk-b stays healthy: sk-a is disabled.
	rejected["sk-a"] = true
	if !m.runHealthCheckOnce() {
		t.Fatal("health check did not run")
	}
	saved := savedCredentials(t, f)
	if !saved["sk-a"].Disabled || saved["sk-b"].Disabled {
		t.Errorf("after sk-a rejection: a=%+v b=%+v", saved["sk-a"], saved["sk-b"])
	}

	// sk-b fails too: with no healthy key left, nothing new may be disabled
	// (an empty pool would turn clear 401s into an opaque 503).
	rejected["sk-b"] = true
	if !m.runHealthCheckOnce() {
		t.Fatal("health check did not run (both rejected)")
	}
	saved = savedCredentials(t, f)
	if saved["sk-b"].Disabled {
		t.Errorf("last healthy credential was disabled: %+v", saved["sk-b"])
	}

	// sk-b recovers while sk-a stays rejected: sk-a gets disabled again.
	delete(rejected, "sk-b")
	if !m.runHealthCheckOnce() {
		t.Fatal("health check did not run (sk-b recovered)")
	}
	saved = savedCredentials(t, f)
	if !saved["sk-a"].Disabled || saved["sk-b"].Disabled {
		t.Errorf("after recovery: a=%+v b=%+v", saved["sk-a"], saved["sk-b"])
	}

	// sk-a recovers as well: both credentials are enabled again.
	delete(rejected, "sk-a")
	if !m.runHealthCheckOnce() {
		t.Fatal("health check did not run (sk-a recovered)")
	}
	saved = savedCredentials(t, f)
	if saved["sk-a"].Disabled || saved["sk-b"].Disabled {
		t.Errorf("after full recovery: a=%+v b=%+v", saved["sk-a"], saved["sk-b"])
	}
}

// A refresh response echoes the persisted disabled flag unless the probe
// definitively rejects the key — it must never reset disabled to enabled.
func TestRefreshAuthEchoesStoredDisabled(t *testing.T) {
	responder := func(rejected bool) func(string, []byte) ([]byte, error) {
		return func(method string, _ []byte) ([]byte, error) {
			if method == pluginabi.MethodHostHTTPDo {
				status := http.StatusOK
				if rejected {
					status = http.StatusUnauthorized
				}
				return hostOK(pluginapi.HTTPResponse{StatusCode: status, Body: []byte(`{"usage":{}}`)}), nil
			}
			return hostOK(map[string]any{}), nil
		}
	}
	for _, tc := range []struct {
		name     string
		storage  string
		rejected bool
		want     bool
	}{
		{name: "stored-disabled-stays-disabled", storage: `{"type":"opencode-go","id":"x","api_key":"sk-x","disabled":true}`, want: true},
		{name: "stored-enabled-stays-enabled", storage: `{"type":"opencode-go","id":"x","api_key":"sk-x"}`, want: false},
		{name: "rejection-wins", storage: `{"type":"opencode-go","id":"x","api_key":"sk-x"}`, rejected: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCaller{responder: responder(tc.rejected)}
			m := NewManager(NewHostBridge(f.call))
			m.cfg = config.Config{Endpoints: config.Endpoints{Usage: "https://usage.test/usage"}}

			raw, err := m.HandleCall(pluginabi.MethodAuthRefresh, mustJSON(struct{ pluginapi.AuthRefreshRequest }{
				pluginapi.AuthRefreshRequest{
					AuthID: "x", AuthProvider: ProviderID, StorageJSON: []byte(tc.storage),
					Attributes: map[string]string{"api_key": "sk-x"},
				},
			}))
			if err != nil {
				t.Fatalf("refresh: %v", err)
			}
			var resp pluginapi.AuthRefreshResponse
			decodeResult(t, raw, &resp)
			if resp.Auth.Disabled != tc.want {
				t.Errorf("disabled = %t, want %t", resp.Auth.Disabled, tc.want)
			}
		})
	}
}
