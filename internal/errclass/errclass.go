// Package errclass implements FR-009 error classification and the
// resolved §7 retry semantics (07-open-questions.md).
package errclass

import (
	"context"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// Class is a stable error-category identifier (FR-009) used as the
// envelope error Code.
type Class string

const (
	ClassAuth         Class = "auth_failure"
	ClassInvalidModel Class = "invalid_model"
	ClassUnsupported  Class = "unsupported_protocol_or_parameter"
	ClassRateLimit    Class = "rate_limit"
	ClassQuota        Class = "quota_exhaustion"
	ClassBilling      Class = "billing"
	ClassUpstream     Class = "upstream_server_failure"
	ClassNetwork      Class = "timeout_or_network_failure"
	ClassTranslation  Class = "translation_failure"
)

// Host-recognized pluginabi error codes (CLIProxyAPI v8.0.62+). They are
// defined locally so the plugin keeps its v8.0.50 SDK pin: older hosts ignore
// unknown codes and fall back to HTTP-status behavior, newer hosts use them to
// decide credential/model cooldown and request retries.
//
//   - CodeTransientTransport: pre-HTTP dial/TLS/DNS/reset/EOF failure. Skips
//     credential cooldown and stays eligible for request retries; without it a
//     plain 502 cools the credential for the transient-error window.
//   - CodeStreamTruncated: an accepted upstream stream ended without a
//     completion frame. Skips credential cooldown and forbids replaying the
//     request on another credential (the generation was already submitted).
//   - CodeRequestScoped: the failure belongs to the request (translation,
//     unsupported parameter), so CPA skips cooldown and stops rotation.
//   - CodeConnectionLifecycle: the downstream client disconnected or the
//     stream session closed; CPA skips cooldown but keeps credential rotation.
//   - CodeModelNotFound: cool only the requested model instead of the whole
//     credential for 12h.
//   - CodeNotImplemented: optional capability (count_tokens) unsupported;
//     availability-neutral for the credential and model.
//   - CodeCredentialQuota: a 429/402 that exhausts the entire credential
//     across all models.
const (
	CodeTransientTransport  = "transient_transport"
	CodeStreamTruncated     = "stream_truncated"
	CodeRequestScoped       = "request_scoped"
	CodeConnectionLifecycle = "connection_lifecycle"
	CodeModelNotFound       = "model_not_found"
	CodeNotImplemented      = "not_implemented"
	CodeCredentialQuota     = "credential_quota"
)

// Error describes classified request errors and retryability for CPA execution.
type Error struct {
	Class Class
	// Code overrides the envelope error code with a host-recognized value.
	// Empty falls back to the Class-derived code (or request_scoped for
	// request-level classes).
	Code              string
	Message           string
	StatusCode        int
	Retryable         bool
	RetryAfterSeconds int
}

func (e *Error) Error() string {
	return string(e.Class) + ": " + e.Message
}

// FromStatus classifies an upstream HTTP status per resolved §7.
// Network/timeout failures (no status) use FromNetwork instead. A redacted
// message containing "quota" (case-insensitive) upgrades 429/402/403 to
// ClassQuota — a bounded heuristic given no documented upstream quota
// taxonomy. Optional response headers are scanned for Retry-After retry
// guidance (delay-seconds or HTTP-date); a valid positive value is carried
// on the error so ToEnvelopeError can hand it to the host for auth
// cooldowns and downstream Retry-After headers.
func FromStatus(status int, message string, headers ...http.Header) *Error {
	msg := Redact(message)
	lowerMsg := strings.ToLower(msg)
	quota := strings.Contains(lowerMsg, "quota")
	retryAfter := 0
	for _, h := range headers {
		if v := RetryAfterFromHeaders(h); v > 0 {
			retryAfter = v
			break
		}
	}
	quotaClass := func(base Class) Class {
		if quota {
			return ClassQuota
		}
		return base
	}
	switch status {
	case 401:
		// A rejected credential is not retryable for this auth: the host must
		// rotate to another credential instead of retrying the same key.
		return &Error{Class: ClassAuth, Message: msg, StatusCode: status, Retryable: false, RetryAfterSeconds: retryAfter}
	case 403:
		return &Error{Class: quotaClass(ClassAuth), Message: msg, StatusCode: status, Retryable: true, RetryAfterSeconds: retryAfter}
	case 402:
		return &Error{Class: quotaClass(ClassBilling), Message: msg, StatusCode: status, Retryable: true, RetryAfterSeconds: retryAfter}
	case 404:
		// A retired/unknown model must cool only that model; a plain 404 would
		// otherwise take the whole credential out for 12 hours on newer hosts.
		return &Error{Class: ClassInvalidModel, Code: CodeModelNotFound, Message: msg, StatusCode: status, RetryAfterSeconds: retryAfter}
	case 429:
		e := &Error{Class: quotaClass(ClassRateLimit), Message: msg, StatusCode: status, Retryable: true, RetryAfterSeconds: retryAfter}
		if quota {
			// Explicit whole-credential quota refusal; the code keeps the scope
			// across RPC envelopes and stream error details.
			e.Code = CodeCredentialQuota
		}
		return e
	}
	if status >= 500 {
		return &Error{Class: ClassUpstream, Message: msg, StatusCode: status, Retryable: true, RetryAfterSeconds: retryAfter}
	}
	return &Error{Class: ClassUnsupported, Message: msg, StatusCode: status, RetryAfterSeconds: retryAfter}
}

// RetryAfterFromHeaders extracts upstream retry guidance in seconds from
// response headers: an integer delay-seconds value, or an HTTP-date
// converted to remaining seconds. Invalid, past, or non-positive values
// yield 0 so the host falls back to its default backoff. Only the first
// header value is honored. Header names and values are fixed protocol
// tokens — no secrets pass through here.
func RetryAfterFromHeaders(h http.Header) int {
	if len(h) == 0 {
		return 0
	}
	raw := strings.TrimSpace(h.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs <= 0 {
			return 0
		}
		return secs
	}
	if ts, err := http.ParseTime(raw); err == nil {
		secs := int(time.Until(ts).Seconds())
		if secs <= 0 {
			return 0
		}
		return secs
	}
	return 0
}

// FromNetwork classifies a PRE-first-byte network/timeout failure. Timeouts
// surface as 504 and other transport failures as 502, so clients see a
// retryable gateway error instead of an opaque 500. Non-timeout transport
// failures carry the host-recognized transient_transport code so CPA does not
// cool a healthy credential for an upstream reset/EOF; timeouts keep the
// status-based transient cooldown. Failures observed after the first upstream
// byte must use PostFirstByteNetwork instead.
func FromNetwork(err error) *Error {
	msg := ""
	if err != nil {
		msg = Redact(err.Error())
	}
	status := http.StatusBadGateway
	code := CodeTransientTransport
	if isTimeoutError(err) {
		status = http.StatusGatewayTimeout
		code = ""
	}
	return &Error{Class: ClassNetwork, Code: code, Message: msg, Retryable: true, StatusCode: status}
}

// isTimeoutError reports whether err represents a deadline/timeout.
func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "timed out") ||
		strings.Contains(lower, "timeout") ||
		strings.Contains(lower, "deadline exceeded")
}

