package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	impersonation "github.com/zoidypuh/claude-code-impersonation"
)

type config struct {
	listenAddr       string
	mode             string
	upstreamBaseURL  string
	upstreamAPIKey   string
	upstreamModel    string
	anthropicBaseURL string
	anthropicAPIKey  string
	anthropicModel   string
	oauthShape       bool
	signCCH          bool
	addFakeUserID    bool
	corePrompt       string
	debugDumpDir     string
}

func main() {
	cfg := loadConfig()
	switch cfg.mode {
	case "openai-proxy":
		if cfg.upstreamBaseURL == "" {
			log.Fatal("UPSTREAM_BASE_URL is required in openai-proxy mode, for example http://127.0.0.1:4000/v1")
		}
	case "direct-anthropic":
		if cfg.anthropicBaseURL == "" {
			log.Fatal("ANTHROPIC_BASE_URL_REAL is required in direct-anthropic mode, for example https://api.anthropic.com/v1")
		}
		if cfg.anthropicAPIKey == "" {
			log.Fatal("ANTHROPIC_API_KEY_REAL is required in direct-anthropic mode")
		}
	default:
		log.Fatalf("unknown mode %q; use direct-anthropic or openai-proxy", cfg.mode)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/v1/messages", cfg.handleMessages)
	mux.HandleFunc("/v1/messages/count_tokens", cfg.handleCountTokens)
	mux.HandleFunc("/v1/models", cfg.handleModels)

	server := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}

	log.Printf("listening on %s in %s mode, client Anthropic base URL is http://%s", cfg.listenAddr, cfg.mode, cfg.listenAddr)
	if cfg.mode == "direct-anthropic" {
		log.Printf("forwarding sanitized Anthropic /v1/messages to %s", cfg.anthropicBaseURL)
	} else {
		log.Printf("forwarding converted OpenAI /chat/completions to %s", cfg.upstreamBaseURL)
	}
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func loadConfig() config {
	modeFlag := flag.String("mode", "", "direct-anthropic or openai-proxy")
	listenFlag := flag.String("listen", "", "listen address, for example 127.0.0.1:8787")
	flag.Parse()

	mode := firstNonEmpty(*modeFlag, firstArg(), os.Getenv("MINIPROXY_MODE"), os.Getenv("MODE"), os.Getenv("UPSTREAM_TYPE"), "openai-proxy")
	mode = normalizeMode(mode)
	listenAddr := firstNonEmpty(*listenFlag, os.Getenv("LISTEN_ADDR"), "127.0.0.1:8787")

	return config{
		listenAddr:       listenAddr,
		mode:             mode,
		upstreamBaseURL:  strings.TrimRight(os.Getenv("UPSTREAM_BASE_URL"), "/"),
		upstreamAPIKey:   os.Getenv("UPSTREAM_API_KEY"),
		upstreamModel:    os.Getenv("UPSTREAM_MODEL"),
		anthropicBaseURL: strings.TrimRight(firstNonEmpty(os.Getenv("ANTHROPIC_BASE_URL_REAL"), os.Getenv("REAL_ANTHROPIC_BASE_URL"), os.Getenv("ANTHROPIC_UPSTREAM_BASE_URL"), os.Getenv("ANTHROPIC_REAL_BASE_URL"), "https://api.anthropic.com/v1"), "/"),
		anthropicAPIKey:  firstNonEmpty(os.Getenv("ANTHROPIC_API_KEY_REAL"), os.Getenv("ANTHROPIC_AUTH_TOKEN_REAL"), os.Getenv("ANTHROPIC_UPSTREAM_TOKEN"), os.Getenv("ANTHROPIC_AUTH_TOKEN"), os.Getenv("UPSTREAM_API_KEY")),
		anthropicModel:   firstNonEmpty(os.Getenv("ANTHROPIC_MODEL_REAL"), os.Getenv("ANTHROPIC_UPSTREAM_MODEL"), ""),
		oauthShape:       envBoolDefault("CLAUDE_CODE_OAUTH_SHAPE", true),
		signCCH:          envBoolDefault("SIGN_CCH", true),
		addFakeUserID:    envBoolDefault("ADD_FAKE_USER_ID", true),
		corePrompt:       os.Getenv("CORE_PROMPT"),
		debugDumpDir:     firstNonEmpty(os.Getenv("DEBUG_DUMP_DIR"), os.Getenv("MINIPROXY_DEBUG_DUMP_DIR")),
	}
}

func (cfg config) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	sessionID := strings.TrimSpace(r.Header.Get("X-Claude-Code-Session-Id"))
	if sessionID == "" {
		sessionID = impersonation.NewSessionID()
	}

	sanitized, state, err := impersonation.SanitizeAnthropicMessages(raw, impersonation.Options{
		OAuth:         cfg.oauthShape,
		SignCCH:       cfg.signCCH,
		AddFakeUserID: cfg.addFakeUserID,
		SessionID:     sessionID,
		CorePrompt:    cfg.corePrompt,
	})
	if err != nil {
		http.Error(w, "invalid Anthropic request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if state.SessionID == "" {
		state.SessionID = sessionID
	}
	cfg.dumpJSON("sanitized-anthropic-request", sanitized)

	switch cfg.mode {
	case "direct-anthropic":
		cfg.forwardAnthropic(w, r, sanitized, state)
	default:
		cfg.forwardOpenAI(w, r, sanitized, state)
	}
}

func (cfg config) forwardOpenAI(w http.ResponseWriter, r *http.Request, sanitized []byte, state impersonation.Result) {
	openAIReq, wantsStream, requestedModel, err := anthropicToOpenAIChat(sanitized, cfg.upstreamModel)
	if err != nil {
		http.Error(w, "could not convert Anthropic request to OpenAI chat completions: "+err.Error(), http.StatusBadRequest)
		return
	}
	cfg.dumpJSON("openai-request", openAIReq)

	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, cfg.url("/chat/completions"), bytes.NewReader(openAIReq))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	if cfg.upstreamAPIKey != "" {
		upstreamReq.Header.Set("Authorization", "Bearer "+cfg.upstreamAPIKey)
	}

	resp, err := http.DefaultClient.Do(upstreamReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	cfg.dumpJSON("openai-upstream-response", respBody)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
		return
	}

	anthropicResp, err := openAIChatToAnthropic(respBody, requestedModel, state)
	if err != nil {
		http.Error(w, "could not convert OpenAI response to Anthropic response: "+err.Error(), http.StatusBadGateway)
		return
	}
	cfg.dumpJSON("client-anthropic-response", anthropicResp)
	if wantsStream {
		writeAnthropicSSE(w, anthropicResp)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(anthropicResp)
}

func (cfg config) forwardAnthropic(w http.ResponseWriter, r *http.Request, sanitized []byte, state impersonation.Result) {
	if cfg.anthropicModel != "" {
		sanitized = forceJSONModel(sanitized, cfg.anthropicModel)
	}
	cfg.dumpJSON("anthropic-upstream-request", sanitized)
	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, cfg.anthropicURL("/messages"), bytes.NewReader(sanitized))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	impersonation.ApplyAnthropicHeaders(upstreamReq.Header, impersonation.HeaderOptions{
		APIKey:              cfg.anthropicAPIKey,
		UpstreamIsAnthropic: strings.Contains(cfg.anthropicBaseURL, "api.anthropic.com"),
		ExtraBetas:          state.ExtraBetas,
		SessionID:           state.SessionID,
	})
	upstreamReq.Header.Set("Accept-Encoding", "gzip")

	resp, err := http.DefaultClient.Do(upstreamReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		forwardAnthropicSSE(w, resp.Body, state)
		return
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	cfg.dumpJSON("anthropic-upstream-response", respBody)
	respBody, err = decodeResponseBody(respBody, resp.Header.Get("Content-Encoding"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		respBody, err = impersonation.ReverseAnthropicMessages(respBody, state)
		if err != nil {
			http.Error(w, "could not reverse Anthropic response: "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	cfg.dumpJSON("client-anthropic-response", respBody)
	copyHeader(w.Header(), resp.Header)
	w.Header().Del("Content-Encoding")
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

func (cfg config) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tokens := roughTokenCount(raw)
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"input_tokens":%d}`+"\n", tokens)
}

func (cfg config) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if cfg.mode == "openai-proxy" {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, cfg.url("/models"), nil)
		if err == nil && cfg.upstreamAPIKey != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.upstreamAPIKey)
		}
		if err == nil {
			if resp, errDo := http.DefaultClient.Do(req); errDo == nil {
				defer resp.Body.Close()
				copyHeader(w.Header(), resp.Header)
				w.WriteHeader(resp.StatusCode)
				_, _ = io.Copy(w, resp.Body)
				return
			}
		}
	} else {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, cfg.anthropicURL("/models"), nil)
		if err == nil {
			impersonation.ApplyAnthropicHeaders(req.Header, impersonation.HeaderOptions{
				APIKey:              cfg.anthropicAPIKey,
				UpstreamIsAnthropic: strings.Contains(cfg.anthropicBaseURL, "api.anthropic.com"),
				SessionID:           impersonation.NewSessionID(),
			})
			if resp, errDo := http.DefaultClient.Do(req); errDo == nil {
				defer resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					copyHeader(w.Header(), resp.Header)
					w.WriteHeader(resp.StatusCode)
					_, _ = io.Copy(w, resp.Body)
					return
				}
			}
		}
	}
	model := firstNonEmpty(cfg.upstreamModel, cfg.anthropicModel)
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"data":[{"id":%q,"type":"model","owned_by":"proxy"}]}`+"\n", model)
}

func anthropicToOpenAIChat(body []byte, modelOverride string) ([]byte, bool, string, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, false, "", err
	}

	model := getString(root["model"])
	if strings.TrimSpace(modelOverride) != "" {
		model = strings.TrimSpace(modelOverride)
	}
	if model == "" {
		return nil, false, "", fmt.Errorf("request model is empty and UPSTREAM_MODEL is not set")
	}

	stream := getBool(root["stream"])
	out := map[string]any{
		"model":    model,
		"messages": anthropicMessages(root),
		"stream":   false,
	}
	copyNumber(root, out, "temperature")
	copyNumber(root, out, "top_p")
	copyNumberAs(root, out, "max_tokens", "max_tokens")
	if stop, ok := root["stop_sequences"]; ok {
		out["stop"] = stop
	}
	if tools := anthropicTools(root["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	if choice := openAIToolChoice(root["tool_choice"]); choice != nil {
		out["tool_choice"] = choice
	}

	encoded, err := json.Marshal(out)
	return encoded, stream, model, err
}

func anthropicMessages(root map[string]any) []any {
	var messages []any
	if systemText := strings.Join(extractText(root["system"]), "\n\n"); strings.TrimSpace(systemText) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": systemText})
	}
	for _, raw := range anySlice(root["messages"]) {
		msg, _ := raw.(map[string]any)
		role := getString(msg["role"])
		content := msg["content"]
		if role == "" {
			continue
		}
		if role == "assistant" {
			messages = append(messages, anthropicAssistantMessage(content))
			continue
		}
		if role == "user" {
			messages = append(messages, anthropicUserMessages(content)...)
			continue
		}
		messages = append(messages, map[string]any{"role": role, "content": strings.Join(extractText(content), "\n\n")})
	}
	return messages
}

func anthropicAssistantMessage(content any) map[string]any {
	msg := map[string]any{"role": "assistant"}
	texts := extractText(content)
	var toolCalls []any
	for _, raw := range anySlice(content) {
		part, _ := raw.(map[string]any)
		if getString(part["type"]) != "tool_use" {
			continue
		}
		args, _ := json.Marshal(firstNonNil(part["input"], map[string]any{}))
		toolCalls = append(toolCalls, map[string]any{
			"id":   firstString(part["id"], "call_"+impersonation.NewSessionID()),
			"type": "function",
			"function": map[string]any{
				"name":      getString(part["name"]),
				"arguments": string(args),
			},
		})
	}
	msg["content"] = strings.Join(texts, "\n\n")
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	return msg
}

func anthropicUserMessages(content any) []any {
	parts, ok := content.([]any)
	if !ok {
		return []any{map[string]any{"role": "user", "content": strings.Join(extractText(content), "\n\n")}}
	}
	var out []any
	var normalParts []any
	for _, raw := range parts {
		part, _ := raw.(map[string]any)
		switch getString(part["type"]) {
		case "tool_result":
			if len(normalParts) > 0 {
				out = append(out, map[string]any{"role": "user", "content": simplifyOpenAIContent(normalParts)})
				normalParts = nil
			}
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": getString(part["tool_use_id"]),
				"content":      strings.Join(extractText(part["content"]), "\n\n"),
			})
		case "image":
			if imagePart := openAIImagePart(part); imagePart != nil {
				normalParts = append(normalParts, imagePart)
			}
		default:
			for _, text := range extractText(part) {
				if strings.TrimSpace(text) != "" {
					normalParts = append(normalParts, map[string]any{"type": "text", "text": text})
				}
			}
		}
	}
	if len(normalParts) > 0 {
		out = append(out, map[string]any{"role": "user", "content": simplifyOpenAIContent(normalParts)})
	}
	if len(out) == 0 {
		out = append(out, map[string]any{"role": "user", "content": ""})
	}
	return out
}

