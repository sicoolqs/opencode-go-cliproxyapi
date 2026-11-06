package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requireErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err.Error(), want)
	}
}

// withKey satisfies the at-least-one-key rule (spec 04 §3) for fixtures
// that target a later validation check.
const withKey = "api-keys:\n  - value: sk-dummy\n"

// The Console gateway serves each protocol under its own prefix; a single
// base URL can no longer express them. Explicit per-protocol overrides must
// win independently, and an explicit base-url must keep the legacy layout.
func TestLoadEndpointsLegacyAndOverrides(t *testing.T) {
	legacy, err := Load([]byte("base-url: https://opencode.ai/zen/go/v1\n" + withKey))
	if err != nil {
		t.Fatalf("legacy load: %v", err)
	}
	if !legacy.LegacyBase {
		t.Error("LegacyBase = false, want true for an explicit base-url")
	}
	if legacy.CatalogURL != "https://opencode.ai/zen/go/v1/models" {
		t.Errorf("legacy CatalogURL = %q", legacy.CatalogURL)
	}
	if legacy.Endpoints.ChatCompletions != "https://opencode.ai/zen/go/v1/chat/completions" {
		t.Errorf("legacy chat = %q", legacy.Endpoints.ChatCompletions)
	}
	if legacy.Endpoints.Messages != "https://opencode.ai/zen/go/v1/messages" {
		t.Errorf("legacy messages = %q", legacy.Endpoints.Messages)
	}
	if legacy.Endpoints.Usage != "https://opencode.ai/zen/go/v1/usage" {
		t.Errorf("legacy usage = %q", legacy.Endpoints.Usage)
	}

	overridden, err := Load([]byte(withKey + `
chat-completions-url: https://gw.test/openai/v1/chat/completions/
messages-url: https://gw.test/anthropic/v1/messages
responses-url: https://gw.test/openai/v1/responses
usage-url: https://gw.test/v1/usage
`))
	if err != nil {
		t.Fatalf("override load: %v", err)
	}
	if overridden.Endpoints.ChatCompletions != "https://gw.test/openai/v1/chat/completions" {
		t.Errorf("override chat = %q (trailing slash must be trimmed)", overridden.Endpoints.ChatCompletions)
	}
	if overridden.Endpoints.Messages != "https://gw.test/anthropic/v1/messages" {
		t.Errorf("override messages = %q", overridden.Endpoints.Messages)
	}
	if overridden.Endpoints.Responses != "https://gw.test/openai/v1/responses" {
		t.Errorf("override responses = %q", overridden.Endpoints.Responses)
	}
	if overridden.Endpoints.Usage != "https://gw.test/v1/usage" {
		t.Errorf("override usage = %q", overridden.Endpoints.Usage)
	}
	// Untouched endpoints keep the Console defaults.
	if overridden.Endpoints.Google != "https://opencode.ai/inference/go/google/v1beta" {
		t.Errorf("google default lost: %q", overridden.Endpoints.Google)
	}
}

func TestLoadRejectsInvalidEndpointURLs(t *testing.T) {
	for _, doc := range []string{
		"messages-url: http://gw.test/v1/messages\n" + withKey,
		"responses-url: https://gw.test/v1/responses?key=1\n" + withKey,
		"google-url: ftp://gw.test/v1beta\n" + withKey,
		"usage-url: https://gw.test/v1/usage#frag\n" + withKey,
	} {
		if _, err := Load([]byte(doc)); err == nil {
			t.Errorf("Load accepted invalid endpoint config: %q", doc)
		}
	}
}

// Routing defaults to native so upgrades keep the pre-change behavior; an
// unknown mode or an unsatisfiable anthropic deployment is rejected instead
// of silently defaulting.
func TestLoadRoutingDefaultsAndValidation(t *testing.T) {
	c, err := Load([]byte(withKey))
	if err != nil {
		t.Fatalf("Load(minimal): %v", err)
	}
	if c.Routing.Mode != RoutingNative {
		t.Errorf("default mode = %q, want %q", c.Routing.Mode, RoutingNative)
	}
	if !c.Routing.AllowTranslation {
		t.Error("default allow-translation = false, want true")
	}

	prefer, err := Load([]byte(withKey + "routing:\n  mode: prefer-client-protocol\n  allow-translation: false\n"))
	if err != nil {
		t.Fatalf("Load(prefer-client): %v", err)
	}
	if prefer.Routing.Mode != RoutingPreferClient || prefer.Routing.AllowTranslation {
		t.Errorf("routing = %+v", prefer.Routing)
	}

	if _, err := Load([]byte(withKey + "routing:\n  mode: bogus\n")); err == nil {
		t.Error("unknown routing mode was accepted")
	}
	if _, err := Load([]byte(withKey + "routing:\n  mode: anthropic\nprotocols:\n  messages: false\n")); err == nil {
		t.Error("anthropic mode with messages disabled was accepted")
	}
}

