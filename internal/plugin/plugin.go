package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/catalog"
	"opencode-go-cliproxyapi/internal/config"
	"opencode-go-cliproxyapi/internal/errclass"
)

// ProviderID is the single provider key served by this plugin (FR-001).
const ProviderID = "opencode-go"

// pluginName / pluginVersion are reported in registration metadata.
const (
	pluginName    = "opencode-go-cliproxyapi"
	pluginVersion = "0.2.2"
)

// githubRepoURL satisfies the host's validPlugin gate (host.go
// validPlugin rejects empty Metadata.GitHubRepository).
const githubRepoURL = "https://github.com/massiveits/opencode-go-cliproxyapi"

// registerRefreshTimeout bounds ONLY the synchronous initial/reconfigure
// refreshOnce so a slow catalog cannot block host startup/reconfigure for a
// full request-timeout (default 5m). On expiry registration proceeds per
// FR-002 empty/stale semantics; the ticker retries at refresh-interval with
// the full request-timeout.
const registerRefreshTimeout = 10 * time.Second

// Manager owns dispatcher state (config snapshot, catalog manager, refresh
// loop) and routes every RPC method. Safe for concurrent HandleCall use.
type Manager struct {
	bridge *HostBridge // immutable after NewManager

	// lifeMu serializes whole register/reconfigure/shutdown sequences so
	// their stop-wait-install steps cannot interleave into orphaned tickers.
	lifeMu sync.Mutex

	mu  sync.RWMutex
	cfg config.Config
	mgr *catalog.Manager
	// stop/done manage the one background refresh goroutine; both nil
	// when no loop is running.
	stop chan struct{}
	done chan struct{}

	// diagMu guards diagActive, the set of catalog diagnostics currently
	// present. It lives on the plugin Manager (not the catalog snapshot) so
	// register/reconfigure — which build a new catalog manager — do not
	// replay identical diagnostics. A diagnostic logs when it enters the set
	// and stays quiet while it remains; a changed or reappearing diagnostic
	// is new again.
	diagMu     sync.Mutex
	diagActive map[string]struct{}
}

// NewManager returns a dispatcher whose outbound traffic flows through bridge.
func NewManager(bridge *HostBridge) *Manager {
	return &Manager{bridge: bridge}
}

// HandleCall dispatches one RPC method and returns envelope bytes. Handler
// failures travel inside the envelope; a recovered panic becomes a
// "plugin_error" envelope so the host process never dies with us.
func (m *Manager) HandleCall(method string, request []byte) (resp []byte, err error) {
	debugTrace("handle method=%s request_bytes=%d", method, len(request))
	defer func() {
		// Defensive: host-boundary panics are goroutine-contained (see
		// HostBridge.callWithTimeout); this guards future handler bugs.
		if r := recover(); r != nil {
			resp = ErrEnvelope("plugin_error", fmt.Sprintf("internal error handling %s", method))
			err = nil
		}
	}()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return m.handleLifecycle(request)
	case pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		return m.handleModels()
	case pluginabi.MethodPluginShutdown:
		return m.handleShutdown()
	case pluginabi.MethodExecutorExecute:
		return m.handleExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return m.handleExecuteStream(request)
	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID}), nil
	case pluginabi.MethodAuthParse:
		var req pluginapi.AuthParseRequest
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth parse request body"), nil
		}
		m.mu.RLock()
		cfg := m.cfg
		m.mu.RUnlock()
		resp, err := (authProvider{cfg: cfg}).ParseAuth(context.Background(), req)
		if err != nil {
			return ErrEnvelope("auth_failure", err.Error()), nil
		}
		debugTrace("auth parse response provider=%s id=%s attr_api_key_present=%t storage_json_bytes=%d", resp.Auth.Provider, resp.Auth.ID, strings.TrimSpace(resp.Auth.Attributes["api_key"]) != "", len(resp.Auth.StorageJSON))
		return okEnvelope(resp), nil
	case pluginabi.MethodAuthLoginStart:
		var req struct {
			pluginapi.AuthLoginStartRequest
		}
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth login request body"), nil
		}
		_, err := (authProvider{}).StartLogin(context.Background(), req.AuthLoginStartRequest)
		return ErrEnvelope("unsupported", err.Error()), nil
	case pluginabi.MethodAuthLoginPoll:
		var req struct{ pluginapi.AuthLoginPollRequest }
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth poll request body"), nil
		}
		_, err := (authProvider{}).PollLogin(context.Background(), req.AuthLoginPollRequest)
		return ErrEnvelope("unsupported", err.Error()), nil
	case pluginabi.MethodAuthRefresh:
		var req struct{ pluginapi.AuthRefreshRequest }
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth refresh request body"), nil
		}
		return okEnvelope(m.refreshAuth(context.Background(), req.AuthRefreshRequest)), nil
	case pluginabi.MethodQuotaIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID}), nil
	case pluginabi.MethodQuotaDescribe:
		return okEnvelope(pluginapi.QuotaDescribeResponse{SupportedProviders: []string{ProviderID}, DisplayName: "OpenCode Go"}), nil
	case pluginabi.MethodQuotaFetch:
		var req pluginapi.QuotaFetchRequest
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed quota request"), nil
		}
		result, err := m.FetchQuota(context.Background(), req)
		if err != nil {
			return ErrEnvelope("quota_failure", err.Error()), nil
		}
		return okEnvelope(result), nil
	case pluginabi.MethodQuotaReset:
		return ErrEnvelope("unsupported", "OpenCode Go does not support quota resets"), nil
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID}), nil
	case pluginabi.MethodExecutorCountTokens:
		return m.handleCountTokens(request)
	case pluginabi.MethodExecutorHTTPRequest:
		return classEnvelope(&errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: fmt.Sprintf("%s endpoint is not supported by opencode-go", pluginabi.MethodExecutorHTTPRequest),
		}), nil
	default:
		return ErrEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// lifecycleRequest mirrors rpcLifecycleRequest: config_yaml is base64.
