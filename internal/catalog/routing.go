package catalog

import (
	"fmt"
	"net/url"
	"strings"

	"opencode-go-cliproxyapi/internal/config"
	"opencode-go-cliproxyapi/internal/errclass"
)

// RouteDecision is the per-request protocol choice. The catalog only
// describes model capabilities; this struct is what the executor acts on.
type RouteDecision struct {
	// ClientProtocol is the protocol CPA handed the plugin (SourceFormat).
	ClientProtocol Route
	// NativeProtocol is the model's catalog-native protocol.
	NativeProtocol Route
	// UpstreamProtocol is the protocol actually used for this request.
	UpstreamProtocol Route
	// UpstreamURL is the full configured endpoint for UpstreamProtocol.
	UpstreamURL string
	// ConversionNeeded reports whether the request/response bodies must be
	// translated between client and upstream protocols.
	ConversionNeeded bool
	// Reason is a short, secret-free explanation for diagnostics.
	Reason string
}

// implementedProtocols lists the protocols this plugin can actually speak to
// an upstream. generate-content has no adapter.
var implementedProtocols = map[Route]bool{
	RouteChatCompletions: true,
	RouteMessages:        true,
	RouteResponses:       true,
}

// textProtocolOrder is the deterministic preference order used when a
// request must fall back to a protocol other than the client's.
var textProtocolOrder = []Route{RouteChatCompletions, RouteMessages, RouteResponses}

// IsTextProtocol reports whether p is one of the text protocols the plugin
// implements.
func IsTextProtocol(p Route) bool {
	return implementedProtocols[p]
}

// ConsoleCatalog reports whether the record came from a catalog entry that
// declared its api map. Only those entries carry the Console translation
// guarantee; legacy catalogs get native-protocol routing only.
func (r ModelRecord) ConsoleCatalog() bool { return r.FromConsoleCatalog }

// isTextOutput reports whether the model produces text. A record without
// output modality metadata is assumed textual (the catalog's type field is
// "chat" for every Console model).
func (r ModelRecord) isTextOutput() bool {
	if len(r.OutputModes) == 0 {
		return true
	}
	for _, mode := range r.OutputModes {
		if strings.EqualFold(mode, "text") {
			return true
		}
	}
	return false
}

// SpeaksProtocol reports whether the upstream can serve this model through p.
//   - The native protocol always works.
//   - Console catalog text models work through every implemented text
//     protocol: the gateway translates between APIs (documented behavior).
//   - Legacy catalogs and non-text models are restricted to the protocols the
//     model explicitly declares; unknown capability is never assumed.
func (r ModelRecord) SpeaksProtocol(p Route) bool {
	if !IsTextProtocol(p) {
		return false
	}
	if p == r.NativeProtocol {
		return true
	}
	for _, supported := range r.SupportedProtocols {
		if supported == p {
			return true
		}
	}
	if !r.FromConsoleCatalog {
		return false
	}
	return r.isTextOutput()
}

// protocolEnabled reports whether the operator allows p as an upstream
// protocol.
func protocolEnabled(cfg config.Config, p Route) bool {
	switch p {
	case RouteChatCompletions:
		return cfg.Protocols.ChatCompletions
	case RouteMessages:
		return cfg.Protocols.Messages
	case RouteResponses:
		return cfg.Protocols.Responses
	case RouteGoogle:
		return cfg.Protocols.Google
	}
	return false
}

// usable reports whether p can be used as an upstream protocol for rec:
// implemented, enabled and speakable by the model.
func usable(cfg config.Config, rec ModelRecord, p Route) bool {
	return IsTextProtocol(p) && protocolEnabled(cfg, p) && rec.SpeaksProtocol(p)
}

// firstUsable returns the first usable protocol in deterministic order.
func firstUsable(cfg config.Config, rec ModelRecord) (Route, bool) {
	if usable(cfg, rec, rec.NativeProtocol) {
		return rec.NativeProtocol, true
	}
	for _, p := range textProtocolOrder {
		if usable(cfg, rec, p) {
			return p, true
		}
	}
	return "", false
}

