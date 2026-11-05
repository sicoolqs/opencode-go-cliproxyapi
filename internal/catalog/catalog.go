// Package catalog fetches the live OpenCode Go /v1/models catalog,
// normalizes metadata, assigns routes, and serves the last good snapshot
// (FR-002, FR-003, FR-004, FR-010).
//
// Route resolution priority follows arch spec §5: user route override >
// built-in compatibility table > unsupported (excluded from the routable set,
// kept in diagnostics).
//
// No scheduling lives here; M3 wires the refresh cadence.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/config"
)

// catalogBudgetFloor keeps an undersized max-response-bytes knob from
// freezing stale serving forever: catalogs grow larger than typical
// completion bodies, so the fetch budget never drops below 16 MiB while
// max-response-bytes stays meaningful for completions elsewhere.
const catalogBudgetFloor = 16 << 20

// Route is the upstream protocol a model is served through (FR-004).
type Route string

const (
	RouteChatCompletions Route = "chat-completions"
	RouteMessages        Route = "messages"
	RouteResponses       Route = "responses"
	// RouteGoogle is the Gemini generateContent protocol. Models whose
	// catalog entry declares it are served through chat-completions (the
	// Console gateway translates every text model across APIs) because this
	// plugin has no native Gemini adapter; the fallback is reported as a
	// catalog diagnostic rather than dropped silently.
	RouteGoogle Route = "google"
)

// EndpointPath returns the upstream endpoint path for the route (FR-004).
// routeEndpoints maps each route to its upstream endpoint path.
var routeEndpoints = map[Route]string{
	RouteChatCompletions: "/v1/chat/completions",
	RouteMessages:        "/v1/messages",
	RouteResponses:       "/v1/responses",
	RouteGoogle:          "/v1beta/models/{model}:generateContent",
}

// EndpointPath returns the route's upstream endpoint path, or "" for
// unknown routes.
func (r Route) EndpointPath() string {
	return routeEndpoints[r]
}

func routeFromString(s string) (Route, bool) {
	if _, ok := routeEndpoints[Route(s)]; ok {
		return Route(s), true
	}
	return "", false
}

// ModelRecord is one routable normalized catalog entry (arch §7, FR-003).
type ModelRecord struct {
	PublicID    string
	UpstreamID  string
	DisplayName string
	// Protocol is the compatibility field: the protocol used when no
	// per-request routing applies (native when it has a transport).
	Protocol Route
	// NativeProtocol is the protocol declared by the catalog.
	NativeProtocol Route
	// SupportedProtocols lists the protocols the catalog declares for the
	// model (its api map), used for capability checks.
	SupportedProtocols []Route
	// FromConsoleCatalog reports that the entry declared an api map, which
	// carries the gateway's cross-API translation guarantee.
	FromConsoleCatalog bool
	EndpointPath       string
	// EndpointURL is the full per-protocol upstream URL from config. Empty
	// when the snapshot was built from a config without endpoints (tests and
	// legacy direct construction); callers then fall back to
	// JoinUpstreamURL(BaseURL, EndpointPath).
	EndpointURL  string
	ContextLimit int64
	OutputLimit  int64
	InputModes   []string
	OutputModes  []string
	Thinking     *pluginapi.ThinkingSupport
	// Raw preserves the upstream entry verbatim for diagnostics.
	Raw json.RawMessage
}

// UnsupportedModel is a discovered model excluded from the routable set,
// retained for diagnostics (FR-004, open questions §10).
type UnsupportedModel struct {
	UpstreamID string
	Reason     string
}