// The request's schema_version is decoded-and-ignored; registration echoes
// pluginabi.SchemaVersion.
type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type capabilities struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope,omitempty"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
	QuotaProvider         bool                         `json:"quota_provider"`
	ManagementAPI         bool                         `json:"management_api"`
}

type registrationResult struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

func pluginConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{
			Name:        "api-keys",
			Type:        pluginapi.ConfigFieldTypeArray,
			Description: "List of OpenCode Go API keys (`- value: ...`, optional `name: Personal`). Supports ${ENV_VAR} expansion.",
		},
		{
			Name:        "api-keys-file",
			Type:        pluginapi.ConfigFieldTypeString,
			Description: "Optional YAML file with additional API keys (same shape as api-keys); re-read periodically so rotation needs no restart.",
		},
		{
			Name:        "api-keys-file-poll",
			Type:        pluginapi.ConfigFieldTypeString,
			Description: "Poll interval for api-keys-file (default: 1m, minimum: 10s).",
		},
		{
			Name:        "base-url",
			Type:        pluginapi.ConfigFieldTypeString,
			Description: "Legacy single upstream base URL (e.g. https://opencode.ai/zen/go/v1). When set, per-protocol URLs are derived from it; when unset the Console Go endpoints are used.",
		},
		{
			Name:        "catalog-url",
			Type:        pluginapi.ConfigFieldTypeString,
			Description: "Optional model catalog URL (default: https://opencode.ai/inference/go/v1/models).",
		},
		{
			Name:        "chat-completions-url",
			Type:        pluginapi.ConfigFieldTypeString,
			Description: "Optional Chat Completions endpoint (default: https://opencode.ai/inference/go/openai/v1/chat/completions).",
		},
		{
			Name:        "messages-url",
			Type:        pluginapi.ConfigFieldTypeString,
			Description: "Optional Anthropic Messages endpoint (default: https://opencode.ai/inference/go/anthropic/v1/messages).",
		},
		{
			Name:        "responses-url",
			Type:        pluginapi.ConfigFieldTypeString,
			Description: "Optional OpenAI Responses endpoint (default: https://opencode.ai/inference/go/openai/v1/responses).",
		},
		{
			Name:        "google-url",
			Type:        pluginapi.ConfigFieldTypeString,
			Description: "Optional Google generateContent base (default: https://opencode.ai/inference/go/google/v1beta).",
		},
		{
			Name:        "usage-url",
			Type:        pluginapi.ConfigFieldTypeString,
			Description: "Optional quota usage endpoint (default: https://opencode.ai/zen/go/v1/usage).",
		},
		{
			Name:        "model-prefix",
			Type:        pluginapi.ConfigFieldTypeObject,
			Description: "Client-facing model ID prefix configuration (`enabled: bool`, `value: string`).",
		},
		{
			Name:        "catalog",
			Type:        pluginapi.ConfigFieldTypeObject,
			Description: "Catalog discovery settings (`refresh-interval: string`, `stale-while-unavailable: bool`).",
		},
		{
			Name:        "protocols",
			Type:        pluginapi.ConfigFieldTypeObject,
			Description: "Protocol enable/disable switches (`chat-completions: bool`, `messages: bool`, `responses: bool`, `google: bool`). These define the allowed upstream protocol set.",
		},
		{
			Name:        "routing",
			Type:        pluginapi.ConfigFieldTypeObject,
			Description: "Protocol routing policy (`mode: native|prefer-client-protocol|anthropic`, `allow-translation: bool`).",
		},
		{
			Name:        "route-overrides",
			Type:        pluginapi.ConfigFieldTypeObject,
			Description: "Explicit route overrides per model (`<model-id>: {protocol: string, endpoint: string}`).",
		},
		{
			Name:        "request-timeout",
			Type:        pluginapi.ConfigFieldTypeString,
			Description: "Upstream HTTP request timeout (default: 5m).",
		},
		{
			Name:        "max-response-bytes",
			Type:        pluginapi.ConfigFieldTypeInteger,
			Description: "Max non-streaming response body size in bytes (default: 67108864).",
		},
		{
			Name:        "allow-http",
			Type:        pluginapi.ConfigFieldTypeBoolean,
			Description: "Allow plain http:// URLs for local testing/mocking (default: false).",
		},
	}
}