// api-keys-file entries merge with inline keys; duplicates keep the inline
// name and the file is re-read on the configured cadence.
func TestLoadAPIKeysFileMergesAndDedupes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.yaml")
	content := "api-keys:\n  - value: sk-file\n    name: File\n  - value: sk-dup\n    name: FileDup\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := "api-keys:\n  - value: sk-base\n    name: Base\n  - value: sk-dup\n    name: BaseDup\n" +
		"api-keys-file: " + path + "\n" +
		"api-keys-file-poll: 30s\n"
	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(c.APIKeys) != 3 {
		t.Fatalf("keys = %+v, want 3 merged entries", c.APIKeys)
	}
	if c.APIKeys[0].Value != "sk-base" || c.APIKeys[1].Value != "sk-dup" || c.APIKeys[2].Value != "sk-file" {
		t.Errorf("merge order = %+v", c.APIKeys)
	}
	if c.APIKeys[1].Name != "BaseDup" {
		t.Errorf("inline name must win: %+v", c.APIKeys[1])
	}
	if len(c.ConfigAPIKeys) != 2 {
		t.Errorf("ConfigAPIKeys = %+v, want the two inline keys", c.ConfigAPIKeys)
	}
	if c.APIKeysFile != path || c.APIKeysFilePoll != 30*time.Second {
		t.Errorf("file settings = %q %v", c.APIKeysFile, c.APIKeysFilePoll)
	}
}

