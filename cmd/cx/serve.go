package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cxboog/codexcli/codex"
)

// Fields of the public Responses API that the subscription backend refuses.
// They are dropped so that stock OpenAI clients work unmodified.
var unsupportedResponseFields = []string{
	"temperature", "top_p", "truncation", "user", "metadata",
	"background", "max_tool_calls", "top_logprobs", "safety_identifier", "prompt_cache_retention",
}

type server struct {
	c     *codex.Client
	key   string
	model string
}

func cmdServe(ctx context.Context, args []string) error {
	fs := newFlags("serve", `serve [flags]

Serves an OpenAI-compatible API backed by the subscription:
  POST /v1/responses          (streaming and not; stateless, so no previous_response_id)
  POST /v1/chat/completions   (streaming and not; text, images, tools, JSON schema)
  GET  /v1/models
Point a client at it with OPENAI_BASE_URL=http://ADDR/v1 and any API key.`)
	addr := fs.String("addr", "127.0.0.1:8787", "listen address")
	key := fs.String("key", "", "require this API key from clients (recommended if not on loopback)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c, err := codex.New()
	if err != nil {
		return err
	}
	if host, _, err := net.SplitHostPort(*addr); err == nil && *key == "" {
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("refusing to expose your subscription on a non-loopback address without -key")
		}
	}
	s := &server{c: c, key: *key, model: codex.LoadConfig().Model}
	mux := http.NewServeMux()
	for _, prefix := range []string{"", "/v1"} {
		mux.HandleFunc("GET "+prefix+"/models", s.models)
		mux.HandleFunc("POST "+prefix+"/responses", s.responses)
		mux.HandleFunc("POST "+prefix+"/chat/completions", s.chat)
	}
	srv := &http.Server{Addr: *addr, Handler: s.guard(mux), ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	log.Printf("cx serve: listening on http://%s/v1", ln.Addr())
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.key != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got == "" {
				got = r.Header.Get("x-api-key")
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.key)) != 1 {
				writeError(w, 401, "invalid_api_key", "invalid API key")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": code, "code": code}})
}

// fail relays an upstream failure in OpenAI's error shape.
func fail(w http.ResponseWriter, err error) {
	var api *codex.APIError
	if errors.As(err, &api) {
		status := api.Status
		if status == 0 {
			status = http.StatusBadGateway
		}
		code := api.Code
		if code == "" {
			code = api.Type
		}
		if code == "" {
			code = "upstream_error"
		}
		writeError(w, status, code, strings.TrimPrefix(api.Error(), "codex: "))
		return
	}
	writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
}

func (s *server) models(w http.ResponseWriter, r *http.Request) {
	models, err := s.c.Models(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		if m.Visibility == "hide" {
			continue
		}
		data = append(data, map[string]any{"id": m.Slug, "object": "model", "owned_by": "openai", "created": 0})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func readBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	var body map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<20)).Decode(&body); err != nil {
		writeError(w, 400, "invalid_request_error", "invalid JSON body: "+err.Error())
		return nil, false
	}
	return body, true
}

// responses forwards a Responses API call, adjusting it to what the
// backend accepts (always streamed, never stored).
func (s *server) responses(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if body["previous_response_id"] != nil {
		writeError(w, 400, "invalid_request_error", "previous_response_id is not supported: the subscription backend stores nothing. Resend the full input, including prior output items.")
		return
	}
	wantStream, _ := body["stream"].(bool)
	body["stream"], body["store"] = true, false
	for _, f := range unsupportedResponseFields {
		delete(body, f)
	}
	if m, _ := body["model"].(string); m == "" {
		body["model"] = s.model
	}
	if in, ok := body["input"].(string); ok {
		body["input"] = []any{codex.Message("user", in)}
	}
	if body["include"] == nil {
		body["include"] = []string{"reasoning.encrypted_content"}
	}
	raw, _ := json.Marshal(body)

	if wantStream {
		resp, err := s.c.Raw(r.Context(), http.MethodPost, "/responses", raw, http.Header{"Accept": {"text/event-stream"}})
		if err != nil {
			fail(w, err)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		copyFlushing(w, resp.Body)
		return
	}
	stream, err := s.c.StreamRaw(r.Context(), raw)
	if err != nil {
		fail(w, err)
		return
	}
	resp, err := stream.Wait(nil)
	if err != nil {
		fail(w, err)
		return
	}
	out := map[string]any{}
	_ = json.Unmarshal(resp.Envelope, &out)
	out["output"] = resp.Output // the envelope's own list comes back empty
	out["output_text"] = resp.Text()
	writeJSON(w, 200, out)
}

func copyFlushing(w http.ResponseWriter, r io.Reader) {
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// ---- chat completions ------------------------------------------------------

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	Tools    []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Parameters  any    `json:"parameters"`
			Strict      *bool  `json:"strict"`
		} `json:"function"`
	} `json:"tools"`
	ToolChoice        any    `json:"tool_choice"`
	ParallelToolCalls *bool  `json:"parallel_tool_calls"`
	ReasoningEffort   string `json:"reasoning_effort"`
	Verbosity         string `json:"verbosity"`
	ServiceTier       string `json:"service_tier"`
	ResponseFormat    *struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Name   string `json:"name"`
			Schema any    `json:"schema"`
		} `json:"json_schema"`
	} `json:"response_format"`
	StreamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

// chatParts converts chat-completions message content (a string or a list
// of parts) into Responses content parts.
func chatParts(raw json.RawMessage, textType string) ([]any, string) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil, ""
		}
		return []any{map[string]any{"type": textType, "text": s}}, s
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	_ = json.Unmarshal(raw, &parts)
	var out []any
	var text strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text", "output_text":
			out = append(out, map[string]any{"type": textType, "text": p.Text})
			text.WriteString(p.Text)
		case "image_url":
			out = append(out, codex.ImageURL(p.ImageURL.URL))
		}
	}
	return out, text.String()
}