func registrationEnvelope() []byte {
	formats := []string{"openai", "claude", "openai-response"}
	return okEnvelope(registrationResult{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginName,
			GitHubRepository: githubRepoURL,
			ConfigFields:     pluginConfigFields(),
		},
		Capabilities: capabilities{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  formats,
			ExecutorOutputFormats: formats,
			QuotaProvider:         true,
		},
	})
}

// handleLifecycle implements plugin.register / plugin.reconfigure: load
// config, refresh the catalog once, publish state, restart the refresh
// loop. Registration succeeds even when the initial refresh fails (FR-002).
func (m *Manager) handleLifecycle(request []byte) ([]byte, error) {
	var req lifecycleRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed lifecycle request body"), nil
	}
	cfg, err := config.Load(req.ConfigYAML)
	if err != nil {
		debugTrace("lifecycle config_error=%s", err.Error())
		return ErrEnvelope("invalid_config", err.Error()), nil
	}
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	debugTrace("lifecycle config_loaded key_count=%d prefix_enabled=%t prefix=%s", len(cfg.APIKeys), cfg.ModelPrefix.Enabled, cfg.ModelPrefix.Value)
	ctx, cancel := context.WithTimeout(context.Background(), registerRefreshTimeout)
	defer cancel()
	if err := m.materializeAuthRecords(ctx, cfg); err != nil {
		return ErrEnvelope("auth_materialization_failed", err.Error()), nil
	}
	// A nil *HostBridge must not enter the interface as a typed nil, or
	// catalog's nil-client guard never fires and Refresh panics inside Do.
	var client catalog.HostClient
	if m.bridge != nil {
		client = m.bridge
	}
	mgr := catalog.New(cfg, client)
	refreshErr := m.refreshOnce(context.Background(), mgr, registerRefreshTimeout, cfg)
	debugTrace("lifecycle refresh_complete model_count=%d refresh_error=%t", len(mgr.Models()), refreshErr != nil)

	// Retire any running loop and wait for its exit outside m.mu: a mid-refresh
	// tick must never stall readers holding RLock (F4). lifeMu keeps the
	// stop-wait-install sequence atomic against other lifecycles.
	if oldDone := m.closeStop(); oldDone != nil {
		<-oldDone
	}

	m.mu.Lock()
	m.cfg = cfg
	// FR-002 stale-while-unavailable: a failed refresh must neither wipe a
	// non-empty previous snapshot nor keep the OLD manager, whose stored cfg
	// would pin the old base-url/catalog-url/protocols/prefix so ticks
	// fetched the old URL with the new config's keys forever (F5). The NEW
	// manager is adopted unconditionally; when a previous snapshot exists
	// AND the NEW config keeps stale-while-unavailable enabled, it is
	// seeded into the new one, so serving is unchanged until the next
	// successful tick refreshes against the NEW config. Under fail-closed
	// (stale-while-unavailable:false) no seed happens — same as the ticker
	// failure path — so a reconfigure during an outage serves nothing until
	// a refresh succeeds, honoring the operator's policy on BOTH paths.
	if refreshErr != nil && cfg.Catalog.StaleWhileUnavailable && m.mgr != nil && len(m.mgr.Models()) > 0 {
		mgr.SeedFrom(m.mgr)
	}
	m.mgr = mgr
	stop, done := make(chan struct{}), make(chan struct{})
	m.stop, m.done = stop, done
	interval := cfg.Catalog.RefreshInterval
	m.mu.Unlock()

	m.startRefreshLoop(cfg, mgr, interval, stop, done)
	m.startKeyFilePoll(stop)
	return registrationEnvelope(), nil
}

