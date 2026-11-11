// Non-stream and stream execution paths (FR-005/FR-006/FR-007, arch §4/§5
// steps 5-8): resolve the public model ID against the catalog snapshot,
// translate the inbound payload to the record's upstream protocol, call
// upstream through the host HTTP callbacks, and translate back to the
// client protocol. Request authentication is selected by CPA.

package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/adapter/chatcompletions"
	"opencode-go-cliproxyapi/internal/adapter/messages"
	"opencode-go-cliproxyapi/internal/adapter/responses"
	"opencode-go-cliproxyapi/internal/adapter/shared"
	"opencode-go-cliproxyapi/internal/catalog"
	"opencode-go-cliproxyapi/internal/config"
	"opencode-go-cliproxyapi/internal/errclass"
)

// executorRequest mirrors rpcExecutorRequest: the SDK embeds
// pluginapi.ExecutorRequest untagged, so its fields marshal under Go field
// names ("Model", "SourceFormat", "OriginalRequest", "Stream"). StreamID is
// the DOWNSTREAM host-allocated id for executor.execute_stream emissions;
// ids returned by DoStream are UPSTREAM and never interchangeable (§4).
type executorRequest struct {
	pluginapi.ExecutorRequest
	StreamID string `json:"stream_id,omitempty"`
}

// resolvedExecution carries everything both execution paths need after
// model/key resolution succeeded.
type resolvedExecution struct {
	cfg   config.Config
	rec   catalog.ModelRecord
	key   string
	tools shared.ResponseTools
}

// resolveExecution resolves the requested model against the snapshot and
// extracts the key selected by CPA. A non-nil second return is a ready-made
// failure envelope.
func (m *Manager) resolveExecution(req executorRequest) (*resolvedExecution, []byte) {
	m.mu.RLock()
	cfg, mgr := m.cfg, m.mgr
	m.mu.RUnlock()
	if req.AuthProvider != ProviderID {
		return nil, classEnvelope(&errclass.Error{Class: errclass.ClassAuth, Message: "selected auth provider is not opencode-go"})
	}
	key := strings.TrimSpace(req.AuthAttributes["api_key"])
	debugTrace("executor auth model=%s auth_id=%s provider=%s attr_api_key_present=%t attr_count=%d storage_json_bytes=%d", req.Model, req.AuthID, req.AuthProvider, key != "", len(req.AuthAttributes), len(req.StorageJSON))
	if key == "" {
		return nil, classEnvelope(&errclass.Error{Class: errclass.ClassAuth, Message: "selected auth has no api key"})
	}
	var rec catalog.ModelRecord
	var found bool
	if mgr != nil && req.Model != "" {
		rec, found = mgr.Lookup(req.Model)
	}
	if !found {
		return nil, classEnvelope(&errclass.Error{
			Class:      errclass.ClassInvalidModel,
			Message:    "model not in routable catalog",
			StatusCode: http.StatusNotFound,
		})
	}
	if rec.Protocol == catalog.RouteGoogle {
		// ResolveRoute falls back to a text protocol for Google-native
		// models; reaching here means no usable text protocol exists.
		return nil, classEnvelope(&errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: "model has no usable text upstream protocol",
		})
	}
	return &resolvedExecution{cfg: cfg, rec: rec, key: key, tools: *shared.NewResponseTools()}, nil
}

// CPA SourceFormat values the plugin accepts. They mirror the SDK's
// sdk/translator.Format* constants (asserted by the sdkcheck-tagged test);
// the SDK translator package is not imported at runtime because it pulls the
// whole gateway dependency graph into this shared library.
const (
	sourceFormatOpenAI         = "openai"
	sourceFormatClaude         = "claude"
	sourceFormatOpenAIResponse = "openai-response"
)

// parseClientProtocol maps CPA's SourceFormat to the plugin's protocol
// vocabulary. Only formats the plugin declares as input are accepted; an
// unknown format is an explicit error, never a guess.
func parseClientProtocol(sourceFormat string) (catalog.Route, *errclass.Error) {
	switch strings.ToLower(strings.TrimSpace(sourceFormat)) {
	case sourceFormatOpenAI:
		return catalog.RouteChatCompletions, nil
	case sourceFormatClaude:
		return catalog.RouteMessages, nil
	case sourceFormatOpenAIResponse:
		return catalog.RouteResponses, nil
	}
	return "", shared.UnsupportedFormat(sourceFormat, "OpenCode Go routing")
}