func openAIImagePart(part map[string]any) map[string]any {
	source, _ := part["source"].(map[string]any)
	if source == nil {
		return nil
	}
	switch getString(source["type"]) {
	case "url":
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": getString(source["url"])}}
	case "base64":
		mediaType := firstString(source["media_type"], "image/png")
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + mediaType + ";base64," + getString(source["data"])}}
	default:
		return nil
	}
}

func simplifyOpenAIContent(parts []any) any {
	if len(parts) == 1 {
		part, _ := parts[0].(map[string]any)
		if getString(part["type"]) == "text" {
			return getString(part["text"])
		}
	}
	return parts
}

func anthropicTools(raw any) []any {
	var out []any
	for _, item := range anySlice(raw) {
		tool, _ := item.(map[string]any)
		name, description, inputSchema := anthropicToolFields(tool)
		if name == "" {
			continue
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": description,
				"parameters":  firstNonNil(inputSchema, map[string]any{"type": "object"}),
			},
		})
	}
	return out
}

func anthropicToolFields(tool map[string]any) (string, string, any) {
	if tool == nil {
		return "", "", nil
	}
	if getString(tool["type"]) == "custom" {
		custom, _ := tool["custom"].(map[string]any)
		if custom != nil {
			return getString(custom["name"]), getString(custom["description"]), custom["input_schema"]
		}
	}
	return getString(tool["name"]), getString(tool["description"]), tool["input_schema"]
}