// startKeyFilePoll re-reads api-keys-file and applies changes without a
// restart. It exits when stop closes (reconfigure or shutdown).
func (m *Manager) startKeyFilePoll(stop <-chan struct{}) {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()
	if cfg.APIKeysFile == "" || m.bridge == nil {
		return
	}
	interval := cfg.APIKeysFilePoll
	if interval <= 0 {
		interval = config.DefaultAPIKeysFilePoll
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				m.pollKeyFileOnce()
			}
		}
	}()
}

// pollKeyFileOnce re-reads the key file and applies a changed key set. It
// reports whether an update was applied.
func (m *Manager) pollKeyFileOnce() bool {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()
	if cfg.APIKeysFile == "" || m.bridge == nil {
		return false
	}
	fileKeys, err := config.LoadKeysFile(cfg.APIKeysFile)
	if err != nil {
		_ = m.bridge.Log("warn", "opencode-go: api-keys-file reload failed",
			map[string]any{"reason": "unreadable-or-invalid-file"})
		return false
	}
	merged := config.MergeAPIKeys(cfg.ConfigAPIKeys, fileKeys)
	if sameAPIKeys(merged, cfg.APIKeys) {
		return false
	}
	m.applyAPIKeys(merged)
	return true
}

// applyAPIKeys installs a new merged key set: credentials are reconciled and
// the catalog refresh loop is restarted so it uses the new keys.
func (m *Manager) applyAPIKeys(keys []config.APIKey) {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()

	m.mu.Lock()
	if m.mgr == nil {
		m.mu.Unlock()
		return
	}
	cfg := m.cfg
	cfg.APIKeys = keys
	m.cfg = cfg
	mgr := m.mgr
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), registerRefreshTimeout)
	defer cancel()
	if err := m.materializeAuthRecords(ctx, cfg); err != nil {
		if m.bridge != nil {
			_ = m.bridge.Log("warn", "opencode-go: api-keys-file credential update failed",
				map[string]any{"reason": "materialize-failed"})
		}
		return
	}

	if oldDone := m.closeStop(); oldDone != nil {
		<-oldDone
	}
	stop, done := make(chan struct{}), make(chan struct{})
	m.mu.Lock()
	m.stop, m.done = stop, done
	m.mu.Unlock()
	m.startRefreshLoop(cfg, mgr, cfg.Catalog.RefreshInterval, stop, done)
	m.startKeyFilePoll(stop)
	if m.bridge != nil {
		_ = m.bridge.Log("info", "opencode-go: api-keys-file reloaded and applied",
			map[string]any{"key_count": len(keys)})
	}
}

// sameAPIKeys reports whether two key sets are identical in order and labels.
func sameAPIKeys(a, b []config.APIKey) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Value != b[i].Value || a[i].Name != b[i].Name {
			return false
		}
	}
	return true
}

// Credential lifecycle: configured keys are probed at register/reconfigure,
// rejected keys are disabled so the host stops scheduling them, and records
// whose key is no longer configured are disabled (the host ABI has no delete).
const (
	credentialProbeTimeout = 10 * time.Second
	credentialFingerprint  = 8 // hex chars of sha256 shown in logs
)

type credentialHealth int

const (
	credentialUnknown credentialHealth = iota
	credentialHealthy
	credentialRejected
)

