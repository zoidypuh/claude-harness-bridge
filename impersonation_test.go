package impersonation

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestSanitizeAnthropicMessagesOAuthToolAndSystem(t *testing.T) {
	body := []byte(`{
		"system":"# Harness\nIf asked for marker, reply MARKER.",
		"betas":["custom-beta"],
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"browser_navigate","description":"Open a URL","input_schema":{"type":"object","properties":{"session_id":{"type":"string","description":"session"}},"required":["session_id","missing",7]}}]
	}`)

	out, state, err := SanitizeAnthropicMessages(body, Options{
		OAuth:     true,
		SessionID: "11111111-1111-4111-8111-111111111111",
	})
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}

	if len(state.ExtraBetas) != 1 || state.ExtraBetas[0] != "custom-beta" {
		t.Fatalf("ExtraBetas = %#v", state.ExtraBetas)
	}
	if name := pathString(got, "tools", 0, "name"); name != "BrowserNavigate" {
		t.Fatalf("tool name = %q", name)
	}
	if desc := pathString(got, "tools", 0, "description"); desc != "" {
		t.Fatalf("tool description = %q, want stripped", desc)
	}
	if _, ok := pathMap(got, "tools", 0, "input_schema", "properties", "thread_id"); !ok {
		t.Fatalf("session_id property was not renamed: %s", string(out))
	}
	required, _ := pathValue(got, "tools", 0, "input_schema", "required").([]any)
	if len(required) != 1 || required[0] != "thread_id" {
		t.Fatalf("tool required = %#v, want only thread_id", required)
	}
	if text := pathString(got, "system", 0, "text"); !strings.HasPrefix(text, "x-anthropic-billing-header:") || strings.Contains(text, "cch=00000") {
		t.Fatalf("billing block was not signed: %q", text)
	}
	if text := pathString(got, "system", 3, "text"); !strings.Contains(text, "MARKER") {
		t.Fatalf("short forwarded system prompt was not preserved: %q", text)
	}
	if typ := pathString(got, "thinking", "type"); typ != "adaptive" {
		t.Fatalf("thinking.type = %q", typ)
	}
	if effort := pathString(got, "output_config", "effort"); effort != "medium" {
		t.Fatalf("output_config.effort = %q", effort)
	}
	if userID := pathString(got, "metadata", "user_id"); !ValidUserID(userID) {
		t.Fatalf("invalid fake user_id: %q", userID)
	}

	response := []byte(`{"content":[{"type":"tool_use","name":"BrowserNavigate","id":"toolu_1","input":{}}]}`)
	reversed, err := ReverseAnthropicMessages(response, state)
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal(reversed, &resp); err != nil {
		t.Fatal(err)
	}
	if name := pathString(resp, "content", 0, "name"); name != "browser_navigate" {
		t.Fatalf("reversed tool name = %q", name)
	}
}

func TestSanitizeAnthropicMessagesForcedToolChoiceDisablesThinking(t *testing.T) {
	for _, toolChoice := range []string{
		`{"type":"any"}`,
		`{"type":"tool","name":"browser_navigate"}`,
	} {
		body := []byte(`{
			"model":"claude-sonnet-4-6",
			"thinking":{"type":"adaptive"},
			"output_config":{"effort":"high"},
			"messages":[{"role":"user","content":"open example"}],
			"tools":[{"name":"browser_navigate","input_schema":{"type":"object","properties":{"url":{"type":"string"}}}}],
			"tool_choice":` + toolChoice + `
		}`)

		out, _, err := SanitizeAnthropicMessages(body, Options{OAuth: true})
		if err != nil {
			t.Fatal(err)
		}

		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if thinking := pathValue(got, "thinking"); thinking != nil {
			t.Fatalf("thinking = %#v, want removed for forced tool_choice %s", thinking, toolChoice)
		}
		if effort := pathValue(got, "output_config", "effort"); effort != nil {
			t.Fatalf("output_config.effort = %#v, want removed for forced tool_choice %s", effort, toolChoice)
		}
		if contextManagement := pathValue(got, "context_management"); contextManagement != nil {
			t.Fatalf("context_management = %#v, want removed when thinking is removed for forced tool_choice", contextManagement)
		}
		if pathString(got, "tool_choice", "type") == "tool" {
			if name := pathString(got, "tool_choice", "name"); name != "BrowserNavigate" {
				t.Fatalf("tool_choice name = %q, want sanitized", name)
			}
		}
	}
}