// resolveRoute is the single routing entry point shared by the streaming and
// non-streaming paths: explicit override > routing.mode > capability check >
// native fallback.
func (m *Manager) resolveRoute(res *resolvedExecution, sourceFormat string) (catalog.RouteDecision, []byte) {
	client, eErr := parseClientProtocol(sourceFormat)
	if eErr != nil {
		return catalog.RouteDecision{}, classEnvelope(eErr)
	}
	decision, eErr := catalog.ResolveRoute(res.cfg, res.rec, client)
	if eErr != nil {
		return catalog.RouteDecision{}, classEnvelope(eErr)
	}
	debugTrace("executor route model=%s client=%s native=%s upstream=%s conversion=%t reason=%s",
		res.rec.UpstreamID, decision.ClientProtocol, decision.NativeProtocol,
		decision.UpstreamProtocol, decision.ConversionNeeded, decision.Reason)
	return decision, nil
}

// handleCountTokens implements executor.count_tokens by proxying the
// Anthropic count_tokens endpoint with the selected credential. Only the
// claude source format has an upstream equivalent; other formats keep the
// explicit unsupported error.
func (m *Manager) handleCountTokens(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed executor request body"), nil
	}
	res, failEnv := m.resolveExecution(req)
	if res == nil {
		return failEnv, nil
	}
	if strings.ToLower(strings.TrimSpace(req.SourceFormat)) != "claude" {
		return classEnvelope(errclass.NotImplemented("executor.count_tokens is implemented for the claude source format only")), nil
	}
	sessionID, eErr := resolveOpenCodeSessionID(req)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	body, eErr := shared.RewriteModelID(res.rec.UpstreamID, req.OriginalRequest, "claude")
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	base := strings.TrimRight(strings.TrimSpace(res.cfg.Endpoints.Messages), "/")
	if base == "" {
		base = strings.TrimRight(catalog.JoinUpstreamURL(res.cfg.BaseURL, "/v1/messages"), "/")
	}
	url := base + "/count_tokens"
	timeout := res.cfg.RequestTimeout
	if timeout <= 0 || timeout > 20*time.Second {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	resp, err := m.bridge.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     url,
		Headers: upstreamAuthHeaders(catalog.RouteMessages, res.key, sessionID, req.Headers.Get("User-Agent")),
		Body:    body,
	})
	if err != nil {
		return classEnvelope(errclass.FromNetwork(err)), nil
	}
	if resp.StatusCode >= 400 {
		return classEnvelope(shared.UpstreamStatusError(resp.StatusCode, resp.Body, resp.Headers)), nil
	}
	if int64(len(resp.Body)) > res.cfg.MaxResponseBytes {
		return classEnvelope(errclass.Translation("response exceeds max-response-bytes")), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: resp.Body, Headers: resp.Headers}), nil
}