func TestLoadAPIKeysFileErrors(t *testing.T) {
	if _, err := Load([]byte("api-keys-file: /nonexistent/keys.yaml\n" + withKey)); err == nil {
		t.Error("missing api-keys-file was accepted")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.yaml")
	if err := os.WriteFile(path, []byte("api-keys:\n  - value: sk-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load([]byte("api-keys-file: " + path + "\napi-keys-file-poll: 5s\n")); err == nil {
		t.Error("too-short api-keys-file-poll was accepted")
	}
}

func TestLoadMinimalAppliesAllDefaults(t *testing.T) {
	c, err := Load([]byte(withKey))
	if err != nil {
		t.Fatalf("Load(minimal) failed: %v", err)
	}
	if c.BaseURL != "https://opencode.ai/inference/go" {
		t.Errorf("BaseURL = %q", c.BaseURL)
	}
	if c.LegacyBase {
		t.Errorf("LegacyBase = true, want false for the Console defaults")
	}
	if c.CatalogURL != "https://opencode.ai/inference/go/v1/models" {
		t.Errorf("CatalogURL = %q", c.CatalogURL)
	}
	if c.Endpoints.ChatCompletions != "https://opencode.ai/inference/go/openai/v1/chat/completions" {
		t.Errorf("ChatCompletions = %q", c.Endpoints.ChatCompletions)
	}
	if c.Endpoints.Messages != "https://opencode.ai/inference/go/anthropic/v1/messages" {
		t.Errorf("Messages = %q", c.Endpoints.Messages)
	}
	if c.Endpoints.Responses != "https://opencode.ai/inference/go/openai/v1/responses" {
		t.Errorf("Responses = %q", c.Endpoints.Responses)
	}
	if c.Endpoints.Google != "https://opencode.ai/inference/go/google/v1beta" {
		t.Errorf("Google = %q", c.Endpoints.Google)
	}
	if c.Endpoints.Usage != "https://opencode.ai/zen/go/v1/usage" {
		t.Errorf("Usage = %q", c.Endpoints.Usage)
	}
	if !c.ModelPrefix.Enabled || c.ModelPrefix.Value != "opencode-go" {
		t.Errorf("ModelPrefix = %+v", c.ModelPrefix)
	}
	if len(c.APIKeys) != 1 || c.APIKeys[0].Value != "sk-dummy" {
		t.Errorf("APIKeys = %+v", c.APIKeys)
	}
	if c.Catalog.RefreshInterval != 15*time.Minute {
		t.Errorf("RefreshInterval = %v", c.Catalog.RefreshInterval)
	}
	if !c.Catalog.StaleWhileUnavailable {
		t.Errorf("StaleWhileUnavailable = false")
	}
	if !c.Protocols.ChatCompletions || !c.Protocols.Messages || !c.Protocols.Responses || !c.Protocols.Google {
		t.Errorf("Protocols = %+v", c.Protocols)
	}
	if c.AllowHTTP {
		t.Errorf("AllowHTTP = true")
	}
	if c.RequestTimeout != 5*time.Minute {
		t.Errorf("RequestTimeout = %v", c.RequestTimeout)
	}
	if c.MaxResponseBytes != 67108864 {
		t.Errorf("MaxResponseBytes = %d", c.MaxResponseBytes)
	}
}

func TestLoadFullConfigHonorsEveryField(t *testing.T) {
	t.Setenv("TEST_OG_KEY", "test-secret-value")
	doc := `
enabled: true
future-key: 1
base-url: http://localhost:9090/v1
catalog-url: https://example.com/models
model-prefix:
  enabled: true
  value: myprefix
api-keys:
  - value: ${TEST_OG_KEY}
catalog:
  refresh-interval: 1h
  stale-while-unavailable: false
protocols:
  chat-completions: false
  messages: false
  responses: false
route-overrides:
  gpt-5.6-luna:
    protocol: responses
    endpoint: /v1/responses
allow-http: true
request-timeout: 5m
max-response-bytes: 1024
`
	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("Load(full) failed: %v", err)
	}
	if c.BaseURL != "http://localhost:9090/v1" {
		t.Errorf("BaseURL = %q", c.BaseURL)
	}
	if c.CatalogURL != "https://example.com/models" {
		t.Errorf("CatalogURL = %q", c.CatalogURL)
	}
	if !c.ModelPrefix.Enabled || c.ModelPrefix.Value != "myprefix" {
		t.Errorf("ModelPrefix = %+v", c.ModelPrefix)
	}
	if len(c.APIKeys) != 1 || c.APIKeys[0].Value != "test-secret-value" {
		t.Errorf("APIKeys = %+v", c.APIKeys)
	}
	if c.Catalog.RefreshInterval != time.Hour {
		t.Errorf("RefreshInterval = %v", c.Catalog.RefreshInterval)
	}
	if c.Catalog.StaleWhileUnavailable {
		t.Errorf("StaleWhileUnavailable = true")
	}
	if c.Protocols.ChatCompletions || c.Protocols.Messages || c.Protocols.Responses {
		t.Errorf("Protocols = %+v", c.Protocols)
	}
	o, ok := c.RouteOverrides["gpt-5.6-luna"]
	if !ok || o.Protocol != "responses" || o.Endpoint != "/v1/responses" {
		t.Errorf("RouteOverrides = %+v", c.RouteOverrides)
	}
	if !c.AllowHTTP {
		t.Errorf("AllowHTTP = false")
	}
	if c.RequestTimeout != 5*time.Minute {
		t.Errorf("RequestTimeout = %v", c.RequestTimeout)
	}
	if c.MaxResponseBytes != 1024 {
		t.Errorf("MaxResponseBytes = %d", c.MaxResponseBytes)
	}
}

func TestLoadEnvExpansion(t *testing.T) {
	t.Setenv("TEST_OG_KEY", "expanded-secret")
	c, err := Load([]byte("api-keys:\n  - value: ${TEST_OG_KEY}\n"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(c.APIKeys) != 1 || c.APIKeys[0].Value != "expanded-secret" {
		t.Fatalf("APIKeys = %+v", c.APIKeys)
	}
}

func TestLoadIgnoresRemovedSchedulerFields(t *testing.T) {
	doc := "routing:\n  strategy: round-robin\n  max-retries-per-request: 9\n  transient-retry-interval: 1s\n  fallback-cooldown: 1m\n" +
		"api-keys:\n  - value: sk-dummy\n    priority: 7\n"
	if _, err := Load([]byte(doc)); err != nil {
		t.Fatalf("removed scheduler fields must remain unknown and ignored: %v", err)
	}
}

func TestLoadDerivedCatalogURLFromCustomBase(t *testing.T) {
	c, err := Load([]byte("base-url: https://upstream.example.com/api\n" + withKey))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if c.CatalogURL != "https://upstream.example.com/api/models" {
		t.Errorf("CatalogURL = %q", c.CatalogURL)
	}
}

func TestLoadDerivedCatalogURLTrimsTrailingSlash(t *testing.T) {
	c, err := Load([]byte("base-url: https://opencode.ai/zen/go/v1/\n" + withKey))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if c.CatalogURL != "https://opencode.ai/zen/go/v1/models" {
		t.Errorf("CatalogURL = %q, want no double slash", c.CatalogURL)
	}
}

// An EXPLICIT catalog-url ending in "/" must normalize the same way as the
// derived default, so the stored endpoint never carries a trailing slash.
func TestLoadExplicitCatalogURLTrimsTrailingSlash(t *testing.T) {
	c, err := Load([]byte("catalog-url: https://mirror.example.com/api/models///\n" + withKey))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if c.CatalogURL != "https://mirror.example.com/api/models" {
		t.Errorf("CatalogURL = %q, want trailing slashes trimmed", c.CatalogURL)
	}
}

func TestPrefixDisabledSkipsValueValidation(t *testing.T) {
	doc := `
model-prefix:
  enabled: false
  value: "has space"
` + withKey
	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := PublicID(c, "glm-5.2"); got != "glm-5.2" {
		t.Errorf("PublicID = %q", got)
	}
}

func TestPublicID(t *testing.T) {
	enabled := Config{ModelPrefix: ModelPrefix{Enabled: true, Value: "opencode-go"}}
	if got := PublicID(enabled, "glm-5.2"); got != "opencode-go/glm-5.2" {
		t.Errorf("PublicID(enabled) = %q", got)
	}
	disabled := Config{ModelPrefix: ModelPrefix{Enabled: false}}
	if got := PublicID(disabled, "glm-5.2"); got != "glm-5.2" {
		t.Errorf("PublicID(disabled) = %q", got)
	}
}

func TestLoadRejections(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"invalid yaml syntax", "[unclosed", "decode config"},
		{"unknown anchor has no line info", "request-timeout: *nope\n", "decode config: invalid YAML structure"},
		{"bare-scalar api key never leaks", "api-keys:\n  - sk-live-secret-123\n", "decode config: invalid YAML structure"},
		{"keyless config rejected", "", "api-keys: at least one key is required"},
		{"request-timeout zero rejected", "request-timeout: 0s\n" + withKey, "request-timeout: must be positive"},
		{
			"duplicate route-override keys",
			"route-overrides:\n" +
				"  gpt-5.6-luna:\n    protocol: responses\n    endpoint: /v1/responses\n" +
				"  gpt-5.6-luna:\n    protocol: messages\n    endpoint: /v1/messages\n",
			"decode config: invalid YAML structure near line",
		},
		{
			"key expands to empty",
			"api-keys:\n  - value: ${TEST_OG_DEFINITELY_UNSET_VAR}\n",
			"api-keys[0].value: expanded to empty",
		},
		{
			"second key expands to empty",
			"api-keys:\n  - value: literal-ok\n  - value: ${TEST_OG_DEFINITELY_UNSET_VAR}\n",
			"api-keys[1].value: expanded to empty",
		},
		{"explicitly empty base-url", `base-url: ""`, "base-url: must not be empty"},
		{"explicitly empty catalog-url", `catalog-url: ""`, "catalog-url: must not be empty"},
		{"malformed base-url", `base-url: "::::"`, "base-url: invalid URL"},
		{"scheme-less base-url", "base-url: opencode.ai/zen/go/v1", "base-url: invalid URL"},
		{"malformed catalog-url", `catalog-url: "http://exa mple.com"`, "catalog-url: invalid URL"},
		{"http base-url without allow-http", "base-url: http://localhost:9090/v1", "base-url: http scheme requires allow-http"},
		{"http catalog-url without allow-http", "catalog-url: http://localhost:9090/models", "catalog-url: http scheme requires allow-http"},
		{"ftp base-url", "base-url: ftp://files.example.com/v1", `base-url: unsupported scheme "ftp"`},
		{"file catalog-url", "catalog-url: file://host/share/models", `catalog-url: unsupported scheme "file"`},
		{"ftp base-url with allow-http", "allow-http: true\nbase-url: ftp://files.example.com/v1", `base-url: unsupported scheme "ftp"`},
		{"file catalog-url with allow-http", "allow-http: true\ncatalog-url: file://host/share/models", `catalog-url: unsupported scheme "file"`},
		{"refresh-interval missing unit", "catalog:\n  refresh-interval: 5\n", "catalog.refresh-interval: invalid duration"},
		{"refresh-interval negative", "catalog:\n  refresh-interval: -5m\n" + withKey, "catalog.refresh-interval: must be at least 1m"},
		{"refresh-interval below floor", "catalog:\n  refresh-interval: 1ms\n" + withKey, "catalog.refresh-interval: must be at least 1m"},
		{"request-timeout unparsable", "request-timeout: bogus\n", "request-timeout: invalid duration"},
		{"request-timeout explicitly empty", `request-timeout: ""`, "request-timeout: invalid duration"},
		{"request-timeout zero rejected", "request-timeout: 0s\n" + withKey, "request-timeout: must be positive"},
		{"userinfo credentials never echoed", "base-url: https://user:sekret@/v1", "base-url: invalid URL https://"},
		{"max-response-bytes zero rejected", "max-response-bytes: 0\n" + withKey, "max-response-bytes: must be positive"},
		{"max-response-bytes negative rejected", "max-response-bytes: -5\n" + withKey, "max-response-bytes: must be positive"},
		{
			"override unsupported protocol",
			"route-overrides:\n  bad-model:\n    protocol: grpc\n    endpoint: /v1/x\n" + withKey,
			"route-overrides[bad-model].protocol: unsupported protocol",
		},
		{
			"override empty endpoint",
			"route-overrides:\n  bad-model:\n    protocol: responses\n    endpoint: \"\"\n" + withKey,
			"route-overrides[bad-model].endpoint: must not be empty",
		},
		{
			"override endpoint without leading slash",
			"route-overrides:\n  bad-model:\n    protocol: responses\n    endpoint: v1/x\n" + withKey,
			"route-overrides[bad-model].endpoint: must start with /",
		},
		{"prefix enabled empty value", "model-prefix:\n  enabled: true\n  value: \"\"\n" + withKey, "model-prefix.value: invalid provider-ID characters"},
		{"prefix with space", "model-prefix:\n  value: has space\n" + withKey, "model-prefix.value: invalid provider-ID characters"},
		{"prefix leading dash", "model-prefix:\n  value: -lead\n" + withKey, "model-prefix.value: invalid provider-ID characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load([]byte(tt.yaml))
			requireErrContains(t, err, tt.wantErr)
			if tt.name == "key expands to empty" && strings.Contains(err.Error(), "${") {
				t.Errorf("error leaks environment reference: %q", err.Error())
			}
		})
	}
}