func TestSanitizeAnthropicMessagesObfuscatesDefaultPromptMarkers(t *testing.T) {
	body := []byte(`{
		"system":"HERMES AGENT OpenClaw instructions from soul.md",
		"messages":[{"role":"user","content":[{"type":"text","text":"read /home/user/.hermes and .openclaw state"}]}]
	}`)

	out, _, err := SanitizeAnthropicMessages(body, Options{OAuth: true})
	if err != nil {
		t.Fatal(err)
	}

	text := string(out)
	for _, marker := range []string{"HERMES AGENT", "OpenClaw", "soul.md", ".hermes", ".openclaw"} {
		if strings.Contains(text, marker) {
			t.Fatalf("expected %q to be obfuscated in %s", marker, text)
		}
	}
	if !strings.Contains(text, "\u200b") {
		t.Fatalf("expected zero-width obfuscation in %s", text)
	}
}

func TestObfuscateDefaultPromptMarkersPreservesThinkingBlocks(t *testing.T) {
	root := map[string]any{
		"content": []any{
			map[string]any{"type": "thinking", "thinking": "hermes openclaw soul.md stays"},
			map[string]any{"type": "redacted_thinking", "data": "hermes openclaw stays too"},
			map[string]any{"type": "text", "text": "hermes openclaw changes"},
		},
	}

	obfuscateDefaultPromptMarkers(root)
	out, err := marshalJSON(root)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Contains(out, []byte(`"thinking":"hermes openclaw soul.md stays"`)) {
		t.Fatalf("thinking block changed: %s", string(out))
	}
	if !bytes.Contains(out, []byte(`"data":"hermes openclaw stays too"`)) {
		t.Fatalf("redacted thinking block changed: %s", string(out))
	}
	if bytes.Contains(out, []byte(`"text":"hermes openclaw changes"`)) {
		t.Fatalf("text block was not obfuscated: %s", string(out))
	}
}

func TestSanitizeAnthropicMessagesCustomToolSchema(t *testing.T) {
	body := []byte(`{
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"type":"custom",
			"custom":{
				"name":"browser_navigate",
				"description":"Open a URL",
				"input_schema":{
					"$schema":"http://json-schema.org/draft-07/schema#",
					"type":["object","object"],
					"properties":{
						"session_id":{"type":"string","format":"uuid","description":"session"},
						"url":{"type":["string","null"],"description":"target"},
						"bad":true
					},
					"required":["session_id","url","missing",7],
					"exclusiveMinimum":true
				}
			}
		}]
	}`)

	out, state, err := SanitizeAnthropicMessages(body, Options{OAuth: true})
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}

	if name := pathString(got, "tools", 0, "custom", "name"); name != "BrowserNavigate" {
		t.Fatalf("custom tool name = %q", name)
	}
	if typ := pathString(got, "tools", 0, "type"); typ != "custom" {
		t.Fatalf("tool type = %q", typ)
	}
	if desc := pathString(got, "tools", 0, "custom", "description"); desc != "" {
		t.Fatalf("custom tool description = %q, want stripped", desc)
	}
	if _, ok := pathMap(got, "tools", 0, "custom", "input_schema", "properties", "thread_id"); !ok {
		t.Fatalf("custom session_id property was not renamed: %s", string(out))
	}
	if _, ok := pathMap(got, "tools", 0, "custom", "input_schema", "properties", "url"); !ok {
		t.Fatalf("custom url property missing: %s", string(out))
	}
	if bad := pathValue(got, "tools", 0, "custom", "input_schema", "properties", "bad"); bad != true {
		t.Fatalf("boolean property schema changed unexpectedly: %#v", bad)
	}
	if schema := pathValue(got, "tools", 0, "custom", "input_schema", "$schema"); schema != nil {
		t.Fatalf("$schema was not stripped: %#v", schema)
	}
	if format := pathValue(got, "tools", 0, "custom", "input_schema", "properties", "thread_id", "format"); format != nil {
		t.Fatalf("format was not stripped: %#v", format)
	}
	if exclusive := pathValue(got, "tools", 0, "custom", "input_schema", "exclusiveMinimum"); exclusive != nil {
		t.Fatalf("boolean exclusiveMinimum was not stripped: %#v", exclusive)
	}
	required, _ := pathValue(got, "tools", 0, "custom", "input_schema", "required").([]any)
	if len(required) != 2 || required[0] != "thread_id" || required[1] != "url" {
		t.Fatalf("custom required = %#v, want thread_id and url", required)
	}
	if original := state.ReverseToolNames["BrowserNavigate"]; original != "browser_navigate" {
		t.Fatalf("reverse custom tool name = %q", original)
	}
}

