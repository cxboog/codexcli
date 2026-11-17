package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cxboog/codexcli/codex"
)

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	call        func(ctx context.Context, c *codex.Client, args json.RawMessage) (string, error)
}

func obj(required []string, props map[string]any) map[string]any {
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func mcpTools() []mcpTool {
	cfg := codex.LoadConfig()
	return []mcpTool{
		{
			Name: "ask",
			Description: "Ask an OpenAI model (via the user's ChatGPT subscription) a question and get its answer. " +
				"Good for a second opinion, a review from a different model, or a web-grounded answer. " +
				"The model sees only what you put in the prompt, so include all needed context.",
			InputSchema: obj([]string{"prompt"}, map[string]any{
				"prompt":     str("The full prompt."),
				"system":     str("Optional system prompt."),
				"model":      str("Model slug; defaults to " + cfg.Model + ". Use the models tool to list options."),
				"effort":     str("Reasoning effort, e.g. low, medium, high."),
				"web_search": map[string]any{"type": "boolean", "description": "Let the model search the web."},
				"images":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Absolute paths or URLs of images to attach."},
			}),
			call: func(ctx context.Context, c *codex.Client, raw json.RawMessage) (string, error) {
				var a struct {
					Prompt, System, Model, Effort string
					WebSearch                     bool `json:"web_search"`
					Images                        []string
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", err
				}
				if a.Prompt == "" {
					return "", errors.New("prompt is required")
				}
				req := codex.Request{Model: a.Model, Instructions: a.System}
				if req.Model == "" {
					req.Model = cfg.Model
				}
				if a.Effort == "" {
					a.Effort = cfg.ReasoningEffort
				}
				if a.Effort != "" {
					req.Reasoning = &codex.Reasoning{Effort: a.Effort}
				}
				if a.WebSearch {
					req.Tools = []codex.Tool{codex.WebSearchTool(true)}
				}
				var parts []map[string]any
				for _, im := range a.Images {
					if strings.HasPrefix(im, "http://") || strings.HasPrefix(im, "https://") {
						parts = append(parts, codex.ImageURL(im))
						continue
					}
					p, err := codex.ImageFile(im)
					if err != nil {
						return "", err
					}
					parts = append(parts, p)
				}
				req.Input = []codex.Item{codex.UserMessage(a.Prompt, parts...)}
				resp, err := c.Respond(ctx, req)
				if err != nil {
					return "", err
				}
				return resp.Text(), nil
			},
		},
		{
			Name:        "web_search",
			Description: "Search the web through OpenAI's search backend. Returns titles, URLs and snippets.",
			InputSchema: obj([]string{"query"}, map[string]any{
				"query":   str("Search query."),
				"domains": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Restrict results to these domains."},
			}),
			call: func(ctx context.Context, c *codex.Client, raw json.RawMessage) (string, error) {
				var a struct {
					Query   string
					Domains []string
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", err
				}
				res, err := c.Search(ctx, codex.SearchRequest{Queries: []string{a.Query}, Domains: a.Domains})
				if err != nil {
					return "", err
				}
				var b strings.Builder
				for _, h := range res.Results {
					if h.URL != "" {
						fmt.Fprintf(&b, "%s\n%s\n%s\n\n", h.Title, h.URL, strings.Join(strings.Fields(h.Snippet), " "))
					}
				}
				if b.Len() == 0 {
					return res.Output, nil
				}
				return b.String(), nil
			},
		},
		{
			Name:        "generate_image",
			Description: "Generate an image (or edit reference images) with OpenAI's image model and save it to disk. Returns the saved path.",
			InputSchema: obj([]string{"prompt"}, map[string]any{
				"prompt":      str("What to draw."),
				"output_path": str("Absolute path to write to. Defaults to a file in the system temp directory."),
				"size":        str("e.g. 1024x1024, 1536x1024, 1024x1536."),
				"quality":     str("low, medium or high."),
				"background":  str("transparent or opaque."),
				"references":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Absolute paths of images to edit or use as reference."},
			}),
			call: func(ctx context.Context, c *codex.Client, raw json.RawMessage) (string, error) {
				var a struct {
					Prompt, Size, Quality, Background string
					OutputPath                        string `json:"output_path"`
					References                        []string
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", err
				}
				req := codex.ImageRequest{Prompt: a.Prompt, Size: a.Size, Quality: a.Quality, Background: a.Background}
				for _, r := range a.References {
					p, err := codex.ImageFile(r)
					if err != nil {
						return "", err
					}
					req.Images = append(req.Images, p)
				}
				imgs, err := c.GenerateImages(ctx, req)
				if err != nil {
					return "", err
				}
				path := a.OutputPath
				if path == "" {
					path = filepath.Join(os.TempDir(), fmt.Sprintf("cx-image-%s.%s", time.Now().Format("20060102-150405"), imgs[0].Format))
				}
				if err := os.WriteFile(path, imgs[0].Data, 0o644); err != nil {
					return "", err
				}
				return "saved to " + path, nil
			},
		},
		{
			Name:        "models",
			Description: "List the OpenAI models available on the user's subscription, with their reasoning effort levels.",
			InputSchema: obj(nil, map[string]any{}),
			call: func(ctx context.Context, c *codex.Client, _ json.RawMessage) (string, error) {
				models, err := c.Models(ctx)
				if err != nil {
					return "", err
				}
				var b strings.Builder
				for _, m := range models {
					if m.Visibility == "hide" {
						continue
					}
					var lv []string
					for _, l := range m.ReasoningLevels {
						lv = append(lv, l.Effort)
					}
					fmt.Fprintf(&b, "%s: %s (effort: %s)\n", m.Slug, m.Description, strings.Join(lv, ", "))
				}
				return b.String(), nil
			},
		},
		{
			Name:        "usage",
			Description: "Show how much of the ChatGPT subscription's usage limit is used and when it resets.",
			InputSchema: obj(nil, map[string]any{}),
			call: func(ctx context.Context, c *codex.Client, _ json.RawMessage) (string, error) {
				u, err := c.Usage(ctx)
				if err != nil {
					return "", err
				}
				return string(u.Raw), nil
			},
		},
	}
}

// cmdMCP speaks the Model Context Protocol over stdio (newline-delimited
// JSON-RPC), exposing the subscription as tools.
func cmdMCP(ctx context.Context, args []string) error {
	fs := newFlags("mcp", `mcp

Runs an MCP server on stdio with tools: ask, web_search, generate_image, models, usage.
Register it with e.g.:  claude mcp add cx -- cx mcp`)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c, err := codex.New()
	if err != nil {
		return err
	}
	tools := mcpTools()
	var mu sync.Mutex
	out := json.NewEncoder(os.Stdout)
	reply := func(id json.RawMessage, result any, rpcErr map[string]any) {
		msg := map[string]any{"jsonrpc": "2.0", "id": id}
		if rpcErr != nil {
			msg["error"] = rpcErr
		} else {
			msg["result"] = result
		}
		mu.Lock()
		defer mu.Unlock()
		_ = out.Encode(msg)
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	in := bufio.NewReader(os.Stdin)
	for {
		line, err := in.ReadBytes('\n')
		if len(line) > 0 {
			var req rpcRequest
			if json.Unmarshal(line, &req) != nil {
				reply(nil, nil, map[string]any{"code": -32700, "message": "parse error"})
			} else {
				wg.Add(1)
				go func() {
					defer wg.Done()
					handleMCP(ctx, c, tools, req, reply)
				}()
			}
		}
		if err != nil || ctx.Err() != nil {
			return nil // stdin closed: the client is gone
		}
	}
}

func handleMCP(ctx context.Context, c *codex.Client, tools []mcpTool, req rpcRequest, reply func(json.RawMessage, any, map[string]any)) {
	if len(req.ID) == 0 {
		return // notification; nothing to answer
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		reply(req.ID, map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "cx", "version": "0.1.0"},
		}, nil)
	case "ping":
		reply(req.ID, map[string]any{}, nil)
	case "tools/list":
		reply(req.ID, map[string]any{"tools": tools}, nil)
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			reply(req.ID, nil, map[string]any{"code": -32602, "message": err.Error()})
			return
		}
		if len(p.Arguments) == 0 {
			p.Arguments = json.RawMessage("{}")
		}
		for _, t := range tools {
			if t.Name != p.Name {
				continue
			}
			text, err := t.call(ctx, c, p.Arguments)
			isErr := err != nil
			if isErr {
				text = strings.TrimPrefix(err.Error(), "codex: ")
			}
			reply(req.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isErr}, nil)
			return
		}
		reply(req.ID, nil, map[string]any{"code": -32602, "message": "unknown tool: " + p.Name})
	default:
		reply(req.ID, nil, map[string]any{"code": -32601, "message": "method not found: " + req.Method})
	}
}