// handleExecute implements executor.execute (non-stream). Stream-flagged
// requests are routed to the stream path instead of rejected.
func (m *Manager) handleExecute(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed executor request body"), nil
	}
	debugTrace("executor invoked model=%s source_format=%s stream=%t original_body_%s payload_%s", req.Model, req.SourceFormat, req.Stream, debugBodyMeta(req.OriginalRequest), debugBodyMeta(req.Payload))
	if req.Stream {
		return m.executeStream(req)
	}
	res, failEnv := m.resolveExecution(req)
	if res == nil {
		return failEnv, nil
	}
	sessionID, eErr := resolveOpenCodeSessionID(req)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	decision, routeFail := m.resolveRoute(res, req.SourceFormat)
	if routeFail != nil {
		return routeFail, nil
	}
	debugTrace("executor session mode=%s source_format=%s x_opencode_session=%s fallback=%t", "non-stream", req.SourceFormat, sessionID, sessionID == emptyOpenCodeSessionID)
	upstreamBody, eErr := buildUpstreamRequest(decision.UpstreamProtocol, res.rec.UpstreamID, req.SourceFormat, req.OriginalRequest, res.rec.Thinking, &res.tools)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}

	url := decision.UpstreamURL
	if url == "" {
		url = catalog.JoinUpstreamURL(res.cfg.BaseURL, res.rec.EndpointPath)
	}
	debugTrace("executor resolved public_model=%s upstream_model=%s route=%s url=%s key_count=%d", req.Model, res.rec.UpstreamID, decision.UpstreamProtocol, url, len(res.cfg.APIKeys))
	debugTrace("executor sending non-stream url=%s body_len=%d", url, len(upstreamBody))
	ctx, cancel := context.WithTimeout(context.Background(), res.cfg.RequestTimeout)
	defer cancel()
	resp, err := m.bridge.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     url,
		Headers: upstreamAuthHeaders(decision.UpstreamProtocol, res.key, sessionID, req.Headers.Get("User-Agent")),
		Body:    upstreamBody,
	})
	if err != nil {
		debugTrace("executor non-stream network error: %v", err)
		return classEnvelope(errclass.FromNetwork(err)), nil
	}
	debugTrace("executor received non-stream status=%d body_len=%d", resp.StatusCode, len(resp.Body))
	if resp.StatusCode >= 400 {
		return classEnvelope(shared.UpstreamStatusError(resp.StatusCode, resp.Body, resp.Headers)), nil
	}
	// Parse/envelope guard only; true OOM prevention belongs to the host transport's byte cap.
	if int64(len(resp.Body)) > res.cfg.MaxResponseBytes {
		return classEnvelope(errclass.Translation("response exceeds max-response-bytes")), nil
	}
	converted, eErr := convertNonStream(decision.UpstreamProtocol, req.SourceFormat, resp.StatusCode, resp.Body, &res.tools)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: converted, Headers: resp.Headers}), nil
}

func buildUpstreamRequest(route catalog.Route, upstreamModel, sourceFormat string, sourceBody []byte, ts *pluginapi.ThinkingSupport, tools ...*shared.ResponseTools) ([]byte, *errclass.Error) {
	switch route {
	case catalog.RouteChatCompletions:
		return chatcompletions.BuildRequest(upstreamModel, sourceFormat, sourceBody, ts, tools...)
	case catalog.RouteMessages:
		return messages.BuildRequest(upstreamModel, sourceFormat, sourceBody, ts, tools...)
	case catalog.RouteResponses:
		return responses.BuildRequest(upstreamModel, sourceFormat, sourceBody, ts, tools...)
	}
	return nil, errclass.Translation("unsupported route")
}

func upstreamAuthHeaders(route catalog.Route, key, sessionID, clientUA string) http.Header {
	var h http.Header
	if route == catalog.RouteMessages {
		h = messages.AuthHeaders(key)
	} else {
		// Chat Completions and Responses endpoints are OpenAI-style bearer.
		h = chatcompletions.AuthHeaders(key)
	}
	// OpenCode Go requires clients to identify themselves with a distinct user
	// agent rather than a generic HTTP-library name, and to send a stable
	// session ID. A real client UA is forwarded when it is meaningful;
	// generic library/framework UAs fall back to the plugin's own identity.
	h.Set("User-Agent", upstreamUserAgent(clientUA))
	h.Set("x-opencode-session", sessionID)
	return h
}

// maxUserAgentLen bounds a forwarded client UA so a hostile client cannot
// inflate the upstream header without limit.
const maxUserAgentLen = 200

// genericUserAgentPrefixes lists SDK/HTTP-library/framework UA prefixes that
// OpenCode Go explicitly does not want to see ("rather than a generic SDK or
// HTTP-library name"). They are matched case-insensitively as prefixes.
var genericUserAgentPrefixes = []string{
	"go-http-client", "python-requests", "python-urllib", "httpx", "aiohttp",
	"curl/", "wget/", "libwww", "okhttp", "java/", "node-fetch", "undici",
	"axios", "node/", "deno/", "bun/", "dart:io", "hertz", "fasthttp",
	"go-resty", "resty", "postmanruntime", "insomnia", "httpie", "reqwest",
	"hyper", "guzzle", "restsharp", "unity",
}

