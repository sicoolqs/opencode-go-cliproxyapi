package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const idleStreamYAML = testValidYAML + "request-timeout: 300ms\n"

// activeStreamResponder serves a chat-completions stream with a delay between
// chunks, so the total stream lifetime exceeds request-timeout while the gap
// between chunks stays under it.
func activeStreamResponder(chunks int, gap time.Duration) func(string, []byte) ([]byte, error) {
	i := 0
	return wrapWithCatalog(multiRouteCatalog, func(method string, payload []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostHTTPDo:
			var wire struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(payload, &wire)
			if strings.HasSuffix(wire.URL, "/usage") {
				return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"usage":{}}`)}), nil
			}
			return hostOK(map[string]any{}), nil
		case pluginabi.MethodHostHTTPDoStream:
			return hostOK(hostStreamStartResp{StatusCode: http.StatusOK, StreamID: "up-active"}), nil
		case pluginabi.MethodHostHTTPStreamRead:
			if i >= chunks {
				return hostOK(hostStreamReadResp{Done: true}), nil
			}
			i++
			time.Sleep(gap)
			return hostOK(hostStreamReadResp{Payload: []byte(
				`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"x"}}]}` + "\n\n")}), nil
		default:
			return hostOK(map[string]any{}), nil
		}
	})
}

// A long but constantly active stream must survive request-timeout: the
// watchdog is idle-based, not a total-duration cap.
func TestExecuteStreamIdleWatchdogResetsOnActivity(t *testing.T) {
	f := &fakeCaller{responder: activeStreamResponder(6, 100*time.Millisecond)}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(idleStreamYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}

	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/glm-5.3", "claude", []byte(ccRequestBody), "down-active"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("envelope = %+v", env.Error)
	}
	m.bridge.WaitForInFlight(10 * time.Second)

	if got := len(f.callsOf(pluginabi.MethodHostStreamEmit)); got == 0 {
		t.Fatal("active stream emitted nothing")
	}
	closes := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(closes) != 1 {
		t.Fatalf("downstream closes = %d, want 1", len(closes))
	}
	if strings.Contains(string(closes[0].payload), "error_detail") {
		t.Fatalf("clean completion must not carry an error detail: %s", closes[0].payload)
	}
	if strings.Contains(string(closes[0].payload), `"error":"`) {
		t.Fatalf("active stream closed with error: %s", closes[0].payload)
	}
}

// A stream that never produces headers within the start timeout surfaces a
// retryable 504 rather than an opaque 500.
func TestExecuteStreamFirstByteTimeoutIs504(t *testing.T) {
	f := &fakeCaller{responder: wrapWithCatalog(multiRouteCatalog, func(method string, payload []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostHTTPDo:
			var wire struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(payload, &wire)
			if strings.HasSuffix(wire.URL, "/usage") {
				return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"usage":{}}`)}), nil
			}
			return hostOK(map[string]any{}), nil
		case pluginabi.MethodHostHTTPDoStream:
			// Never answer within the start deadline.
			time.Sleep(400 * time.Millisecond)
			return hostOK(hostStreamStartResp{StatusCode: http.StatusOK, StreamID: "up-late"}), nil
		default:
			return hostOK(map[string]any{}), nil
		}
	})}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML+"request-timeout: 120ms\n")); err != nil {
		t.Fatalf("register: %v", err)
	}

	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/glm-5.3", "claude", []byte(ccRequestBody), "down-late"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if env.OK || env.Error == nil {
		t.Fatalf("envelope = %+v, want timeout error", env)
	}
	if env.Error.HTTPStatus != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", env.Error.HTTPStatus)
	}
	if !env.Error.Retryable {
		t.Fatalf("timeout must be retryable: %+v", env.Error)
	}
	m.bridge.WaitForInFlight(10 * time.Second)
}