// PostFirstByteNetwork classifies a network/timeout failure seen AFTER the
// first upstream byte was received: part of the response may already be
// delivered downstream, so a retry would risk duplicate delivery of a half
// stream — never retryable (§7). The stream_truncated code additionally tells
// the host to skip credential cooldown and never replay the request on another
// credential, because the generation was already submitted upstream.
func PostFirstByteNetwork(msg string) *Error {
	return &Error{Class: ClassNetwork, Code: CodeStreamTruncated, Message: Redact(msg), Retryable: false}
}

// Translation reports an FR-005/FR-006 translation failure; never retryable.
// It is request-scoped: no credential rotation or cooldown can fix a malformed
// or unsupported client payload.
func Translation(msg string) *Error {
	return &Error{Class: ClassTranslation, Code: CodeRequestScoped, Message: Redact(msg)}
}

// NotImplemented reports an optional capability the plugin does not implement
// for this source format (count_tokens outside claude). The code keeps the
// credential and model availability-neutral on newer hosts; 501 keeps the
// same meaning on older hosts.
func NotImplemented(msg string) *Error {
	return &Error{Class: ClassUnsupported, Code: CodeNotImplemented, Message: Redact(msg), StatusCode: http.StatusNotImplemented}
}

// UpstreamFallback builds a ClassUpstream error for the adapter fallback
// sites (chunk/failure errors) that have no upstream status code. Resolved
// §7 makes upstream server failures retryable; the message is redacted at
// construction.
func UpstreamFallback(msg string) *Error {
	return &Error{Class: ClassUpstream, Message: Redact(msg), Retryable: true}
}