// upstreamUserAgent forwards a meaningful client UA (Claude Code, Codex,
// OpenCode, a named agent) and falls back to the plugin identity for empty,
// oversized, control-character or generic library/framework UAs.
func upstreamUserAgent(clientUA string) string {
	ua := strings.TrimSpace(clientUA)
	if ua == "" || len(ua) > maxUserAgentLen || hasHeaderControlChars(ua) || isGenericUserAgent(ua) {
		return pluginName + "/" + pluginVersion
	}
	return ua
}

func isGenericUserAgent(ua string) bool {
	lower := strings.ToLower(ua)
	for _, prefix := range genericUserAgentPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func hasHeaderControlChars(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

const emptyOpenCodeSessionID = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// sessionMetadataKeys lists CPA/host metadata keys carrying a stable
// conversation identity, most authoritative first.
var sessionMetadataKeys = []string{
	"canonical_session_id",
	"session_id",
	"conversation_id",
	"prompt_cache_key",
}

// sessionHeaderNames lists client headers carrying a stable conversation
// identity, official OpenCode header first, then the native headers of the
// harnesses Console recognizes (Claude Code, Codex, generic session ids).
var sessionHeaderNames = []string{
	"X-Opencode-Session",
	"X-Session-Affinity",
	"X-Claude-Code-Session-Id",
	"X-Conversation-Id",
	"X-Session-Id",
	"Session-Id",
}

// maxSessionIDLen bounds a forwarded session identifier so a hostile client
// cannot inflate the upstream header without limit.
const maxSessionIDLen = 200

// resolveOpenCodeSessionID applies the precedence: CPA's canonical identity,
// an explicit inbound session header, then a digest of the conversation's
// first user turn. Every branch is deterministic, so a multi-turn
// conversation keeps one session ID across requests instead of minting a
// fresh one per call.
func resolveOpenCodeSessionID(req executorRequest) (string, *errclass.Error) {
	for _, key := range sessionMetadataKeys {
		if raw, ok := req.Metadata[key]; ok {
			if sid, ok := raw.(string); ok {
				if cleaned := sanitizeSessionID(sid); cleaned != "" {
					return cleaned, nil
				}
			}
		}
	}
	for _, name := range sessionHeaderNames {
		if sid := sanitizeSessionID(req.Headers.Get(name)); sid != "" {
			return sid, nil
		}
	}
	return deriveOpenCodeSessionID(req.SourceFormat, req.OriginalRequest)
}

// sanitizeSessionID trims a candidate identifier and rejects values that are
// empty or longer than maxSessionIDLen.
func sanitizeSessionID(sid string) string {
	sid = strings.TrimSpace(sid)
	if sid == "" || len(sid) > maxSessionIDLen {
		return ""
	}
	return sid
}

// deriveOpenCodeSessionID hashes the model-visible content of the initial user
// turn before translation (FR-012/AC-H). Metadata is excluded;
// valid requests without a user turn use the fixed empty-input digest.
func deriveOpenCodeSessionID(sourceFormat string, originalRequest []byte) (string, *errclass.Error) {
	var content strings.Builder
	appendParts := func(raw json.RawMessage, target string) *errclass.Error {
		parts, eErr := shared.DecodeStringOrParts(raw, target)
		if eErr != nil {
			return eErr
		}
		for _, part := range parts {
			if part.ImageURL != "" {
				content.WriteString(part.ImageURL)
			} else {
				content.WriteString(part.Text)
			}
		}
		return nil
	}

	switch sourceFormat {
	case "openai":
		var req shared.ChatCompletionsRequest
		if err := json.Unmarshal(originalRequest, &req); err != nil {
			return "", errclass.Translation("malformed openai request JSON: " + err.Error())
		}
		started := false
		for _, msg := range req.Messages {
			if !started {
				if msg.Role == "system" || msg.Role == "developer" {
					continue
				}
				if msg.Role != "user" {
					break
				}
				started = true
			} else if msg.Role != "user" {
				break
			}
			if eErr := appendParts(msg.Content, "/v1/chat/completions"); eErr != nil {
				return "", eErr
			}
		}
	case "claude":
		req, eErr := shared.DecodeClaudeMessages(originalRequest)
		if eErr != nil {
			return "", eErr
		}
		started := false
		for _, msg := range req.Messages {
			if !started {
				if msg.Role != "user" {
					continue
				}
				started = true
			} else if msg.Role != "user" {
				break
			}
			if msg.Content != "" {
				content.WriteString(msg.Content)
			}
			for _, block := range msg.Blocks {
				switch block.Kind {
				case "text":
					content.WriteString(block.Text)
				case "image":
					content.WriteString(block.URL)
				case "tool_result":
					parts, eErr := shared.ToolResultParts(block.Result)
					if eErr != nil {
						return "", eErr
					}
					for _, part := range parts {
						content.WriteString(part.Text)
						content.WriteString(part.ImageURL)
					}
				}
			}
		}
	case "openai-response":
		var req shared.ResponsesRequest
		if err := json.Unmarshal(originalRequest, &req); err != nil {
			return "", errclass.Translation("malformed openai-response request JSON: " + err.Error())
		}
		items, eErr := req.DecodeInputItems()
		if eErr != nil {
			return "", eErr
		}
		started := false
		for _, item := range items {
			isUserMessage := item.Role == "user" && (item.Type == "message" || item.Type == "")
			if !started {
				if !isUserMessage {
					continue
				}
				started = true
			} else if !isUserMessage {
				break
			}
			if eErr := appendParts(item.Content, "/v1/responses"); eErr != nil {
				return "", eErr
			}
		}
	default:
		return "", shared.UnsupportedFormat(sourceFormat, "OpenCode Go session derivation")
	}

	digest := sha256.Sum256([]byte(content.String()))
	return hex.EncodeToString(digest[:]), nil
}

// convertNonStream routes one upstream response to its adapter's uniform
// translator: every adapter owns status classification (>=400 → §7
// classified errors), native passthrough, and cross-format conversion for
// all client formats.
func convertNonStream(route catalog.Route, sourceFormat string, status int, body []byte, tools ...*shared.ResponseTools) ([]byte, *errclass.Error) {
	switch route {
	case catalog.RouteChatCompletions:
		return chatcompletions.ConvertNonStreamResponse(sourceFormat, status, body, tools...)
	case catalog.RouteMessages:
		return messages.ConvertNonStreamResponse(sourceFormat, status, body, tools...)
	case catalog.RouteResponses:
		return responses.ConvertNonStreamResponse(sourceFormat, status, body, tools...)
	}
	return nil, errclass.Translation("unsupported route")
}

// classEnvelope renders a classified failure as the wire error envelope;
// ToEnvelopeError redacts and propagates Retryable/HTTPStatus/RetryAfterSeconds host-side.
func classEnvelope(e *errclass.Error) []byte {
	wire := errclass.ToEnvelopeError(e)
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &wire})
	return out
}