// keyHash returns the full sha256 hex of a key (auth identity).
func keyHash(key string) string {
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:])
}

// keyFingerprint returns a short, non-secret identifier for logs.
func keyFingerprint(key string) string {
	fp := keyHash(key)
	if len(fp) > credentialFingerprint {
		return fp[:credentialFingerprint]
	}
	return fp
}

func authIDFor(key string) string {
	return "opencode-go-key-" + keyHash(key)
}

// probeCredential asks the upstream usage endpoint whether the key is
// accepted. Only a definitive 401 disables a credential: 5xx and transport
// errors are transient and must never quarantine a working key.
func (m *Manager) probeCredential(ctx context.Context, cfg config.Config, key string) credentialHealth {
	if m.bridge == nil || strings.TrimSpace(key) == "" {
		return credentialUnknown
	}
	url := strings.TrimSpace(cfg.Endpoints.Usage)
	if url == "" {
		base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
		if base == "" {
			return credentialUnknown
		}
		url = base + "/usage"
	}
	probeCtx, cancel := context.WithTimeout(ctx, credentialProbeTimeout)
	defer cancel()
	resp, err := m.bridge.Do(probeCtx, pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     url,
		Headers: http.Header{"Authorization": {"Bearer " + key}, "Accept": {"application/json"}},
	})
	if err != nil {
		return credentialUnknown
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return credentialHealthy
	case http.StatusUnauthorized:
		return credentialRejected
	}
	return credentialUnknown
}

// authFileRecord is the persisted plugin credential shape.
type authFileRecord struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Label    string `json:"label"`
	APIKey   string `json:"api_key"`
	Disabled bool   `json:"disabled,omitempty"`
}

// refreshAuth re-validates a static key during a host-scheduled refresh.
// A 401 disables the credential; a healthy or inconclusive probe leaves the
// host-managed state untouched.
func (m *Manager) refreshAuth(ctx context.Context, req pluginapi.AuthRefreshRequest) pluginapi.AuthRefreshResponse {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()
	auth := pluginapi.AuthData{
		Provider: req.AuthProvider, ID: req.AuthID, StorageJSON: req.StorageJSON,
		Metadata: req.Metadata, Attributes: req.Attributes,
	}
	key := strings.TrimSpace(req.Attributes["api_key"])
	if key != "" {
		switch m.probeCredential(ctx, cfg, key) {
		case credentialRejected:
			auth.Disabled = true
			if m.bridge != nil {
				_ = m.bridge.Log("error", fmt.Sprintf(
					"opencode-go: credential rejected by upstream (401) for key fingerprint %s during periodic refresh; impact: credential disabled; action: rotate the API key",
					keyFingerprint(key)),
					map[string]any{"key_fingerprint": keyFingerprint(key), "reason": "credential-rejected", "disabled": true})
			}
		case credentialHealthy:
			// Do not clear a host-managed disabled flag from a refresh.
		}
	}
	return pluginapi.AuthRefreshResponse{Auth: auth, NextRefreshAfter: time.Now().Add(credentialRefreshInterval)}
}