// ResolveRoute applies the routing policy for one request:
// explicit override > routing.mode > capability check > native fallback.
func ResolveRoute(cfg config.Config, rec ModelRecord, client Route) (RouteDecision, *errclass.Error) {
	decision := RouteDecision{
		ClientProtocol: client,
		NativeProtocol: rec.NativeProtocol,
	}

	// 1. Explicit per-model override wins unconditionally, but the chosen
	// protocol still has to be implemented and enabled.
	if override, ok := cfg.RouteOverrides[rec.UpstreamID]; ok {
		target := Route(override.Protocol)
		if !IsTextProtocol(target) {
			return decision, errclass.Translation(fmt.Sprintf(
				"route override for %q selects unsupported protocol %q", rec.UpstreamID, override.Protocol))
		}
		if !protocolEnabled(cfg, target) {
			return decision, &errclass.Error{
				Class:   errclass.ClassUnsupported,
				Message: fmt.Sprintf("route override for %q selects protocol %q disabled by config", rec.UpstreamID, target),
			}
		}
		return finishDecision(cfg, rec, decision, target, fmt.Sprintf("model %q routed via user override (%s)", rec.UpstreamID, target))
	}

	// 2. Policy-driven target.
	var (
		target Route
		reason string
	)
	switch cfg.Routing.Mode {
	case config.RoutingNative:
		target = rec.NativeProtocol
		reason = fmt.Sprintf("native protocol %s", rec.NativeProtocol)
	case config.RoutingAnthropic:
		if !usable(cfg, rec, RouteMessages) {
			return decision, &errclass.Error{
				Class:   errclass.ClassUnsupported,
				Message: fmt.Sprintf("routing.mode anthropic cannot serve model %q through messages", rec.UpstreamID),
			}
		}
		target = RouteMessages
		reason = "routing.mode anthropic"
	default: // config.RoutingPreferClient
		if usable(cfg, rec, client) {
			target = client
			reason = fmt.Sprintf("client protocol %s", client)
		} else if cfg.Routing.AllowTranslation && usable(cfg, rec, rec.NativeProtocol) {
			target = rec.NativeProtocol
			reason = fmt.Sprintf("client protocol %s unavailable; fell back to native %s", client, rec.NativeProtocol)
		} else {
			return decision, &errclass.Error{
				Class: errclass.ClassUnsupported,
				Message: fmt.Sprintf(
					"model %q cannot be served through client protocol %s and translation is not allowed",
					rec.UpstreamID, client),
			}
		}
	}

	// 3. Native protocol without an adapter (generate-content): fall back to
	// a text protocol the model can speak, reporting why. Never silent.
	if target == RouteGoogle || !IsTextProtocol(target) {
		if fallback, ok := firstUsable(cfg, rec); ok {
			reason = fmt.Sprintf("native protocol %s has no transport; using %s", rec.NativeProtocol, fallback)
			target = fallback
		} else {
			return decision, &errclass.Error{
				Class:   errclass.ClassUnsupported,
				Message: fmt.Sprintf("model %q has no usable upstream protocol", rec.UpstreamID),
			}
		}
	}

	// 4. Protocol switches are the allowed upstream set.
	if !protocolEnabled(cfg, target) {
		if cfg.Routing.Mode == config.RoutingPreferClient && cfg.Routing.AllowTranslation {
			if fallback, ok := firstUsable(cfg, rec); ok {
				reason = fmt.Sprintf("protocol %s disabled by config; using %s", target, fallback)
				target = fallback
			} else {
				return decision, &errclass.Error{
					Class:   errclass.ClassUnsupported,
					Message: fmt.Sprintf("model %q has no enabled upstream protocol", rec.UpstreamID),
				}
			}
		} else {
			return decision, &errclass.Error{
				Class:   errclass.ClassUnsupported,
				Message: fmt.Sprintf("protocol %s is disabled by config for model %q", target, rec.UpstreamID),
			}
		}
	}

	return finishDecision(cfg, rec, decision, target, reason)
}

// finishDecision fills the resolved target and endpoint URL.
func finishDecision(cfg config.Config, rec ModelRecord, d RouteDecision, target Route, reason string) (RouteDecision, *errclass.Error) {
	url, eErr := EndpointURL(cfg, rec, target)
	if eErr != nil {
		return d, eErr
	}
	d.UpstreamProtocol = target
	d.UpstreamURL = url
	d.ConversionNeeded = d.ClientProtocol != "" && d.ClientProtocol != target
	d.Reason = reason
	return d, nil
}

// EndpointURL resolves the full upstream URL for a target protocol. A route
// override supplies a path joined onto the configured base; otherwise the
// per-protocol endpoint applies.
func EndpointURL(cfg config.Config, rec ModelRecord, target Route) (string, *errclass.Error) {
	if override, ok := cfg.RouteOverrides[rec.UpstreamID]; ok && override.Endpoint != "" {
		// Legacy direct construction has no base to join onto: use the path
		// verbatim. Config.Load always supplies a base.
		if cfg.BaseURL == "" {
			return override.Endpoint, nil
		}
		url := joinOverride(cfg, override.Endpoint)
		if urlEscapesBase(cfg.BaseURL, url) {
			return "", &errclass.Error{
				Class:   errclass.ClassUnsupported,
				Message: "resolved endpoint escapes the configured upstream host",
			}
		}
		return url, nil
	}
	switch target {
	case RouteChatCompletions:
		if cfg.Endpoints.ChatCompletions != "" {
			return cfg.Endpoints.ChatCompletions, nil
		}
	case RouteMessages:
		if cfg.Endpoints.Messages != "" {
			return cfg.Endpoints.Messages, nil
		}
	case RouteResponses:
		if cfg.Endpoints.Responses != "" {
			return cfg.Endpoints.Responses, nil
		}
	case RouteGoogle:
		if cfg.Endpoints.Google != "" {
			return strings.TrimRight(cfg.Endpoints.Google, "/") + "/models/" + rec.UpstreamID + ":generateContent", nil
		}
	}
	// Legacy direct construction without endpoints: leave the URL empty so
	// the executor falls back to JoinUpstreamURL(BaseURL, EndpointPath).
	// Configs loaded through config.Load always carry endpoints.
	if cfg.BaseURL == "" {
		return "", nil
	}
	return JoinUpstreamURL(cfg.BaseURL, target.EndpointPath()), nil
}

// joinOverride appends an override path to the configured base using the
// legacy rule: a leading /v1 is dropped because the base already carries the
// version segment (default base .../inference/go, legacy .../zen/go/v1).
func joinOverride(cfg config.Config, path string) string {
	return strings.TrimSuffix(cfg.BaseURL, "/") + strings.TrimPrefix(path, "/v1")
}

// urlEscapesBase reports whether fullURL leaves the configured base's
// authority or scheme. Any parse failure counts as escaping (fail closed).
func urlEscapesBase(baseURL, fullURL string) bool {
	base, err := url.Parse(baseURL)
	if err != nil || base.Host == "" {
		return true
	}
	got, err := url.Parse(fullURL)
	if err != nil || got.Host == "" {
		return true
	}
	return !strings.EqualFold(got.Host, base.Host) || got.Scheme != base.Scheme
}