// streamStartTimeout bounds how long the upstream may take to produce
// response headers (first byte). It is capped by request-timeout so an
// operator-configured shorter value still wins.
var streamStartTimeout = 60 * time.Second

// statusClientClosedRequest is the conventional non-standard status for a
// downstream client that disconnected before the stream completed. The stream
// response is already committed, so the status is diagnostic only.
const statusClientClosedRequest = 499

// streamIdleTimeout returns the idle deadline applied between upstream chunks.
func streamIdleTimeout(cfg config.Config) time.Duration {
	if cfg.RequestTimeout > 0 {
		return cfg.RequestTimeout
	}
	return 5 * time.Minute
}

// streamConverter is the common shape of the three adapters' stream
// converters: feed one upstream SSE chunk, get translated client events.
type streamConverter interface {
	Feed(chunk []byte) (events [][]byte, done bool, eErr *errclass.Error)
}

// Compile-time proof the close-without-terminal Flush seam (F5) picks up
// both converters that can hold a deferred terminal.
var (
	_ interface{ Flush() [][]byte } = (*chatcompletions.StreamConverter)(nil)
	_ interface{ Flush() [][]byte } = (*messages.StreamConverter)(nil)
)

func newStreamConverter(route catalog.Route, sourceFormat string, tools ...*shared.ResponseTools) streamConverter {
	switch route {
	case catalog.RouteMessages:
		return messages.NewStreamConverter(sourceFormat, tools...)
	case catalog.RouteResponses:
		return responses.NewStreamConverter(sourceFormat, tools...)
	}
	return chatcompletions.NewStreamConverter(sourceFormat, tools...)
}