// HostClient issues HTTP requests through the host (pluginapi.HTTPDo
// semantics); M3's host callback satisfies it directly.
type HostClient interface {
	Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

// rawModel tolerantly decodes one catalog entry. It understands both the
// Console Go schema (name / limit / modalities / capabilities.reasoning /
// api) and the legacy zen-go schema (display_name / context_length /
// input_modes / thinking); unknown fields are ignored via json tags while
// Raw keeps the original bytes for diagnostics.
type rawModel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`

	Limit           *rawLimit `json:"limit"`
	ContextLength   int64     `json:"context_length"`
	MaxOutputTokens int64     `json:"max_output_tokens"`

	Modalities       *rawModalities `json:"modalities"`
	InputModes       []string       `json:"input_modes"`
	OutputModes      []string       `json:"output_modes"`
	InputModalities  []string       `json:"input_modalities"`
	OutputModalities []string       `json:"output_modalities"`

	Capabilities *rawCapabilities `json:"capabilities"`
	Thinking     *rawThinking     `json:"thinking"`

	// API maps a protocol name (chat-completions, messages, responses,
	// generate-content) to its native endpoint description. This is the
	// authoritative routing source for the Console catalog.
	API map[string]rawAPI `json:"api"`

	// Raw is the verbatim upstream entry (populated by Refresh).
	Raw json.RawMessage `json:"-"`
}

type rawLimit struct {
	Context int64 `json:"context"`
	Output  int64 `json:"output"`
}

type rawModalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

type rawCapabilities struct {
	Reasoning *rawReasoning `json:"reasoning"`
}

type rawReasoning struct {
	Supported bool     `json:"supported"`
	Effort    []string `json:"effort"`
}

type rawAPI struct {
	BaseURL string `json:"base_url"`
}

// rawThinking tolerantly decodes the optional legacy per-model thinking
// object; pluginapi.ThinkingSupport itself is untagged, so snake_case
// upstream keys are decoded here and mapped field-by-field (partial objects
// decode to a partial struct).
type rawThinking struct {
	Min            *int     `json:"min"`
	Max            *int     `json:"max"`
	ZeroAllowed    *bool    `json:"zero_allowed"`
	DynamicAllowed *bool    `json:"dynamic_allowed"`
	Levels         []string `json:"levels"`
}

// apiProtocolOrder fixes the preference when an entry advertises several
// native protocols (Console translates between all of them). Chat
// Completions comes first because it has the widest client compatibility.
var apiProtocolOrder = []struct {
	key   string
	route Route
}{
	{"chat-completions", RouteChatCompletions},
	{"messages", RouteMessages},
	{"responses", RouteResponses},
	{"generate-content", RouteGoogle},
}

// prefixRoutes covers unknown variants of known families;
// longest prefix wins (list is ordered longest-first).
var prefixRoutes = []struct {
	prefix string
	route  Route
}{
	{"muse-spark", RouteResponses},
	{"deepseek", RouteChatCompletions},
	{"minimax", RouteMessages},
	{"longcat", RouteChatCompletions},
	{"grok", RouteResponses},
	{"gpt", RouteResponses},
	{"kimi", RouteChatCompletions},
	{"qwen", RouteMessages},
	{"glm", RouteChatCompletions},
	{"mimo", RouteChatCompletions},
	{"hy", RouteChatCompletions},
}

// Manager owns the catalog snapshot. All accessors are safe for
// concurrent use alongside Refresh.
type Manager struct {
	cfg    config.Config
	client HostClient

	mu sync.Mutex
	// raw retains the decoded upstream entries behind the current snapshot
	// so SeedFrom can rebuild records against a NEW config (route overrides,
	// protocol flags, and prefix settings may all have changed since the
	// snapshot was built).
	raw    []rawModel
	models []ModelRecord
	// index maps both PublicID and UpstreamID to their record so ID
	// resolution is O(1); rebuilt atomically with models on every swap.
	index map[string]ModelRecord
	unsup []UnsupportedModel
	warns []string
	// diags is the structured form of warns+unsup. Dedup state lives on the
	// plugin Manager, which outlives per-reconfigure catalog managers.
	diags []Diagnostic
}

// New returns a Manager serving cfg through the host client.
func New(cfg config.Config, client HostClient) *Manager {
	return &Manager{cfg: cfg, client: client}
}

// SeedFrom republishes prev's last-good snapshot into m by REBUILDING every
// record from prev's raw upstream entries through the same
// resolution/validation path a refresh applies, but against m's OWN cfg:
// routes re-resolve (a removed override drops its model), the protocolEnabled
// gate re-applies, PublicIDs recompute under m's prefix, endpoints re-check
// against m's base-url, and collisions/diagnostics regenerate (arch §5).
//
// m keeps its OWN cfg: subsequent Refresh calls use m's base-url/catalog-url/
// protocols/prefix (F5: a failed-refresh reconfigure must adopt the new
// manager rather than keep ticking the old URL). Because seeding rebuilds
// instead of copying, a config change between snapshots takes effect at seed
// time — e.g. a protocol kill-switch flipped during a catalog outage demotes
// its routes immediately instead of serving stale routable records until the
// outage ends.
func (m *Manager) SeedFrom(prev *Manager) {
	prev.mu.Lock()
	raw := prev.raw
	prev.mu.Unlock()
	m.swap(raw)
}

// Refresh fetches and swaps the catalog snapshot. On failure it returns a
// classified, key-free error; the previous snapshot keeps serving while
// catalog.stale-while-unavailable is enabled, otherwise the routable set
// is cleared until the next success (FR-002).
func (m *Manager) Refresh(ctx context.Context, apiKey string) error {
	if m.client == nil {
		// Only direct construction can produce a nil host client
		// (production always wires the bridge); fail loudly through the
		// classified path instead of panicking inside Do.
		return m.fail("host client unavailable")
	}
	req := pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    m.cfg.CatalogURL,
		Headers: http.Header{
			"Authorization": []string{"Bearer " + apiKey},
			"Accept":        []string{"application/json"},
		},
	}
	resp, err := m.client.Do(ctx, req)
	if err != nil {
		return m.fail("network error")
	}
	budget := max(m.cfg.MaxResponseBytes, catalogBudgetFloor)
	if int64(len(resp.Body)) > budget {
		return m.fail("response exceeds max-response-bytes")
	}
	if resp.StatusCode != http.StatusOK {
		return m.fail(fmt.Sprintf("http %d", resp.StatusCode))
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return m.fail("invalid json")
	}
	var rawEntries []json.RawMessage
	var extra []Diagnostic
	if env.Data == nil {
		// Key ABSENCE is upstream shape drift, never an intended clear:
		// clear like an empty plan (swap(nil)) but leave a diagnostic.
		// A present-but-empty array stays silent (tested-intended state).
		extra = append(extra, shapeDiagnostic(
			`catalog: upstream catalog response missing "data" field; impact: the routable catalog was cleared until the next successful refresh`,
			"catalog-cleared"))
	} else if err := json.Unmarshal(env.Data, &rawEntries); err != nil {
		return m.fail("invalid json")
	}
	entries := make([]rawModel, 0, len(rawEntries))
	for _, raw := range rawEntries {
		var entry rawModel
		if err := json.Unmarshal(raw, &entry); err != nil {
			// One malformed entry must not blank the whole catalog: keep the
			// snapshot and report it, mirroring the per-entry diagnostics.
			extra = append(extra, shapeDiagnostic(
				"catalog: one catalog entry is not a JSON object and was skipped; impact: that entry is not published",
				"entry-not-published"))
			continue
		}
		entry.Raw = append(json.RawMessage(nil), raw...)
		entries = append(entries, entry)
	}
	m.swap(entries, extra...)
	return nil
}

// fail applies the stale policy and returns the classified error.
func (m *Manager) fail(category string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.cfg.Catalog.StaleWhileUnavailable {
		// Clear the index too — Lookup must stop resolving IDs whose
		// records are gone (FR-002). Raw entries go with them: a cleared
		// snapshot has nothing to seed.
		m.raw, m.models, m.index, m.unsup, m.warns, m.diags = nil, nil, nil, nil, nil, nil
	}
	return fmt.Errorf("catalog refresh failed: %s", category)
}

// swap builds the new snapshot from decoded entries and installs it
// atomically under the mutex (FR-010 dedup, arch §5 route priority).
// extraWarns are caller-supplied snapshot diagnostics (e.g. decode-level
// shape-drift notices) recorded alongside the per-entry ones.
func (m *Manager) swap(entries []rawModel, extra ...Diagnostic) {
	models := make([]ModelRecord, 0, len(entries))
	index := make(map[string]ModelRecord, len(entries)*2)
	diags := append([]Diagnostic(nil), extra...)
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.ID == "" {
			diags = append(diags, missingIDDiagnostic())
			continue
		}
		if seen[e.ID] {
			diags = append(diags, duplicateDiagnostic(e.ID))
			continue
		}
		seen[e.ID] = true
		route, endpoint, ok, diag := m.resolve(e)
		if diag != nil && diag.Kind != DiagUnroutable {
			diags = append(diags, *diag)
		}
		if !ok {
			if diag != nil {
				diags = append(diags, *diag)
			} else {
				diags = append(diags, unroutableDiagnostic(e.ID, "", "no route determined"))
			}
			continue
		}
		inputModes := firstNonEmpty(e.InputModes, e.InputModalities)
		outputModes := firstNonEmpty(e.OutputModes, e.OutputModalities)
		if e.Modalities != nil {
			inputModes = firstNonEmpty(e.Modalities.Input, inputModes)
			outputModes = firstNonEmpty(e.Modalities.Output, outputModes)
		}
		display := firstNonEmptyString(e.Name, e.DisplayName, e.ID)
		contextLimit, outputLimit := e.ContextLength, e.MaxOutputTokens
		if e.Limit != nil {
			if e.Limit.Context > 0 {
				contextLimit = e.Limit.Context
			}
			if e.Limit.Output > 0 {
				outputLimit = e.Limit.Output
			}
		}
		rec := ModelRecord{
			PublicID:           config.PublicID(m.cfg, e.ID),
			UpstreamID:         e.ID,
			DisplayName:        display,
			Protocol:           route, // compatibility field; see NativeProtocol
			NativeProtocol:     route,
			SupportedProtocols: supportedProtocolsFromAPI(e.API),
			FromConsoleCatalog: len(e.API) > 0,
			EndpointPath:       endpoint,
			ContextLimit:       contextLimit,
			OutputLimit:        outputLimit,
			InputModes:         inputModes,
			OutputModes:        outputModes,
			Thinking:           normalizeThinking(e.Thinking, e.Capabilities),
			Raw:                e.Raw,
		}
		// A model stays routable while at least one enabled protocol can serve
		// it. Protocol switches no longer hide models whose native protocol
		// happens to be disabled: a Console text model remains reachable
		// through the other APIs.
		defaultProto, routable := firstUsable(m.cfg, rec)
		if !routable {
			reason := fmt.Sprintf(
				"no usable upstream protocol (native %s; chat=%t messages=%t responses=%t)",
				rec.NativeProtocol, m.cfg.Protocols.ChatCompletions,
				m.cfg.Protocols.Messages, m.cfg.Protocols.Responses)
			if !protocolEnabled(m.cfg, rec.NativeProtocol) {
				reason = fmt.Sprintf("protocol %s disabled by config", rec.NativeProtocol)
			}
			diags = append(diags, unroutableDiagnostic(e.ID, rec.NativeProtocol, reason))
			continue
		}
		if !IsTextProtocol(rec.NativeProtocol) {
			// The catalog cannot know which text protocol a request will use;
			// the routing policy decides that per request.
			diags = append(diags, googleFallbackDiagnostic(e.ID))
		}
		rec.Protocol = defaultProto
		if endpoint == "" {
			endpoint = defaultProto.EndpointPath()
		}
		rec.EndpointPath = endpoint
		url, eErr := EndpointURL(m.cfg, rec, defaultProto)
		if eErr != nil {
			diags = append(diags, unroutableDiagnostic(e.ID, rec.NativeProtocol, eErr.Message))
			continue
		}
		rec.EndpointURL = url
		// With a prefix enabled, one record's PublicID can equal another
		// record's UpstreamID (upstream "foo" and "opencode-go/foo" both
		// claim index key "<prefix>/foo"); last-write-wins would silently
		// misroute. First in catalog order wins (same dedup rule as
		// duplicate IDs); the later record is excluded. Same-role clashes
		// are impossible — distinct upstream IDs and an injective public
		// mapping — so the slot owner always holds the key via its other
		// role, and a record's own public==upstream rewrite is not a clash.
		if other, ok := index[rec.PublicID]; ok {
			reason := fmt.Sprintf("public id %q collides with upstream id %q", rec.PublicID, other.UpstreamID)
			diags = append(diags, collisionDiagnostic(e.ID, reason))
			continue
		}
		if other, ok := index[rec.UpstreamID]; ok {
			reason := fmt.Sprintf("public id %q collides with upstream id %q", other.PublicID, rec.UpstreamID)
			diags = append(diags, collisionDiagnostic(e.ID, reason))
			continue
		}
		models = append(models, rec)
		index[rec.PublicID] = rec
		index[rec.UpstreamID] = rec
	}
	warns := make([]string, 0, len(diags))
	unsup := make([]UnsupportedModel, 0, len(diags))
	for _, d := range diags {
		warns = append(warns, d.Message)
		// Unsupported() is the compatibility view of every excluded model:
		// both plain unroutable entries and id collisions are exclusions.
		if d.Kind == DiagUnroutable || d.Kind == DiagIDCollision {
			unsup = append(unsup, UnsupportedModel{UpstreamID: d.ModelID, Reason: d.Reason})
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.raw, m.models, m.index, m.unsup, m.warns, m.diags = entries, models, index, unsup, warns, diags
}

// protocolEnabled reports whether the resolved route's protocol flag is on
// (spec 04 §4); flags default to true.
func (m *Manager) protocolEnabled(r Route) bool {
	switch r {
	case RouteChatCompletions:
		return m.cfg.Protocols.ChatCompletions
	case RouteMessages:
		return m.cfg.Protocols.Messages
	case RouteResponses:
		return m.cfg.Protocols.Responses
	case RouteGoogle:
		return m.cfg.Protocols.Google
	}
	return true
}

// resolve applies route priority strictly: user override, the catalog's
// authoritative api field, then the legacy compatibility table (family
// prefix) for catalogs that predate the api field. The returned diagnostic
// is informational for overrides and unroutable for unsupported catalogs;
// a nil diagnostic with ok=false means "no route determined".
func (m *Manager) resolve(e rawModel) (Route, string, bool, *Diagnostic) {
	if o, ok := m.cfg.RouteOverrides[e.ID]; ok {
		// Spec 04 §5: an override MUST be visible in diagnostics. Load
		// validation (spec 04 §6) guarantees a known protocol, so the
		// map lookup above is the only decision point here.
		diag := routeOverrideDiagnostic(e.ID, nativeProtocolFromAPI(e.API), Route(o.Protocol), o.Endpoint)
		return Route(o.Protocol), o.Endpoint, true, &diag
	}
	if len(e.API) > 0 {
		for _, p := range apiProtocolOrder {
			if _, ok := e.API[p.key]; ok {
				return p.route, "", true, nil
			}
		}
		reason := fmt.Sprintf("no supported api protocol (got %s)", strings.Join(sortedKeys(e.API), ", "))
		diag := unroutableDiagnostic(e.ID, "", reason)
		return "", "", false, &diag
	}
	id := strings.ToLower(e.ID)
	for _, p := range prefixRoutes {
		if strings.HasPrefix(id, p.prefix) {
			return p.route, "", true, nil
		}
	}
	return "", "", false, nil
}

// nativeProtocolFromAPI returns the catalog-declared native protocol for
// diagnostics, or "" when the entry carries no api map.
func nativeProtocolFromAPI(api map[string]rawAPI) Route {
	for _, p := range apiProtocolOrder {
		if _, ok := api[p.key]; ok {
			return p.route
		}
	}
	return ""
}

// supportedProtocolsFromAPI maps a catalog api map to protocol values in
// deterministic order. Legacy entries without api yield nil.
func supportedProtocolsFromAPI(api map[string]rawAPI) []Route {
	if len(api) == 0 {
		return nil
	}
	out := make([]Route, 0, len(api))
	for _, p := range apiProtocolOrder {
		if _, ok := api[p.key]; ok {
			out = append(out, p.route)
		}
	}
	return out
}

// sortedKeys returns map keys in a stable order for diagnostics.
func sortedKeys(m map[string]rawAPI) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// firstNonEmptyString returns the first non-empty value.
func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// JoinUpstreamURL joins the configured base-url onto an endpoint path and
// is the single source of truth for that join (spec 05 §2): base-url
// already carries the version segment (default .../zen/go/v1, catalog at
// {base}/models), so a leading "/v1" on the endpoint is dropped instead of
// doubled. The authority-escape check (endpointEscapesBase) must parse
// exactly the URL this produces — two independent join formulas would be
// free to drift apart and validate one URL while sending another.
func JoinUpstreamURL(baseURL, endpoint string) string {
	return strings.TrimSuffix(baseURL, "/") + strings.TrimPrefix(endpoint, "/v1")
}

// endpointEscapesBase reports whether joining baseURL with endpoint via
// JoinUpstreamURL would send the request to a different authority or scheme
// than the configured base-url. Any parse failure counts as escaping (fail
// closed).
func endpointEscapesBase(baseURL, endpoint string) bool {
	base, err := url.Parse(baseURL)
	if err != nil {
		return true
	}
	got, err := url.Parse(JoinUpstreamURL(baseURL, endpoint))
	if err != nil {
		return true
	}
	return !strings.EqualFold(got.Host, base.Host) || got.Scheme != base.Scheme
}

func firstNonEmpty(a, b []string) []string {
	if len(a) > 0 {
		return a
	}
	return b
}

// normalizeThinking converts the catalog's thinking metadata into its
// pluginapi form. The Console schema (capabilities.reasoning) wins when
// present; the legacy per-model thinking object is the fallback. Absent
// metadata yields nil, partial ones a partial struct.
func normalizeThinking(rt *rawThinking, caps *rawCapabilities) *pluginapi.ThinkingSupport {
	if caps != nil && caps.Reasoning != nil {
		if !caps.Reasoning.Supported {
			return nil
		}
		t := &pluginapi.ThinkingSupport{}
		if len(caps.Reasoning.Effort) > 0 {
			t.Levels = append([]string(nil), caps.Reasoning.Effort...)
			for _, lvl := range t.Levels {
				switch strings.ToLower(lvl) {
				case "none":
					t.ZeroAllowed = true
				case "auto":
					t.DynamicAllowed = true
				}
			}
		}
		return t
	}
	if rt == nil {
		return nil
	}
	t := &pluginapi.ThinkingSupport{}
	if rt.Min != nil {
		t.Min = *rt.Min
	}
	if rt.Max != nil {
		t.Max = *rt.Max
	}
	if rt.ZeroAllowed != nil {
		t.ZeroAllowed = *rt.ZeroAllowed
	}
	if rt.DynamicAllowed != nil {
		t.DynamicAllowed = *rt.DynamicAllowed
	}
	if len(rt.Levels) > 0 {
		t.Levels = append([]string(nil), rt.Levels...)
	}
	return t
}

// Models returns the routable snapshot (FR-003 identity fields). The slice
// is a shallow copy; records are rebuilt fresh on every swap and consumers
// treat them as read-only, so nested slices/thinking are shared safely.
func (m *Manager) Models() []ModelRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ModelRecord, len(m.models))
	copy(out, m.models)
	return out
}

// Lookup resolves an ID to its routable record, accepting the public ID
// (with or without prefix) or the bare upstream ID (FR-003): the PublicID
// key wins over UpstreamID when both exist.
func (m *Manager) Lookup(id string) (ModelRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.index[id]
	return rec, ok
}

// Unsupported returns models excluded from the routable set with reasons.
func (m *Manager) Unsupported() []UnsupportedModel {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]UnsupportedModel, len(m.unsup))
	copy(out, m.unsup)
	return out
}

// Warnings returns the human-readable message of every catalog diagnostic
// (info and warn) for compatibility with callers that predate the structured
// Diagnostics view.
func (m *Manager) Warnings() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.warns))
	copy(out, m.warns)
	return out
}

// Diagnostics returns the current structured catalog diagnostics.
func (m *Manager) Diagnostics() []Diagnostic {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Diagnostic, len(m.diags))
	copy(out, m.diags)
	return out
}