func TestSanitizeAnthropicMessagesDoesNotCorruptDescriptionPropertySchema(t *testing.T) {
	body := []byte(`{
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"name":"question",
			"description":"Ask a question",
			"input_schema":{
				"type":"object",
				"properties":{
					"description":{"description":"Question description","type":"string"}
				},
				"required":["description"]
			}
		}]
	}`)

	out, _, err := SanitizeAnthropicMessages(body, Options{OAuth: true})
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if desc := pathString(got, "tools", 0, "description"); desc != "" {
		t.Fatalf("tool description = %q, want stripped", desc)
	}
	if typ := pathString(got, "tools", 0, "input_schema", "properties", "description", "type"); typ != "string" {
		t.Fatalf("description property type = %q, want string; body: %s", typ, string(out))
	}
	if desc := pathString(got, "tools", 0, "input_schema", "properties", "description", "description"); desc != "" {
		t.Fatalf("description property annotation = %q, want stripped", desc)
	}
}

func TestApplyAnthropicHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-Client-Request-Id", "third-party")
	h.Set("Sec-Fetch-Mode", "cors")

	session := ApplyAnthropicHeaders(h, HeaderOptions{
		APIKey:              "sk-ant-oat01-test",
		UpstreamIsAnthropic: false,
		ExtraBetas:          []string{"custom-beta"},
		SessionID:           "22222222-2222-4222-8222-222222222222",
	})

	if session != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("session = %q", session)
	}
	if got := h.Get("Authorization"); got != "Bearer sk-ant-oat01-test" {
		t.Fatalf("Authorization = %q", got)
	}
	if strings.Contains(h.Get("Anthropic-Beta"), "claude-code-20250219") {
		t.Fatalf("third-party upstream should not receive claude-code beta: %q", h.Get("Anthropic-Beta"))
	}
	if !strings.Contains(h.Get("Anthropic-Beta"), "custom-beta") {
		t.Fatalf("extra beta missing: %q", h.Get("Anthropic-Beta"))
	}
	if got := h.Get("X-App"); got != "" {
		t.Fatalf("X-App = %q, want empty for third-party upstream", got)
	}
	if got := h.Get("X-Client-Request-Id"); got != "" {
		t.Fatalf("X-Client-Request-Id = %q, want stripped", got)
	}
	if got := h.Get("User-Agent"); got != DefaultClaudeUserAgent {
		t.Fatalf("User-Agent = %q", got)
	}

	h2 := http.Header{}
	ApplyAnthropicHeaders(h2, HeaderOptions{APIKey: "sk-ant-api03-test", UpstreamIsAnthropic: true})
	if got := h2.Get("x-api-key"); got != "sk-ant-api03-test" {
		t.Fatalf("x-api-key = %q", got)
	}
	if got := h2.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want empty for Anthropic API key", got)
	}
	if got := h2.Get("X-App"); got != "cli" {
		t.Fatalf("X-App = %q", got)
	}
}

func TestTextReplacementsPreserveThinkingBlocks(t *testing.T) {
	body := []byte(`{"content":[{"type":"thinking","thinking":"OpenClaw stays {\"nested\":true}","signature":"sig"},{"type":"redacted_thinking","data":"OpenClaw stays too"},{"type":"text","text":"OpenClaw changes"}]}`)
	out := ApplyTextReplacementsPreservingThinking(body, []TextReplacement{{Find: "OpenClaw", Replace: "OCPlatform"}})
	if !bytes.Contains(out, []byte(`{"type":"thinking","thinking":"OpenClaw stays {\"nested\":true}","signature":"sig"}`)) {
		t.Fatalf("thinking block changed: %s", string(out))
	}
	if !bytes.Contains(out, []byte(`{"type":"redacted_thinking","data":"OpenClaw stays too"}`)) {
		t.Fatalf("redacted thinking block changed: %s", string(out))
	}
	if !bytes.Contains(out, []byte(`"text":"OCPlatform changes"`)) {
		t.Fatalf("text block was not replaced: %s", string(out))
	}
}

func TestReverseAnthropicSSELine(t *testing.T) {
	state := Result{ReverseToolNames: map[string]string{"Terminal": "terminal"}}
	line := []byte(`data: {"type":"content_block_start","content_block":{"type":"tool_use","name":"Terminal","id":"toolu_1"}}`)
	out := ReverseAnthropicSSELine(line, state)
	if !bytes.Contains(out, []byte(`"name":"terminal"`)) {
		t.Fatalf("line was not reversed: %s", string(out))
	}
}

func pathString(root map[string]any, path ...any) string {
	value := pathValue(root, path...)
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func pathMap(root map[string]any, path ...any) (map[string]any, bool) {
	value := pathValue(root, path...)
	m, ok := value.(map[string]any)
	return m, ok
}

func pathValue(value any, path ...any) any {
	for _, elem := range path {
		switch key := elem.(type) {
		case string:
			m, _ := value.(map[string]any)
			value = m[key]
		case int:
			a, _ := value.([]any)
			if key < 0 || key >= len(a) {
				return nil
			}
			value = a[key]
		}
	}
	return value
}