func (cr *chatRequest) toRequest(defaultModel string) (codex.Request, error) {
	req := codex.Request{Model: cr.Model, Verbosity: cr.Verbosity, ParallelToolCalls: cr.ParallelToolCalls}
	if req.Model == "" {
		req.Model = defaultModel
	}
	var system []string
	for _, m := range cr.Messages {
		switch m.Role {
		case "system", "developer":
			if _, text := chatParts(m.Content, "input_text"); text != "" {
				system = append(system, text)
			}
		case "user":
			parts, _ := chatParts(m.Content, "input_text")
			req.Input = append(req.Input, codex.Item{"type": "message", "role": "user", "content": parts})
		case "assistant":
			if parts, _ := chatParts(m.Content, "output_text"); len(parts) > 0 {
				req.Input = append(req.Input, codex.Item{"type": "message", "role": "assistant", "content": parts})
			}
			for _, tc := range m.ToolCalls {
				req.Input = append(req.Input, codex.Item{"type": "function_call", "call_id": tc.ID, "name": tc.Function.Name, "arguments": tc.Function.Arguments})
			}
		case "tool":
			_, text := chatParts(m.Content, "input_text")
			req.Input = append(req.Input, codex.Item{"type": "function_call_output", "call_id": m.ToolCallID, "output": text})
		default:
			return req, fmt.Errorf("unsupported message role %q", m.Role)
		}
	}
	req.Instructions = strings.Join(system, "\n\n")
	for _, t := range cr.Tools {
		if t.Type != "function" {
			return req, fmt.Errorf("unsupported tool type %q", t.Type)
		}
		tool := codex.FunctionTool(t.Function.Name, t.Function.Description, t.Function.Parameters)
		if t.Function.Strict != nil {
			tool["strict"] = *t.Function.Strict
		}
		req.Tools = append(req.Tools, tool)
	}
	switch tc := cr.ToolChoice.(type) {
	case string:
		req.ToolChoice = tc
	case map[string]any:
		if fn, ok := tc["function"].(map[string]any); ok {
			req.ToolChoice = map[string]any{"type": "function", "name": fn["name"]}
		}
	}
	if cr.ReasoningEffort != "" {
		req.Reasoning = &codex.Reasoning{Effort: cr.ReasoningEffort}
	}
	if cr.ServiceTier == "priority" || cr.ServiceTier == "flex" {
		req.ServiceTier = cr.ServiceTier
	}
	if rf := cr.ResponseFormat; rf != nil {
		switch rf.Type {
		case "json_schema":
			req.Schema, req.SchemaName = rf.JSONSchema.Schema, rf.JSONSchema.Name
		case "json_object":
			req.Extra = map[string]any{"text": map[string]any{"format": map[string]any{"type": "json_object"}}}
		}
	}
	return req, nil
}

func chatUsage(u *codex.Usage) map[string]any {
	if u == nil {
		return nil
	}
	return map[string]any{
		"prompt_tokens": u.InputTokens, "completion_tokens": u.OutputTokens, "total_tokens": u.TotalTokens,
		"prompt_tokens_details":     map[string]any{"cached_tokens": u.CachedTokens},
		"completion_tokens_details": map[string]any{"reasoning_tokens": u.ReasoningTokens},
	}
}

func (s *server) chat(w http.ResponseWriter, r *http.Request) {
	var cr chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<20)).Decode(&cr); err != nil {
		writeError(w, 400, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	req, err := cr.toRequest(s.model)
	if err != nil {
		writeError(w, 400, "invalid_request_error", err.Error())
		return
	}
	stream, err := s.c.Stream(r.Context(), req)
	if err != nil {
		fail(w, err)
		return
	}
	id := "chatcmpl-" + codex.NewID()
	created := time.Now().Unix()

	toolCall := func(index int, fc codex.FunctionCall) map[string]any {
		return map[string]any{"index": index, "id": fc.CallID, "type": "function",
			"function": map[string]any{"name": fc.Name, "arguments": fc.Arguments}}
	}

	if !cr.Stream {
		resp, err := stream.Wait(nil)
		if err != nil {
			fail(w, err)
			return
		}
		msg := map[string]any{"role": "assistant", "content": resp.Text()}
		finish := "stop"
		if calls := resp.FunctionCalls(); len(calls) > 0 {
			tcs := make([]any, len(calls))
			for i, fc := range calls {
				tcs[i] = toolCall(i, fc)
			}
			msg["tool_calls"] = tcs
			finish = "tool_calls"
		}
		writeJSON(w, 200, map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
			"usage":   chatUsage(resp.Usage),
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
	}
	send(chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	nCalls := 0
	resp, err := stream.Wait(func(ev *codex.Event) {
		switch ev.Type {
		case codex.EventTextDelta:
			send(chunk(map[string]any{"content": ev.Delta}, nil))
		case codex.EventItemDone:
			if fc, ok := ev.Item.AsFunctionCall(); ok {
				send(chunk(map[string]any{"tool_calls": []any{toolCall(nCalls, fc)}}, nil))
				nCalls++
			}
		}
	})
	if err != nil {
		// Headers are gone; the best we can do is an in-band error event.
		send(map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error"}})
		return
	}
	finish := "stop"
	if nCalls > 0 {
		finish = "tool_calls"
	}
	send(chunk(map[string]any{}, finish))
	if cr.StreamOptions.IncludeUsage {
		last := chunk(map[string]any{}, nil)
		last["choices"] = []any{}
		last["usage"] = chatUsage(resp.Usage)
		send(last)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}