func openAIToolChoice(raw any) any {
	choice, _ := raw.(map[string]any)
	if choice == nil {
		return nil
	}
	switch getString(choice["type"]) {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "tool":
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": getString(choice["name"]),
			},
		}
	default:
		return nil
	}
}

func openAIChatToAnthropic(body []byte, model string, state impersonation.Result) ([]byte, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	choices := anySlice(root["choices"])
	if len(choices) == 0 {
		return nil, fmt.Errorf("OpenAI response had no choices")
	}
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	if msg == nil {
		return nil, fmt.Errorf("OpenAI response choice had no message")
	}

	var content []any
	if text := getString(msg["content"]); strings.TrimSpace(text) != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, raw := range anySlice(msg["tool_calls"]) {
		call, _ := raw.(map[string]any)
		fn, _ := call["function"].(map[string]any)
		var input any = map[string]any{}
		if args := getString(fn["arguments"]); strings.TrimSpace(args) != "" {
			_ = json.Unmarshal([]byte(args), &input)
		}
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    firstString(call["id"], "toolu_"+impersonation.NewSessionID()),
			"name":  getString(fn["name"]),
			"input": input,
		})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}

	out := map[string]any{
		"id":            firstString(root["id"], "msg_"+impersonation.NewSessionID()),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   anthropicStopReason(getString(choice["finish_reason"])),
		"stop_sequence": nil,
		"usage":         anthropicUsage(root["usage"]),
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return impersonation.ReverseAnthropicMessages(encoded, state)
}