// materializeAuthRecords makes CPA-visible auth files idempotently. Configured
// keys are probed; rejected keys are disabled when at least one other key is
// healthy (a single rejected key stays enabled so clients receive a clear 401
// instead of an opaque 503). Records whose key is no longer configured are
// disabled instead of left to poison scheduling.
func (m *Manager) materializeAuthRecords(ctx context.Context, cfg config.Config) error {
	if m.bridge == nil {
		return nil
	}
	entries, err := m.bridge.AuthList(ctx)
	if err != nil {
		return fmt.Errorf("list existing auth records: %w", err)
	}

	type existingAuth struct {
		name     string
		path     string
		disabled bool
	}
	existing := make(map[string]existingAuth, len(entries))
	reserved := make(map[string]struct{}, len(entries)*2)
	for _, entry := range entries {
		id := strings.TrimSpace(entry.ID)
		if name := strings.TrimSpace(entry.Name); name != "" {
			reserved[name] = struct{}{}
		}
		if id != "" {
			reserved[id] = struct{}{}
		}
		if entry.Provider != ProviderID && entry.Type != ProviderID &&
			!strings.HasPrefix(id, "opencode-go-key-") {
			continue
		}
		rec := existingAuth{name: strings.TrimSpace(entry.Name), path: entry.Path, disabled: entry.Disabled}
		// Before the auth manager is ready, v8 lists disk files without IDs.
		// Recover identity from the host-reported path when needed.
		if id == "" || rec.name == "" {
			if !filepath.IsAbs(entry.Path) {
				return fmt.Errorf("existing auth record has no absolute path")
			}
			raw, readErr := os.ReadFile(entry.Path)
			if readErr != nil {
				return fmt.Errorf("cannot read existing plugin auth record")
			}
			var fileRec authFileRecord
			if json.Unmarshal(raw, &fileRec) != nil || fileRec.APIKey == "" {
				return fmt.Errorf("existing plugin auth record is invalid")
			}
			if id == "" {
				id = authIDFor(fileRec.APIKey)
			}
			if rec.name == "" {
				rec.name = filepath.Base(entry.Path)
				reserved[rec.name] = struct{}{}
			}
			rec.disabled = rec.disabled || fileRec.Disabled
		}
		if id == "" {
			continue
		}
		existing[id] = rec
		reserved[id] = struct{}{}
	}

	// Probe configured keys concurrently; registration latency stays bounded by
	// one probe timeout rather than one per key.
	health := make([]credentialHealth, len(cfg.APIKeys))
	var wg sync.WaitGroup
	for i, entry := range cfg.APIKeys {
		wg.Add(1)
		go func(i int, key string) {
			defer wg.Done()
			health[i] = m.probeCredential(ctx, cfg, key)
		}(i, entry.Value)
	}
	wg.Wait()
	healthy := 0
	for _, h := range health {
		if h == credentialHealthy {
			healthy++
		}
	}
	// Disabling every credential would turn clear 401s into an opaque 503
	// "no auth available"; keep them enabled when none validated healthy.
	allowDisable := healthy > 0

	configured := make(map[string]struct{}, len(cfg.APIKeys))
	for _, entry := range cfg.APIKeys {
		configured[authIDFor(entry.Value)] = struct{}{}
	}

	// Retire credentials whose key is no longer configured.
	for id, rec := range existing {
		if _, ok := configured[id]; ok {
			continue
		}
		if rec.disabled {
			continue
		}
		if !allowDisable {
			_ = m.bridge.Log("warn", fmt.Sprintf(
				"opencode-go: credential %q is no longer configured but stays enabled because no configured key is healthy; impact: it may still be scheduled",
				rec.name), map[string]any{"file": rec.name, "reason": "no-healthy-key"})
			continue
		}
		if err := m.setAuthDisabled(ctx, rec.name, rec.path, true); err != nil {
			_ = m.bridge.Log("warn", "opencode-go: failed to disable removed credential", map[string]any{"file": rec.name})
			continue
		}
		_ = m.bridge.Log("warn", fmt.Sprintf(
			"opencode-go: credential %q is no longer configured and was disabled; impact: it is skipped by the scheduler, the file may be deleted manually",
			rec.name), map[string]any{"file": rec.name, "reason": "removed-from-config"})
	}

	// Ensure configured credentials exist with the right enabled state.
	for i, entry := range cfg.APIKeys {
		id := authIDFor(entry.Value)
		label := accountLabel(cfg, entry.Value, "", i)
		desiredDisabled := health[i] == credentialRejected && allowDisable
		rec, exists := existing[id]
		if exists {
			switch {
			case desiredDisabled && !rec.disabled:
				if err := m.setAuthDisabled(ctx, rec.name, rec.path, true); err != nil {
					_ = m.bridge.Log("warn", "opencode-go: failed to disable credential", map[string]any{"file": rec.name})
				}
			case !desiredDisabled && rec.disabled && health[i] == credentialHealthy:
				// Only a positive probe re-enables: an inconclusive probe must
				// not override host-managed state.
				if err := m.setAuthDisabled(ctx, rec.name, rec.path, false); err != nil {
					_ = m.bridge.Log("warn", "opencode-go: failed to enable credential", map[string]any{"file": rec.name})
				}
			}
		} else {
			name := availableAuthName(label, reserved)
			record, marshalErr := json.Marshal(authFileRecord{
				Type: ProviderID, ID: id, Label: label, APIKey: entry.Value, Disabled: desiredDisabled,
			})
			if marshalErr != nil {
				return fmt.Errorf("build auth record")
			}
			if err := m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: name, JSON: record}); err != nil {
				return err
			}
			reserved[name] = struct{}{}
			debugTrace("auth materialized id=%s file=%s disabled=%t", id, name, desiredDisabled)
		}
		if health[i] == credentialRejected {
			_ = m.bridge.Log("error", fmt.Sprintf(
				"opencode-go: credential rejected by upstream (401) for key fingerprint %s; impact: %s; action: rotate the API key and reload the config",
				keyFingerprint(entry.Value), rejectedImpact(desiredDisabled)),
				map[string]any{"key_fingerprint": keyFingerprint(entry.Value), "reason": "credential-rejected", "disabled": desiredDisabled})
		}
	}
	return nil
}