// TestLoadErrorsNeverLeakDecodedValues pins the security property: decode
// and validation failures must not echo raw scalars (a bare-scalar API key)
// or embedded userinfo credentials into the error the host logs.
func TestLoadErrorsNeverLeakDecodedValues(t *testing.T) {
	cases := []struct {
		name   string
		yaml   string
		secret string
	}{
		{"bare-scalar api key", "api-keys:\n  - sk-live-supersecret-42\n", "sk-live-supersecret-42"},
		{"duplicate api key values", "api-keys:\n  - value: sk-live-dupsecret-7\n  - value: sk-live-dupsecret-7\n", "sk-live-dupsecret-7"},
		{"userinfo credentials", "base-url: https://admin:p4ssw0rd@/v1\n", "p4ssw0rd"},
	}
	for _, tc := range cases {
		_, err := Load([]byte(tc.yaml))
		if err == nil {
			t.Fatalf("%s: expected rejection", tc.name)
		}
		if strings.Contains(err.Error(), tc.secret) {
			t.Fatalf("%s: error leaks secret %q: %v", tc.name, tc.secret, err)
		}
	}
}

// TestValidateURLRejectsQueryFragmentUserinfo pins the fix for bases that
// parse fine but break JoinUpstreamURL: a query or fragment truncates the
// joined path (…/v1?tenant=x/chat/completions), and userinfo leaks
// credentials into every upstream request URL.
func TestValidateURLRejectsQueryFragmentUserinfo(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		httpOK  bool
		wantErr string
	}{
		{"query rejected naming setting", "https://host/v1?tenant=x", false, "base-url: must not contain query"},
		{"fragment rejected", "https://host/v1#frag", false, "must not contain query, fragment, or userinfo"},
		{"userinfo rejected without echoing credentials", "https://user:key@host/v1", false, "must not contain query, fragment, or userinfo"},
		{"clean https passes", "https://host/v1", false, ""},
		{"http with allow-http passes", "http://localhost:9090/v1", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateURL("base-url", tt.raw, tt.httpOK)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateURL(%q) = %v, want nil", tt.raw, err)
				}
				return
			}
			requireErrContains(t, err, tt.wantErr)
			if strings.Contains(err.Error(), "key") && strings.Contains(tt.raw, ":key@") {
				t.Errorf("error echoes credentials: %q", err.Error())
			}
		})
	}
}