// handleExecuteStream implements executor.execute_stream (FR-006, §7).
// It decodes once and delegates to executeStream so a stream-flagged
// request arriving via executor.execute is never parsed twice.
func (m *Manager) handleExecuteStream(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed executor request body"), nil
	}
	debugTrace("executor stream invoked model=%s source_format=%s stream=%t original_body_%s payload_%s", req.Model, req.SourceFormat, req.Stream, debugBodyMeta(req.OriginalRequest), debugBodyMeta(req.Payload))
	return m.executeStream(req)
}

// executeStream runs the already-decoded stream execution: pre-first-byte errors
// (invalid request, unroutable model, upstream HTTP >=400) return immediately as
// error envelopes so CPA can failover pre-emission.
// On success (upstream HTTP 200 OK), the reading and emitting pump loop runs in
// a background goroutine and executeStream returns okEnvelope immediately so the
// host can start draining chunks to the downstream client without buffer deadlocks.
func (m *Manager) executeStream(req executorRequest) ([]byte, error) {
	res, failEnv := m.resolveExecution(req)
	if res == nil {
		return failEnv, nil
	}
	sessionID, eErr := resolveOpenCodeSessionID(req)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	decision, routeFail := m.resolveRoute(res, req.SourceFormat)
	if routeFail != nil {
		return routeFail, nil
	}
	debugTrace("executor session mode=%s source_format=%s x_opencode_session=%s fallback=%t", "stream", req.SourceFormat, sessionID, sessionID == emptyOpenCodeSessionID)
	upstreamBody, eErr := buildUpstreamRequest(decision.UpstreamProtocol, res.rec.UpstreamID, req.SourceFormat, req.OriginalRequest, res.rec.Thinking, &res.tools)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}

	url := decision.UpstreamURL
	if url == "" {
		url = catalog.JoinUpstreamURL(res.cfg.BaseURL, res.rec.EndpointPath)
	}
	debugTrace("executor sending stream url=%s body_len=%d", url, len(upstreamBody))
	startTimeout := res.cfg.RequestTimeout
	if startTimeout <= 0 || startTimeout > streamStartTimeout {
		startTimeout = streamStartTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	st, stHeaders, id, err := m.bridge.DoStream(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     url,
		Headers: upstreamAuthHeaders(decision.UpstreamProtocol, res.key, sessionID, req.Headers.Get("User-Agent")),
		Body:    upstreamBody,
	})
	debugTrace("executor stream DoStream status=%d upstreamID=%s err=%v", st, id, err)
	if err != nil {
		return classEnvelope(errclass.FromNetwork(err)), nil
	}
	if st >= 400 {
		var body []byte
		if id != "" {
			watchdog := time.AfterFunc(res.cfg.RequestTimeout, func() {
				_ = m.bridge.StreamClose(id)
			})
			body, _, _, _ = m.bridge.StreamRead(id)
			watchdog.Stop()
			_ = m.bridge.StreamClose(id)
		}
		return classEnvelope(shared.UpstreamStatusError(st, body, stHeaders)), nil
	}

	downID := req.StreamID
	if m.bridge != nil {
		m.bridge.inFlight.Add(1)
	}
	go func() {
		if m.bridge != nil {
			defer m.bridge.inFlight.Done()
		}
		defer m.recoverBackground("stream pump")
		m.pumpStream(downID, id, res, req.SourceFormat, decision.UpstreamProtocol)
	}()
	return okEnvelope(struct{}{}), nil
}