// rejectedImpact describes the consequence of a rejected credential.
func rejectedImpact(disabled bool) string {
	if disabled {
		return "credential disabled and skipped by the scheduler"
	}
	return "no healthy key remains, so requests fail with 401 until the key is rotated"
}

// setAuthDisabled rewrites an existing auth file's disabled flag, preserving
// every other field. A missing path is treated as a no-op failure.
func (m *Manager) setAuthDisabled(ctx context.Context, name, path string, disabled bool) error {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return fmt.Errorf("credential file path unavailable")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil || len(record) == 0 {
		return fmt.Errorf("invalid credential file")
	}
	if disabled {
		record["disabled"] = true
	} else {
		delete(record, "disabled")
	}
	updated, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" {
		name = filepath.Base(path)
	}
	return m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: name, JSON: updated})
}

// handleModels implements model.static / model.for_auth (FR-003): the last
// good catalog snapshot mapped to wire ModelInfos; empty catalog yields an
// empty slice, not an error.
func (m *Manager) handleModels() ([]byte, error) {
	m.mu.RLock()
	mgr := m.mgr
	m.mu.RUnlock()
	models := make([]pluginapi.ModelInfo, 0)
	if mgr != nil {
		for _, rec := range mgr.Models() {
			models = append(models, pluginapi.ModelInfo{
				ID:                        rec.PublicID,
				Object:                    "model",
				OwnedBy:                   ProviderID,
				DisplayName:               rec.DisplayName,
				ContextLength:             rec.ContextLimit,
				MaxCompletionTokens:       rec.OutputLimit,
				SupportedInputModalities:  rec.InputModes,
				SupportedOutputModalities: rec.OutputModes,
				Thinking:                  rec.Thinking,
			})
		}
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: ProviderID, Models: models}), nil
}

// shutdownDrainTimeout bounds how long handleShutdown waits for orphaned
// host-callback goroutines (timed-out callbacks and their abandon/drain
// cleanup) to finish before returning. Package var so tests can shrink it.
var shutdownDrainTimeout = 15 * time.Second

// handleShutdown stops the refresh loop, drains orphaned host callbacks,
// and clears state. The drain matters on Unix: the SDK loader frees host_api
// and dlclose's the plugin immediately after this export returns
// (loader_unix.go), so a goroutine still calling into the host would crash
// the process — see COMPATIBILITY.md limitations.
func (m *Manager) handleShutdown() ([]byte, error) {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	oldDone := m.closeStop()
	if oldDone != nil {
		<-oldDone
	}
	if m.bridge != nil && !m.bridge.WaitForInFlight(shutdownDrainTimeout) {
		// Non-Windows unload during this residual window can crash the
		// host; the warn makes the stall visible without blocking forever.
		_ = m.bridge.Log("warn", "shutdown proceeding with host callbacks still in flight", nil)
	}
	m.mu.Lock()
	m.cfg, m.mgr = config.Config{}, nil
	m.mu.Unlock()
	return okEnvelope(struct{}{}), nil
}

// startRefreshLoop spawns the single background ticker goroutine over the
// caller-supplied stop/done pair (already stored under m.mu) and the served
// catalog manager. The loop captures its own cfg/mgr/bridge snapshot so
// it never contends on m.mu; reconfigure swaps state and restarts the loop.
func (m *Manager) startRefreshLoop(cfg config.Config, mgr *catalog.Manager, interval time.Duration, stop, done chan struct{}) {
	// F4: tick refresh contexts derive from stopCtx so close(stop) aborts
	// an in-flight Refresh immediately instead of leaving lifeMu held until
	// the old config's request-timeout expires. The watcher goroutine is
	// required because the loop body blocks inside refreshOnce while a tick
	// runs and cannot select on stop itself.
	stopCtx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stop
		cancel()
	}()
	go func() {
		ticker := time.NewTicker(interval)
		defer close(done)
		defer ticker.Stop()
		defer cancel()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				func() {
					defer func() {
						// CGO bridge calls can panic; a tick must never
						// take the host process down.
						if r := recover(); r != nil && m.bridge != nil {
							_ = m.bridge.Log("error", "catalog refresh panicked", nil)
						}
					}()
					m.refreshOnce(stopCtx, mgr, cfg.RequestTimeout, cfg)
				}()
			}
		}
	}()
}

