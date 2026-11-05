// Package config loads and validates the opencode-go plugin configuration
// per spec 04 (configuration) and spec 05 §2 (HTTPS/timeouts).
package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults (spec 04 §2/§4). The Go plan is served from the Console
// inference gateway; the legacy /zen/go/v1 base URL remains available
// through an explicit base-url setting.
const (
	DefaultAPIBase            = "https://opencode.ai/inference/go"
	DefaultCatalogURL         = "https://opencode.ai/inference/go/v1/models"
	DefaultChatCompletionsURL = "https://opencode.ai/inference/go/openai/v1/chat/completions"
	DefaultResponsesURL       = "https://opencode.ai/inference/go/openai/v1/responses"
	DefaultMessagesURL        = "https://opencode.ai/inference/go/anthropic/v1/messages"
	DefaultGoogleBaseURL      = "https://opencode.ai/inference/go/google/v1beta"
	// DefaultUsageURL stays on the legacy host: the Console docs do not
	// document a usage endpoint, and both hosts answer /usage identically
	// (verified 401 unauthenticated). Per the migration rule the existing
	// verified endpoint is preserved rather than guessed from URL patterns.
	DefaultUsageURL         = "https://opencode.ai/zen/go/v1/usage"
	DefaultModelPrefix      = "opencode-go"
	DefaultRefreshInterval  = 15 * time.Minute
	DefaultRequestTimeout   = 5 * time.Minute
	DefaultMaxResponseBytes = int64(67108864) // 64 MiB
)

// Endpoints holds the per-protocol upstream URLs. Each protocol has its own
// full URL because the Console gateway serves them under different prefixes
// (/openai, /anthropic, /google); a single base URL can no longer express
// them. Usage is kept separate from the inference endpoints on purpose.
type Endpoints struct {
	ChatCompletions string
	Messages        string
	Responses       string
	// Google is the generateContent base; the model path is appended per
	// request (.../models/<model>:generateContent).
	Google string
	Usage  string
}

// ModelPrefix controls the client-facing model ID prefix.
type ModelPrefix struct {
	Enabled bool
	Value   string
}

type APIKey struct {
	Name  string
	Value string
}

type Catalog struct {
	RefreshInterval       time.Duration
	StaleWhileUnavailable bool
}

type Protocols struct {
	ChatCompletions bool
	Messages        bool
	Responses       bool
	// Google gates models whose native protocol is Gemini generateContent.
	// Such models are served through chat-completions (Console translates
	// every text model across APIs); disabling this drops them.
	Google bool
}

type RouteOverride struct {
	Protocol string `yaml:"protocol"`
	Endpoint string `yaml:"endpoint"`
}

type Config struct {
	// BaseURL is the join base used for route-override paths. It is the
	// legacy base-url when set, otherwise the Console API base.
	BaseURL string
	// LegacyBase reports that base-url was explicitly configured, which
	// selects the pre-Console /zen/go/v1 path layout.
	LegacyBase       bool
	CatalogURL       string
	Endpoints        Endpoints
	ModelPrefix      ModelPrefix
	APIKeys          []APIKey
	Catalog          Catalog
	Protocols        Protocols
	RouteOverrides   map[string]RouteOverride
	AllowHTTP        bool
	RequestTimeout   time.Duration
	MaxResponseBytes int64
}

// rawConfig mirrors the YAML shape; pointer fields distinguish "unset"
// (apply default) from explicitly-set values including "" (validate as-is).
// Unknown fields are ignored (host may pass extra keys).
type rawConfig struct {
	BaseURL            *string                  `yaml:"base-url"`
	CatalogURL         *string                  `yaml:"catalog-url"`
	ChatCompletionsURL *string                  `yaml:"chat-completions-url"`
	MessagesURL        *string                  `yaml:"messages-url"`
	ResponsesURL       *string                  `yaml:"responses-url"`
	GoogleURL          *string                  `yaml:"google-url"`
	UsageURL           *string                  `yaml:"usage-url"`
	ModelPrefix        rawPrefix                `yaml:"model-prefix"`
	APIKeys            []rawKey                 `yaml:"api-keys"`
	Catalog            rawCatalog               `yaml:"catalog"`
	Protocols          rawProtocols             `yaml:"protocols"`
	RouteOverrides     map[string]RouteOverride `yaml:"route-overrides"`
	AllowHTTP          bool                     `yaml:"allow-http"`
	RequestTimeout     *string                  `yaml:"request-timeout"`
	MaxResponseBytes   *int64                   `yaml:"max-response-bytes"`
}

type rawPrefix struct {
	Enabled *bool   `yaml:"enabled"`
	Value   *string `yaml:"value"`
}

