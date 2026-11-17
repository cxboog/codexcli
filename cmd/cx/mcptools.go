package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cxboog/codexcli/codex"
	"github.com/cxboog/codexcli/mcp"
)

// maxToolOutput caps what one tool call may put into the model's context.
const maxToolOutput = 200_000

// serverSpec is one MCP server to connect to.
type serverSpec struct {
	name    string
	command string
	args    []string
	env     []string
	url     string
	headers map[string]string
}

// parseMCPFlag turns a -mcp value into server specs:
//
//	https://host/mcp        streamable HTTP server
//	@servers.json           every server in a {"mcpServers": {...}} file (.mcp.json format)
//	anything else           a command line to run as a stdio server
func parseMCPFlag(v string) ([]serverSpec, error) {
	switch {
	case strings.HasPrefix(v, "http://"), strings.HasPrefix(v, "https://"):
		return []serverSpec{{name: v, url: v}}, nil
	case strings.HasPrefix(v, "@"):
		return loadMCPConfig(v[1:])
	}
	words, err := splitCommand(v)
	if err != nil {
		return nil, fmt.Errorf("-mcp %q: %w", v, err)
	}
	if len(words) == 0 {
		return nil, errors.New("-mcp: empty command")
	}
	return []serverSpec{{name: filepath.Base(words[0]), command: words[0], args: words[1:]}}, nil
}

func loadMCPConfig(path string) ([]serverSpec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Servers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(cfg.Servers) == 0 {
		return nil, fmt.Errorf("%s: no mcpServers", path)
	}
	names := make([]string, 0, len(cfg.Servers))
	for n := range cfg.Servers {
		names = append(names, n)
	}
	sort.Strings(names)
	var specs []serverSpec
	for _, n := range names {
		s := cfg.Servers[n]
		spec := serverSpec{name: n, command: os.ExpandEnv(s.Command), url: os.ExpandEnv(s.URL)}
		switch {
		case s.Type == "sse":
			return nil, fmt.Errorf("%s: server %q uses the legacy SSE transport, which is not supported (use streamable HTTP or stdio)", path, n)
		case spec.url != "":
			spec.headers = map[string]string{}
			for k, v := range s.Headers {
				spec.headers[k] = os.ExpandEnv(v)
			}
		case spec.command != "":
			for _, a := range s.Args {
				spec.args = append(spec.args, os.ExpandEnv(a))
			}
			for k, v := range s.Env {
				spec.env = append(spec.env, k+"="+os.ExpandEnv(v))
			}
		default:
			return nil, fmt.Errorf("%s: server %q has neither command nor url", path, n)
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// splitCommand splits a command line on spaces, honouring single quotes,
// double quotes and backslash escapes.
func splitCommand(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\\' && i+1 < len(rs):
			i++
			cur.WriteRune(rs[i])
			inWord = true
		case quote == '"':
			if r == '"' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

// toolset is the union of the tools of several MCP servers, presented to
// the model as function tools.
type toolset struct {
	clients []*mcp.Client
	tools   []codex.Tool
	route   map[string]boundTool
}

type boundTool struct {
	client *mcp.Client
	name   string // the tool's real name on its server
}

var badToolChar = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func connectMCP(ctx context.Context, flags []string) (*toolset, error) {
	ts := &toolset{route: map[string]boundTool{}}
	var specs []serverSpec
	for _, f := range flags {
		s, err := parseMCPFlag(f)
		if err != nil {
			return nil, err
		}
		specs = append(specs, s...)
	}
	for _, spec := range specs {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		var c *mcp.Client
		var err error
		if spec.url != "" {
			c, err = mcp.HTTP(cctx, spec.url, spec.headers)
		} else {
			c, err = mcp.Stdio(cctx, spec.command, spec.args, spec.env)
		}
		var tools []mcp.Tool
		if err == nil {
			ts.clients = append(ts.clients, c)
			tools, err = c.Tools(cctx)
		}
		cancel()
		if err != nil {
			ts.close()
			return nil, fmt.Errorf("mcp server %s: %w", spec.name, err)
		}
		for _, t := range tools {
			// Function names are limited to [a-zA-Z0-9_-]{1,64}; qualify with
			// the server name only when two servers offer the same tool.
			name := badToolChar.ReplaceAllString(t.Name, "_")
			if _, taken := ts.route[name]; taken {
				name = badToolChar.ReplaceAllString(spec.name, "_") + "__" + name
			}
			if len(name) > 64 {
				name = name[:64]
			}
			if _, taken := ts.route[name]; taken {
				ts.close()
				return nil, fmt.Errorf("mcp server %s: tool name %q collides with another server's", spec.name, t.Name)
			}
			ts.route[name] = boundTool{c, t.Name}
			ts.tools = append(ts.tools, codex.FunctionTool(name, t.Description, toolSchema(t.InputSchema)))
		}
	}
	return ts, nil
}

// toolSchema makes a server's input schema acceptable to the backend, which
// is stricter than MCP about a few things servers commonly get wrong.
func toolSchema(raw json.RawMessage) any {
	var schema map[string]any
	if json.Unmarshal(raw, &schema) != nil || schema == nil {
		return nil
	}
	if schema["type"] == nil {
		schema["type"] = "object"
	}
	if schema["properties"] == nil {
		schema["properties"] = map[string]any{}
	}
	if v, ok := schema["required"]; ok && v == nil {
		delete(schema, "required")
	}
	return schema
}

func (ts *toolset) close() {
	for _, c := range ts.clients {
		_ = c.Close()
	}
}

// handle runs one model tool call against the server that owns the tool.
func (ts *toolset) handle(ctx context.Context, call codex.FunctionCall) (any, error) {
	bt, ok := ts.route[call.Name]
	if !ok {
		return nil, fmt.Errorf("no such tool %q", call.Name)
	}
	fmt.Fprintf(os.Stderr, "\x1b[2m[tool] %s %s\x1b[0m\n", call.Name, truncateMiddle(call.Arguments, 300))
	res, err := bt.client.Call(ctx, bt.name, json.RawMessage(call.Arguments))
	if err != nil {
		return nil, err
	}
	text := truncateMiddle(res.Text(), maxToolOutput)
	if res.IsError {
		return "error: " + text, nil
	}
	// Images go to the model as images rather than as a placeholder.
	var parts []any
	for _, c := range res.Content {
		if c.Type == "image" && c.Data != "" {
			parts = append(parts, codex.ImageURL("data:"+c.MimeType+";base64,"+c.Data))
		}
	}
	if len(parts) == 0 {
		return text, nil
	}
	if text != "" {
		parts = append([]any{map[string]any{"type": "input_text", "text": text}}, parts...)
	}
	return parts, nil
}

func truncateMiddle(s string, max int) string {
	if len(s) <= max {
		return s
	}
	half := max / 2
	return fmt.Sprintf("%s\n[... %d bytes omitted ...]\n%s", s[:half], len(s)-2*half, s[len(s)-half:])
}
