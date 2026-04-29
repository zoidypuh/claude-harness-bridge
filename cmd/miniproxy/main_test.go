package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMiniProxyOpenAIPathSanitizesAndReversesTools(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if got := getString(body["model"]); got != "gpt-test" {
			t.Fatalf("model = %q", got)
		}
		tools := anySlice(body["tools"])
		if len(tools) != 1 {
			t.Fatalf("tools length = %d", len(tools))
		}
		tool, _ := tools[0].(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		if got := getString(fn["name"]); got != "BrowserNavigate" {
			t.Fatalf("sanitized tool name = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl_test",
			"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"BrowserNavigate","arguments":"{\"url\":\"https://example.com\"}"}}]}}],
			"usage":{"prompt_tokens":12,"completion_tokens":3}
		}`))
	}))
	defer upstream.Close()

	cfg := config{
		upstreamBaseURL: upstream.URL + "/v1",
		mode:            "openai-proxy",
		upstreamModel:   "gpt-test",
		oauthShape:      true,
		signCCH:         true,
		addFakeUserID:   true,
	}
	body := `{
		"model":"claude-sonnet-4-5-20250929",
		"messages":[{"role":"user","content":"open example"}],
		"tools":[{"name":"browser_navigate","description":"Open URL","input_schema":{"type":"object","properties":{"url":{"type":"string"}}}}],
		"tool_choice":{"type":"tool","name":"browser_navigate"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rr := httptest.NewRecorder()

	cfg.handleMessages(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	content := anySlice(resp["content"])
	if len(content) != 1 {
		t.Fatalf("content length = %d", len(content))
	}
	block, _ := content[0].(map[string]any)
	if got := getString(block["name"]); got != "browser_navigate" {
		t.Fatalf("response tool name = %q", got)
	}
	if got := getString(resp["stop_reason"]); got != "tool_use" {
		t.Fatalf("stop_reason = %q", got)
	}
}

func TestMiniProxyDirectAnthropicDecodesGzipAndReversesTools(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if got := pathString(body, "tools", 0, "name"); got != "BrowserNavigate" {
			t.Fatalf("sanitized tool name = %q", got)
		}
		if pathValue(body, "thinking") != nil {
			t.Fatalf("thinking should be stripped for forced tool_choice: %#v", body["thinking"])
		}
		if pathValue(body, "context_management") != nil {
			t.Fatalf("context_management should be stripped for forced tool_choice: %#v", body["context_management"])
		}

		resp := []byte(`{"id":"msg_test","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_1","name":"BrowserNavigate","input":{"url":"https://example.com"}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":5}}`)
		var compressed bytes.Buffer
		gz := gzip.NewWriter(&compressed)
		if _, err := gz.Write(resp); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(compressed.Bytes())
	}))
	defer upstream.Close()

	cfg := config{
		mode:             "direct-anthropic",
		anthropicBaseURL: upstream.URL + "/v1",
		anthropicAPIKey:  "sk-ant-oat01-test",
		oauthShape:       true,
		signCCH:          true,
		addFakeUserID:    true,
	}
	body := `{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":"open example"}],
		"tools":[{"name":"browser_navigate","description":"Open URL","input_schema":{"type":"object","properties":{"url":{"type":"string"}}}}],
		"tool_choice":{"type":"tool","name":"browser_navigate"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rr := httptest.NewRecorder()

	cfg.handleMessages(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want stripped", got)
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if got := pathString(resp, "content", 0, "name"); got != "browser_navigate" {
		t.Fatalf("response tool name = %q, want reversed", got)
	}
}

func TestAnthropicToOpenAIChatUsesCustomToolFields(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":"open example"}],
		"tools":[{"type":"custom","custom":{"name":"BrowserNavigate","description":"","input_schema":{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}}}]
	}`)

	out, _, _, err := anthropicToOpenAIChat(body, "gpt-test")
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	tools := anySlice(got["tools"])
	if len(tools) != 1 {
		t.Fatalf("tools length = %d", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	if got := getString(fn["name"]); got != "BrowserNavigate" {
		t.Fatalf("function name = %q", got)
	}
	parameters, _ := fn["parameters"].(map[string]any)
	if _, ok := parameters["properties"].(map[string]any); !ok {
		t.Fatalf("parameters = %#v", parameters)
	}
}

func TestDirectAnthropicModelsFallbackUsesAnthropicModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "models unavailable", http.StatusBadGateway)
	}))
	defer upstream.Close()

	cfg := config{
		mode:             "direct-anthropic",
		anthropicBaseURL: upstream.URL + "/v1",
		anthropicAPIKey:  "sk-ant-oat01-test",
		anthropicModel:   "claude-sonnet-4-6",
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()

	cfg.handleModels(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "claude-sonnet-4-6") {
		t.Fatalf("fallback model response = %s", rr.Body.String())
	}
}

func TestNormalizeModeAliases(t *testing.T) {
	cases := map[string]string{
		"anthropic":           "direct-anthropic",
		"direct-to-anthropic": "direct-anthropic",
		"openai":              "openai-proxy",
		"upstream-proxy":      "openai-proxy",
	}
	for input, want := range cases {
		if got := normalizeMode(input); got != want {
			t.Fatalf("normalizeMode(%q) = %q, want %q", input, got, want)
		}
	}
}

func pathString(root map[string]any, path ...any) string {
	return getString(pathValue(root, path...))
}

func pathValue(value any, path ...any) any {
	current := value
	for _, part := range path {
		switch p := part.(type) {
		case string:
			obj, _ := current.(map[string]any)
			current = obj[p]
		case int:
			arr, _ := current.([]any)
			if p < 0 || p >= len(arr) {
				return nil
			}
			current = arr[p]
		}
	}
	return current
}
