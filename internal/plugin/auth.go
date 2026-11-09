package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"opencode-go-cliproxyapi/internal/config"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// credentialRefreshInterval is how often credential health is re-checked.
// The plugin drives this cadence itself via startHealthCheckLoop (the host
// auto-refresh loop never schedules auth.refresh for API-key-kind
// credentials); the same value is reported to the host through
// refresh_interval_seconds and NextRefreshAfter.
const credentialRefreshInterval = 15 * time.Minute

// authProvider implements the pluginapi.AuthProvider contract.
type authProvider struct{ cfg config.Config }

var _ pluginapi.AuthProvider = authProvider{}

func (authProvider) Identifier() string { return ProviderID }

func (p authProvider) ParseAuth(_ context.Context, req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	debugTrace("auth parse request provider=%s file=%s raw_bytes=%d", req.Provider, req.FileName, len(req.RawJSON))
	var raw struct {
		Type     string `json:"type"`
		Provider string `json:"provider"`
		ID       string `json:"id"`
		Label    string `json:"label"`
		APIKey   string `json:"api_key"`
		Disabled bool   `json:"disabled"`
	}
	if err := json.Unmarshal(req.RawJSON, &raw); err != nil {
		if req.Provider == ProviderID {
			return pluginapi.AuthParseResponse{}, fmt.Errorf("opencode-go auth record has invalid JSON")
		}
		return pluginapi.AuthParseResponse{}, nil
	}
	if (req.Provider != "" && req.Provider != ProviderID) || (raw.Type != ProviderID && raw.Provider != ProviderID) {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	if strings.TrimSpace(raw.APIKey) == "" {
		return pluginapi.AuthParseResponse{}, fmt.Errorf("opencode-go auth record has no api key")
	}
	if raw.ID == "" {
		raw.ID = req.FileName
	}
	debugTrace("auth parse handled provider=%s file=%s id=%s api_key_present=%t api_key_length=%d", req.Provider, req.FileName, raw.ID, strings.TrimSpace(raw.APIKey) != "", len(raw.APIKey))
	return pluginapi.AuthParseResponse{Handled: true, Auth: pluginapi.AuthData{
		Provider: ProviderID, ID: raw.ID, FileName: req.FileName, Label: accountLabel(p.cfg, raw.APIKey, raw.Label, 0), StorageJSON: req.RawJSON, Disabled: raw.Disabled,
		Attributes: map[string]string{
			"api_key": raw.APIKey,
			// Reported so host-driven refreshes (error-driven retry, manual
			// refresh) honor the plugin's cadence. The auto-refresh scheduler
			// itself never schedules API-key-kind credentials — the plugin
			// runs its own health-check loop instead.
			"refresh_interval_seconds": strconv.Itoa(int(credentialRefreshInterval.Seconds())),
		},
		NextRefreshAfter: time.Now().Add(credentialRefreshInterval),
	}}, nil
}

func (authProvider) StartLogin(context.Context, pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("opencode-go login is unsupported; configure a manual api key")
}

func (authProvider) PollLogin(context.Context, pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	return pluginapi.AuthLoginPollResponse{}, fmt.Errorf("opencode-go login is unsupported; configure a manual api key")
}

func (authProvider) RefreshAuth(_ context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	debugTrace("auth refresh request provider=%s id=%s storage_json_bytes=%d attr_names=%v metadata_names=%v", req.AuthProvider, req.AuthID, len(req.StorageJSON), mapKeys(req.Attributes), mapKeys(req.Metadata))
	return pluginapi.AuthRefreshResponse{Auth: pluginapi.AuthData{Provider: req.AuthProvider, ID: req.AuthID, StorageJSON: req.StorageJSON, Metadata: req.Metadata, Attributes: req.Attributes}}, nil
}

func mapKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

// Labels are presentation only. Stable auth IDs and stored host metadata are preserved.
func accountLabel(cfg config.Config, key, existing string, fallbackIndex int) string {
	index := fallbackIndex
	for i, entry := range cfg.APIKeys {
		if entry.Value == key {
			if name := strings.TrimSpace(entry.Name); name != "" {
				return name
			}
			index = i
			break
		}
	}
	label := strings.TrimSpace(existing)
	if label != "" && !strings.HasPrefix(label, "OpenCode Go credential ") && !strings.HasPrefix(label, "opencode-go-key-") {
		return label
	}
	if len(cfg.APIKeys) > 1 {
		return fmt.Sprintf("OpenCode Go %d", index+1)
	}
	return "OpenCode Go"
}
