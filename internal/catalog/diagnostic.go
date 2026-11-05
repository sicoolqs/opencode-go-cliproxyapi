package catalog

import "fmt"

// DiagnosticKind classifies one catalog normalization diagnostic. Kinds are
// stable identifiers used for log fields and change detection.
type DiagnosticKind string

const (
	// DiagGoogleFallback reports a model whose native protocol is Gemini
	// generateContent, which this plugin has no transport for. Such models
	// are served through a text protocol chosen per request by the routing
	// policy; the catalog cannot know that protocol yet.
	DiagGoogleFallback DiagnosticKind = "google-fallback"
	// DiagRouteOverride reports a user-configured route override.
	DiagRouteOverride DiagnosticKind = "route-override"
	// DiagDuplicateModel reports a duplicated upstream model id.
	DiagDuplicateModel DiagnosticKind = "duplicate-model"
	// DiagIDCollision reports a public-id/upstream-id collision.
	DiagIDCollision DiagnosticKind = "id-collision"
	// DiagCatalogShape reports an upstream catalog shape anomaly.
	DiagCatalogShape DiagnosticKind = "catalog-shape"
	// DiagUnroutable reports a model excluded from the routable catalog.
	DiagUnroutable DiagnosticKind = "unroutable-model"
)

// Diagnostic levels, matching the host.log levels.
const (
	DiagnosticLevelInfo = "info"
	DiagnosticLevelWarn = "warn"
)

// Diagnostic is one structured catalog diagnostic. Message is the full,
// human-readable sentence logged at the diagnostic's level (it carries the
// model id, cause and impact so Docker's default output is self-explanatory);
// Reason is the short cause kept for the Unsupported() compatibility view;
// Fields are the structured attributes for log search.
type Diagnostic struct {
	Kind           DiagnosticKind
	Level          string
	ModelID        string
	NativeProtocol Route
	Reason         string
	Message        string
	Fields         map[string]any
}

// Key returns the change-detection identity of a diagnostic: a diagnostic is
// "new" when its kind, model and message were not present in the previous
// refresh, so a changing cause re-logs while an unchanged one stays quiet.
func (d Diagnostic) Key() string {
	return string(d.Kind) + "|" + d.ModelID + "|" + d.Message
}

// quotedID renders a model id safely for log messages and fields: %q escapes
// control characters so an upstream id cannot forge log lines (CWE-117).
func quotedID(id string) string {
	return fmt.Sprintf("%q", id)
}

// googleFallbackDiagnostic describes a generate-content model. It must not
// claim a concrete fallback protocol: the routing policy decides per request.
func googleFallbackDiagnostic(id string) Diagnostic {
	return Diagnostic{
		Kind:           DiagGoogleFallback,
		Level:          DiagnosticLevelInfo,
		ModelID:        id,
		NativeProtocol: RouteGoogle,
		Message: fmt.Sprintf(
			"catalog: model %s declares native protocol generate-content (google), which this plugin cannot transport; requests are served through a text protocol chosen per request by the routing policy; impact: model remains routable",
			quotedID(id)),
		Fields: map[string]any{
			"model":           quotedID(id),
			"native_protocol": string(RouteGoogle),
			"action":          "served-via-text-protocol",
			"impact":          "none",
		},
	}
}

// routeOverrideDiagnostic describes an intentional user override.
func routeOverrideDiagnostic(id string, native Route, protocol Route, endpoint string) Diagnostic {
	return Diagnostic{
		Kind:           DiagRouteOverride,
		Level:          DiagnosticLevelInfo,
		ModelID:        id,
		NativeProtocol: native,
		Message: fmt.Sprintf(
			"catalog: model %s routed via user override to %s (endpoint %s); impact: overrides the catalog native protocol %s for this model",
			quotedID(id), protocol, quotedID(endpoint), native),
		Fields: map[string]any{
			"model":             quotedID(id),
			"native_protocol":   string(native),
			"upstream_protocol": string(protocol),
			"endpoint":          endpoint,
			"impact":            "overrides-native-protocol",
		},
	}
}

// duplicateDiagnostic reports a duplicated upstream id.
func duplicateDiagnostic(id string) Diagnostic {
	return Diagnostic{
		Kind:    DiagDuplicateModel,
		Level:   DiagnosticLevelWarn,
		ModelID: id,
		Reason:  "duplicate model id",
		Message: fmt.Sprintf(
			"catalog: duplicate model %s ignored (keeping the first occurrence); impact: one catalog entry is not published",
			quotedID(id)),
		Fields: map[string]any{
			"model":  quotedID(id),
			"impact": "entry-not-published",
		},
	}
}

// collisionDiagnostic reports a public/upstream id collision.
func collisionDiagnostic(id, reason string) Diagnostic {
	return Diagnostic{
		Kind:    DiagIDCollision,
		Level:   DiagnosticLevelWarn,
		ModelID: id,
		Reason:  reason,
		Message: fmt.Sprintf(
			"catalog: model %s excluded because %s; impact: not listed in /v1/models and requests return invalid_model",
			quotedID(id), reason),
		Fields: map[string]any{
			"model":  quotedID(id),
			"reason": reason,
			"impact": "not-routable",
		},
	}
}

// shapeDiagnostic reports an upstream catalog shape anomaly.
func shapeDiagnostic(message, impact string) Diagnostic {
	return Diagnostic{
		Kind:    DiagCatalogShape,
		Level:   DiagnosticLevelWarn,
		Message: message,
		Fields: map[string]any{
			"impact": impact,
		},
	}
}

// missingIDDiagnostic reports an entry without an id.
func missingIDDiagnostic() Diagnostic {
	return Diagnostic{
		Kind:    DiagUnroutable,
		Level:   DiagnosticLevelWarn,
		Reason:  "missing id",
		Message: "catalog: one catalog entry has no id and was skipped; impact: that entry is not published",
		Fields: map[string]any{
			"reason": "missing id",
			"impact": "entry-not-published",
		},
	}
}

// unroutableDiagnostic reports a model excluded from the routable catalog.
func unroutableDiagnostic(id string, native Route, reason string) Diagnostic {
	d := Diagnostic{
		Kind:           DiagUnroutable,
		Level:          DiagnosticLevelWarn,
		ModelID:        id,
		NativeProtocol: native,
		Reason:         reason,
		Message: fmt.Sprintf(
			"catalog: model %s excluded from the routable catalog: %s; impact: not listed in /v1/models and requests return invalid_model",
			quotedID(id), reason),
		Fields: map[string]any{
			"model":  quotedID(id),
			"reason": reason,
			"impact": "not-routable",
		},
	}
	if native != "" {
		d.Fields["native_protocol"] = string(native)
	}
	return d
}