// closeStop signals a running loop to exit and clears the stop/done pair.
// It returns the loop's done channel — nil when no loop was running — and
// the CALLER waits on it after this returns, never while holding m.mu (F4).
func (m *Manager) closeStop() chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stop == nil {
		return nil
	}
	done := m.done
	close(m.stop)
	m.stop, m.done = nil, nil
	return done
}

// refreshOnce runs one bounded catalog refresh. Catalog refresh is not a client
// request, so the fallback uses the first configured key only and has no client
// selection, rotation, cooldown, or retry state. parent bounds-and-cancels
// the attempt: the lifecycle path passes context.Background() plus
// registerRefreshTimeout; ticker ticks pass the loop's stop-derived context
// plus the full request-timeout, so close(stop) aborts an in-flight tick
// (F4). On failure it logs a warn via host.log — error text is a redacted
// category label from the catalog package, never key material (FR-011).
func (m *Manager) refreshOnce(parent context.Context, mgr *catalog.Manager, timeout time.Duration, cfg config.Config) error {
	// Fallback: configured materialization input is catalog-only; CPA client
	// authentication remains owned by its AuthProvider and CPA.
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	err := mgr.Refresh(ctx, cfg.APIKeys[0].Value)
	if err != nil {
		if m.bridge != nil {
			_ = m.bridge.Log("warn", "catalog refresh failed", map[string]any{"error": err.Error()})
		}
		return err
	}
	// FR-010: surface every catalog diagnostic individually with its own
	// level, so Docker's default output names the model, cause and impact
	// instead of a generic summary.
	m.logCatalogDiagnostics(mgr)
	return nil
}

// logCatalogDiagnostics forwards each diagnostic that newly entered the
// active set to host.log at its own level. Dedup state lives on the plugin
// Manager and survives register/reconfigure; a diagnostic logs once while it
// remains present, re-logs when its content changes, and re-logs after it
// disappears and reappears. The active-set swap happens under diagMu so
// concurrent refreshes cannot double-log the same diagnostic.
func (m *Manager) logCatalogDiagnostics(mgr *catalog.Manager) {
	if m.bridge == nil {
		return
	}
	diags := mgr.Diagnostics()
	current := make(map[string]struct{}, len(diags))
	for _, d := range diags {
		current[d.Key()] = struct{}{}
	}

	m.diagMu.Lock()
	fresh := make([]catalog.Diagnostic, 0, len(diags))
	for _, d := range diags {
		if _, seen := m.diagActive[d.Key()]; !seen {
			fresh = append(fresh, d)
		}
	}
	m.diagActive = current
	m.diagMu.Unlock()

	for _, d := range fresh {
		fields := make(map[string]any, len(d.Fields)+1)
		for k, v := range d.Fields {
			fields[k] = v
		}
		fields["kind"] = string(d.Kind)
		level := d.Level
		if level == "" {
			level = "warn"
		}
		_ = m.bridge.Log(level, d.Message, fields)
	}
}

func okEnvelope(result any) []byte {
	raw, _ := json.Marshal(result)
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}

func ErrEnvelope(code, message string) []byte {
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: message}})
	return out
}

// Use a readable filename because management clients may show it instead of Label.
func availableAuthName(label string, existing map[string]struct{}) string {
	base := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, label)
	base = strings.Trim(base, "-")
	if base == "" {
		base = "OpenCode-Go"
	}
	runes := []rune(base)
	if len(runes) > 80 {
		base = string(runes[:80])
	}
	name := base + ".json"
	for n := 2; ; n++ {
		if _, exists := existing[name]; !exists {
			return name
		}
		name = fmt.Sprintf("%s-%d.json", base, n)
	}
}