// ToEnvelopeError converts to the shared SDK wire type with a redacted
// message. Re-redacting at the envelope edge is INTENTIONAL defense-in-depth:
// constructors already redact (so non-envelope consumers — logs, executor
// fail paths reading .Message directly) are safe, and Redact is idempotent.
//
// Request-level classes (unsupported parameter, translation failure) default
// to the host-recognized request_scoped code so a malformed client request
// cannot cool a healthy credential or trigger pointless rotation.
func ToEnvelopeError(e *Error) pluginabi.Error {
	status := e.StatusCode
	code := e.Code
	if code == "" {
		switch e.Class {
		case ClassUnsupported, ClassTranslation:
			code = CodeRequestScoped
		default:
			code = string(e.Class)
		}
	}
	if status == 0 {
		switch e.Class {
		case ClassUnsupported, ClassTranslation:
			status = 400
		}
	}
	return pluginabi.Error{
		Code:              code,
		Message:           Redact(e.Message),
		Retryable:         e.Retryable,
		HTTPStatus:        status,
		RetryAfterSeconds: e.RetryAfterSeconds,
	}
}

// bearerRe matches a bearer token (case-insensitive scheme, optional
// space/tab separator, token terminated by whitespace, comma, or quote).
// The separator is optional so a bare "bearer" word is redacted exactly as
// the previous hand-rolled scanner did. Redact is idempotent — applying it
// twice yields the same output — which is what makes edge re-redaction safe.
var bearerRe = regexp.MustCompile(`(?i)bearer[ \t]*[^\s,"]*`)

// apiKeyRe finds x-api-key header echoes (Messages-route auth, FR-007):
// case-insensitive name, then a run of quote/colon/equals/space separators,
// then the value run. The value class spans hex/base62 plus the base64
// alphabet ("+", "/", "="), matching bearerRe/queryKeyRe sibling classes.
// The candidate is only replaced when the value looks
// like a key (contains a digit) so ordinary prose after the header name —
// "x-api-key authentication failed" — passes untouched; every real key
// alphabet (hex, base64, base62) carries digits. Idempotent: "[redacted]"
// starts with '[' which is outside the value class, so it never re-matches.
var apiKeyRe = regexp.MustCompile(`(?i)x-api-key["':= ]*[A-Za-z0-9_\-.+/=]+`)

func redactAPIKey(match string) string {
	value := strings.TrimLeft(match[len("x-api-key"):], "\"':= ")
	hasDigit := false
	for _, r := range value {
		if r >= '0' && r <= '9' {
			hasDigit = true
			break
		}
	}
	if !hasDigit || len(value) < 8 {
		return match
	}
	return match[:len(match)-len(value)] + "[redacted]"
}

// queryKeyRe redacts secrets carried in URL query strings (FR-011): transport
// failures conventionally echo the full URL, so base-url/catalog-url keys
// passed as ?api_key=… / ?token=… must not survive into envelopes or logs.
// The value run stops at whitespace, '&', and quotes so consecutive params
// survive; the replacement keeps the parameter name. Idempotent: the
// redacted value "[redacted]" still matches the value class, so re-running
// yields the same text.
var queryKeyRe = regexp.MustCompile(`(?i)([?&](?:api[_-]?key|key|access[_-]?token|token)=)[^\s&"']+`)

// Redact strips bearer tokens, x-api-key echoes, and query-string key
// parameters from a message so keys never reach logs or clients.
func Redact(msg string) string {
	msg = bearerRe.ReplaceAllString(msg, "Bearer [redacted]")
	msg = apiKeyRe.ReplaceAllStringFunc(msg, redactAPIKey)
	return queryKeyRe.ReplaceAllString(msg, "${1}[redacted]")
}