type rawKey struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type rawCatalog struct {
	RefreshInterval       *string `yaml:"refresh-interval"`
	StaleWhileUnavailable *bool   `yaml:"stale-while-unavailable"`
}

type rawProtocols struct {
	ChatCompletions *bool `yaml:"chat-completions"`
	Messages        *bool `yaml:"messages"`
	Responses       *bool `yaml:"responses"`
	Google          *bool `yaml:"google"`
}

var (
	validProtocols = map[string]bool{"chat-completions": true, "messages": true, "responses": true}
)

// Load decodes YAML, expands ${VAR} references in api-key values only,
// applies defaults, and validates (spec 04 §6). Decode errors never echo
// decoded node values — a malformed entry (e.g. a bare-scalar API key)
// must not leak into the invalid_config envelope the host logs.
func Load(yamlBytes []byte) (Config, error) {
	var raw rawConfig
	if err := yaml.Unmarshal(yamlBytes, &raw); err != nil {
		if n := regexp.MustCompile(`line (\d+)`).FindStringSubmatch(err.Error()); n != nil {
			return Config{}, fmt.Errorf("decode config: invalid YAML structure near line %s", n[1])
		}
		return Config{}, fmt.Errorf("decode config: invalid YAML structure")
	}
	keys := make([]APIKey, len(raw.APIKeys))
	for i, k := range raw.APIKeys {
		keys[i] = APIKey{Value: os.ExpandEnv(k.Value), Name: strings.TrimSpace(k.Name)}
	}
	refreshInterval, err := parseDuration("catalog.refresh-interval", raw.Catalog.RefreshInterval, DefaultRefreshInterval)
	if err != nil {
		return Config{}, err
	}
	requestTimeout, err := parseDuration("request-timeout", raw.RequestTimeout, DefaultRequestTimeout)
	if err != nil {
		return Config{}, err
	}
	if requestTimeout <= 0 {
		return Config{}, fmt.Errorf("request-timeout: must be positive")
	}
	c := Config{
		BaseURL:    strings.TrimRight(orDefault(raw.BaseURL, DefaultAPIBase), "/"),
		LegacyBase: raw.BaseURL != nil,
		ModelPrefix: ModelPrefix{
			Enabled: orDefault(raw.ModelPrefix.Enabled, true),
			Value:   orDefault(raw.ModelPrefix.Value, DefaultModelPrefix),
		},
		APIKeys: keys,
		Catalog: Catalog{
			RefreshInterval:       refreshInterval,
			StaleWhileUnavailable: orDefault(raw.Catalog.StaleWhileUnavailable, true),
		},
		Protocols: Protocols{
			ChatCompletions: orDefault(raw.Protocols.ChatCompletions, true),
			Messages:        orDefault(raw.Protocols.Messages, true),
			Responses:       orDefault(raw.Protocols.Responses, true),
			Google:          orDefault(raw.Protocols.Google, true),
		},
		RouteOverrides:   raw.RouteOverrides,
		AllowHTTP:        raw.AllowHTTP,
		RequestTimeout:   requestTimeout,
		MaxResponseBytes: orDefault(raw.MaxResponseBytes, DefaultMaxResponseBytes),
	}
	// Endpoint layout: an explicit base-url selects the legacy
	// single-base layout (/zen/go/v1 style) for backward compatibility;
	// otherwise the Console gateway's per-protocol URLs apply.
	if c.LegacyBase {
		c.CatalogURL = joinLegacy(c.BaseURL, "/v1/models")
		c.Endpoints = Endpoints{
			ChatCompletions: joinLegacy(c.BaseURL, "/v1/chat/completions"),
			Messages:        joinLegacy(c.BaseURL, "/v1/messages"),
			Responses:       joinLegacy(c.BaseURL, "/v1/responses"),
			Usage:           c.BaseURL + "/usage",
		}
	} else {
		c.CatalogURL = DefaultCatalogURL
		c.Endpoints = Endpoints{
			ChatCompletions: DefaultChatCompletionsURL,
			Messages:        DefaultMessagesURL,
			Responses:       DefaultResponsesURL,
			Google:          DefaultGoogleBaseURL,
			Usage:           DefaultUsageURL,
		}
	}
	trimOverride(&c.CatalogURL, raw.CatalogURL)
	trimOverride(&c.Endpoints.ChatCompletions, raw.ChatCompletionsURL)
	trimOverride(&c.Endpoints.Messages, raw.MessagesURL)
	trimOverride(&c.Endpoints.Responses, raw.ResponsesURL)
	trimOverride(&c.Endpoints.Google, raw.GoogleURL)
	trimOverride(&c.Endpoints.Usage, raw.UsageURL)
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// joinLegacy reproduces the pre-Console join: the base already carries the
// version segment, so a leading /v1 on the endpoint path is dropped instead
// of doubled.
func joinLegacy(base, path string) string {
	return strings.TrimSuffix(base, "/") + strings.TrimPrefix(path, "/v1")
}

// trimOverride applies an explicit URL override, trimming trailing slashes
// so downstream joins cannot double separators.
func trimOverride(dst *string, p *string) {
	if p != nil {
		*dst = strings.TrimRight(*p, "/")
	}
}

// PublicID returns the client-facing model ID (FR-003).
func PublicID(c Config, upstreamID string) string {
	if c.ModelPrefix.Enabled {
		return c.ModelPrefix.Value + "/" + upstreamID
	}
	return upstreamID
}

func (c Config) validate() error {
	if err := validateURL("base-url", c.BaseURL, c.AllowHTTP); err != nil {
		return err
	}
	if err := validateURL("catalog-url", c.CatalogURL, c.AllowHTTP); err != nil {
		return err
	}
	if err := validateURL("chat-completions-url", c.Endpoints.ChatCompletions, c.AllowHTTP); err != nil {
		return err
	}
	if err := validateURL("messages-url", c.Endpoints.Messages, c.AllowHTTP); err != nil {
		return err
	}
	if err := validateURL("responses-url", c.Endpoints.Responses, c.AllowHTTP); err != nil {
		return err
	}
	// google-url is optional: the legacy layout has no Google endpoint, and
	// Google-native models are served through chat-completions.
	if c.Endpoints.Google != "" {
		if err := validateURL("google-url", c.Endpoints.Google, c.AllowHTTP); err != nil {
			return err
		}
	}
	if err := validateURL("usage-url", c.Endpoints.Usage, c.AllowHTTP); err != nil {
		return err
	}
	if len(c.APIKeys) == 0 {
		return fmt.Errorf("api-keys: at least one key is required")
	}
	for i, k := range c.APIKeys {
		if k.Value == "" {
			return fmt.Errorf("api-keys[%d].value: expanded to empty", i)
		}
	}
	seen := make(map[string]bool, len(c.APIKeys))
	for _, k := range c.APIKeys {
		if seen[k.Value] {
			return fmt.Errorf("api-keys: duplicate key values are not allowed")
		}
		seen[k.Value] = true
	}
	if c.Catalog.RefreshInterval < time.Minute {
		return fmt.Errorf("catalog.refresh-interval: must be at least 1m")
	}
	if c.MaxResponseBytes <= 0 {
		return fmt.Errorf("max-response-bytes: must be positive")
	}
	for name, o := range c.RouteOverrides {
		if !validProtocols[o.Protocol] {
			return fmt.Errorf("route-overrides[%s].protocol: unsupported protocol %q", name, o.Protocol)
		}
		if o.Endpoint == "" {
			return fmt.Errorf("route-overrides[%s].endpoint: must not be empty", name)
		}
		if !strings.HasPrefix(o.Endpoint, "/") {
			return fmt.Errorf("route-overrides[%s].endpoint: must start with /", name)
		}
	}
	if c.ModelPrefix.Enabled && !validPrefix(c.ModelPrefix.Value) {
		return fmt.Errorf("model-prefix.value: invalid provider-ID characters %q", c.ModelPrefix.Value)
	}
	return nil
}

func validateURL(name, raw string, allowHTTP bool) error {
	if raw == "" {
		return fmt.Errorf("%s: must not be empty", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: invalid URL", name)
	}
	if u.Scheme == "" || u.Host == "" {
		// Echo only scheme://host — the configured string may embed
		// userinfo credentials (https://user:key@host).
		return fmt.Errorf("%s: invalid URL %s://%s", name, u.Scheme, u.Host)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("%s: must not contain query, fragment, or userinfo", name)
	}
	// Scheme allowlist (spec 05 §2): https always; http only behind
	// allow-http; anything else (ftp://, file://, custom schemes) is
	// rejected at load instead of failing at transport time.
	switch u.Scheme {
	case "https":
	case "http":
		if !allowHTTP {
			return fmt.Errorf("%s: http scheme requires allow-http", name)
		}
	default:
		return fmt.Errorf("%s: unsupported scheme %q; use https", name, u.Scheme)
	}
	return nil
}

func validPrefix(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if i == 0 && !alnum {
			return false
		}
		if !alnum && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func parseDuration(name string, p *string, def time.Duration) (time.Duration, error) {
	if p == nil {
		return def, nil
	}
	d, err := time.ParseDuration(*p)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration", name)
	}
	return d, nil
}

func orDefault[T any](p *T, def T) T {
	if p != nil {
		return *p
	}
	return def
}