func (m *Manager) pumpStream(downID, upstreamID string, res *resolvedExecution, sourceFormat string, upstreamProtocol catalog.Route) {
	var closeOnce sync.Once
	closeWith := func(errMsg string, detail *pluginabi.Error) {
		closeOnce.Do(func() {
			_ = m.bridge.StreamClose(upstreamID)
			if detail != nil {
				_ = m.bridge.StreamCloseDownstreamWithDetail(downID, *detail)
				return
			}
			_ = m.bridge.StreamCloseDownstream(downID, errMsg)
		})
	}
	closeStreams := func(errMsg string) { closeWith(errMsg, nil) }
	// closeWithCode attaches a host-recognized code to a stream failure so CPA
	// can classify it for cooldown/retry decisions. status stays non-zero
	// because a zero HTTPStatus is surfaced as 500.
	closeWithCode := func(code, errMsg string, status int) {
		msg := errclass.Redact(errMsg)
		closeWith(msg, &pluginabi.Error{Code: code, Message: msg, HTTPStatus: status})
	}
	// truncated ends a stream that the upstream accepted but did not complete.
	// The generation was already submitted, so the host must skip credential
	// cooldown and must not replay the request on another credential.
	truncated := func(errMsg string, status int) {
		closeWithCode(errclass.CodeStreamTruncated, errMsg, status)
	}
	defer closeStreams("")

	var aborted atomic.Bool
	idleTimeout := streamIdleTimeout(res.cfg)
	watchdog := time.AfterFunc(idleTimeout, func() {
		defer func() {
			if r := recover(); r != nil && m.bridge != nil {
				_ = m.bridge.Log("error", "stream watchdog panicked", nil)
			}
		}()
		aborted.Store(true)
		_ = m.bridge.StreamClose(upstreamID)
	})
	defer watchdog.Stop()

	conv := newStreamConverter(upstreamProtocol, sourceFormat, &res.tools)
	var (
		total          int64
		upstreamClosed bool
		convDone       bool
	)
	for {
		payload, readErrMsg, closed, err := m.bridge.StreamRead(upstreamID)
		debugTrace("executor stream read chunk_len=%d closed=%t readErrMsg=%q err=%v", len(payload), closed, readErrMsg, err)
		upstreamClosed = closed
		if aborted.Load() {
			truncated("stream exceeded request-timeout", http.StatusGatewayTimeout)
			return
		}
		if err != nil {
			truncated(err.Error(), http.StatusBadGateway)
			return
		}
		if readErrMsg != "" {
			truncated(readErrMsg, http.StatusBadGateway)
			return
		}
		// Reset the idle watchdog on every upstream chunk: long but active
		// streams must not be cut by a total-duration cap.
		if len(payload) > 0 {
			watchdog.Reset(idleTimeout)
		}
		total += int64(len(payload))
		if total > res.cfg.MaxResponseBytes {
			truncated("stream exceeded max-response-bytes", http.StatusBadGateway)
			return
		}
		events, done, convErr := conv.Feed(payload)
		if convErr != nil {
			closeWithCode(errclass.CodeRequestScoped, convErr.Message, http.StatusBadRequest)
			return
		}
		if emitErr := m.emitAll(downID, events); emitErr != nil {
			// Downstream consumer gone or not draining: a client/session
			// lifecycle failure, never a credential fault.
			closeWithCode(errclass.CodeConnectionLifecycle, emitErr.Error(), statusClientClosedRequest)
			return
		}
		convDone = done
		if convDone || upstreamClosed {
			break
		}
	}
	debugTrace("executor stream loop end convDone=%t upstreamClosed=%t", convDone, upstreamClosed)
	if !convDone && upstreamClosed {
		if flusher, ok := conv.(interface{ Flush() [][]byte }); ok {
			flushed := flusher.Flush()
			if emitErr := m.emitAll(downID, flushed); emitErr != nil {
				closeStreams(errclass.Redact(emitErr.Error()))
				return
			}
		}
	}
}

// emitAll feeds converted events downstream in order. StreamEmit is
// deadline-bounded (emitTimeout), so a host that stops draining the
// downstream stream surfaces here as an error; the pump loop treats that
// as a post-first-byte stream-fatal failure and runs fail(), whose
// Once-closer releases both streams.
func (m *Manager) emitAll(downStreamID string, events [][]byte) error {
	for _, evt := range events {
		if err := m.bridge.StreamEmit(downStreamID, evt); err != nil {
			return err
		}
	}
	return nil
}