func writeAnthropicSSE(w http.ResponseWriter, body []byte) {
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeSSE(w, "message_start", map[string]any{
		"type":    "message_start",
		"message": cloneMessageStart(msg),
	})
	for i, raw := range anySlice(msg["content"]) {
		block, _ := raw.(map[string]any)
		startBlock := block
		if getString(block["type"]) == "text" {
			startBlock = map[string]any{"type": "text", "text": ""}
		}
		writeSSE(w, "content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": startBlock})
		if getString(block["type"]) == "text" {
			writeSSE(w, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": i,
				"delta": map[string]any{"type": "text_delta", "text": getString(block["text"])},
			})
		}
		writeSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	writeSSE(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": msg["stop_reason"], "stop_sequence": msg["stop_sequence"]},
		"usage": msg["usage"],
	})
	writeSSE(w, "message_stop", map[string]any{"type": "message_stop"})
	if flusher != nil {
		flusher.Flush()
	}
}

func writeSSE(w io.Writer, event string, data any) {
	encoded, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, encoded)
}

func forwardAnthropicSSE(w http.ResponseWriter, body io.Reader, state impersonation.Result) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	var pending []byte
	for {
		n, err := body.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			for {
				idx := bytes.IndexByte(pending, '\n')
				if idx < 0 {
					break
				}
				line := append([]byte(nil), pending[:idx]...)
				pending = pending[idx+1:]
				line = impersonation.ReverseAnthropicSSELine(line, state)
				_, _ = w.Write(line)
				_, _ = w.Write([]byte("\n"))
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if len(pending) > 0 {
				line := impersonation.ReverseAnthropicSSELine(pending, state)
				_, _ = w.Write(line)
			}
			return
		}
	}
}

func cloneMessageStart(msg map[string]any) map[string]any {
	out := make(map[string]any, len(msg))
	for k, v := range msg {
		out[k] = v
	}
	out["content"] = []any{}
	return out
}

func anthropicStopReason(reason string) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "stop_sequence"
	default:
		return "end_turn"
	}
}

func anthropicUsage(raw any) map[string]any {
	usage, _ := raw.(map[string]any)
	return map[string]any{
		"input_tokens":  numberOrZero(usage["prompt_tokens"]),
		"output_tokens": numberOrZero(usage["completion_tokens"]),
	}
}

func extractText(value any) []string {
	switch v := value.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, raw := range v {
			out = append(out, extractText(raw)...)
		}
		return out
	case map[string]any:
		switch getString(v["type"]) {
		case "", "text", "input_text":
			return []string{getString(v["text"])}
		case "tool_result":
			return extractText(v["content"])
		default:
			return nil
		}
	default:
		return nil
	}
}

func roughTokenCount(raw []byte) int {
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return len(raw)/4 + 1
	}
	text := strings.Join(extractText(root["system"]), "\n") + "\n" + strings.Join(extractText(root["messages"]), "\n")
	if text == "\n" {
		return len(raw)/4 + 1
	}
	return len(text)/4 + 1
}

func (cfg config) url(path string) string {
	return strings.TrimRight(cfg.upstreamBaseURL, "/") + path
}

func (cfg config) anthropicURL(path string) string {
	return strings.TrimRight(cfg.anthropicBaseURL, "/") + path
}

func forceJSONModel(body []byte, model string) []byte {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return body
	}
	root["model"] = model
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

func (cfg config) dumpJSON(label string, body []byte) {
	if cfg.debugDumpDir == "" || len(body) == 0 {
		return
	}
	if err := os.MkdirAll(cfg.debugDumpDir, 0o700); err != nil {
		log.Printf("debug dump disabled: could not create %s: %v", cfg.debugDumpDir, err)
		return
	}
	name := fmt.Sprintf("%s-%s.json", time.Now().UTC().Format("20060102T150405.000000000Z"), safeDumpLabel(label))
	path := filepath.Join(cfg.debugDumpDir, name)
	var pretty bytes.Buffer
	if json.Indent(&pretty, body, "", "  ") == nil {
		body = pretty.Bytes()
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		log.Printf("debug dump failed for %s: %v", label, err)
	}
}

func safeDumpLabel(label string) string {
	var b strings.Builder
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		return "dump"
	}
	return out
}

func copyHeader(dst http.Header, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func decodeResponseBody(body []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return body, nil
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		return io.ReadAll(reader)
	default:
		return nil, fmt.Errorf("unsupported upstream content encoding %q", encoding)
	}
}

func copyNumber(src map[string]any, dst map[string]any, key string) {
	copyNumberAs(src, dst, key, key)
}

func copyNumberAs(src map[string]any, dst map[string]any, srcKey string, dstKey string) {
	switch v := src[srcKey].(type) {
	case float64, json.Number:
		dst[dstKey] = v
	}
}

func anySlice(value any) []any {
	if value == nil {
		return nil
	}
	if out, ok := value.([]any); ok {
		return out
	}
	return nil
}

func getString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	default:
		return ""
	}
}

func firstString(value any, fallback string) string {
	if s := strings.TrimSpace(getString(value)); s != "" {
		return s
	}
	return fallback
}

func getBool(value any) bool {
	b, _ := value.(bool)
	return b
}

func firstNonNil(value any, fallback any) any {
	if value != nil {
		return value
	}
	return fallback
}

func numberOrZero(value any) any {
	if value == nil {
		return 0
	}
	return value
}

func firstArg() string {
	if flag.NArg() == 0 {
		return ""
	}
	return flag.Arg(0)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func normalizeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "anthropic", "direct", "direct-anthropic", "direct_to_anthropic", "direct-to-anthropic":
		return "direct-anthropic"
	case "openai", "proxy", "openai-proxy", "openai_proxy", "upstream-proxy":
		return "openai-proxy"
	default:
		return strings.ToLower(strings.TrimSpace(mode))
	}
}

func envBoolDefault(name string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
