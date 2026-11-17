// Package mcp is a small Model Context Protocol client: enough to connect
// to a server over stdio or streamable HTTP, list its tools and call them.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const protocolVersion = "2025-06-18"

// Tool is a tool a server offers.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Content is one block of a tool result.
type Content struct {
	// Type is text, image, audio, resource or resource_link.
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"` // base64, for image and audio
	MimeType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
	Resource *struct {
		URI      string `json:"uri"`
		Text     string `json:"text"`
		MimeType string `json:"mimeType"`
	} `json:"resource,omitempty"`
}

// Result is what a tool call returned.
type Result struct {
	Content []Content `json:"content"`
	// Structured is the tool's structuredContent, if any.
	Structured json.RawMessage `json:"structuredContent,omitempty"`
	// IsError means the tool ran and reports failure.
	IsError bool `json:"isError"`
}

// Text renders the result as text: its text blocks, with placeholders for
// anything else, falling back to the structured content.
func (r *Result) Text() string {
	var parts []string
	for _, c := range r.Content {
		switch {
		case c.Type == "text":
			parts = append(parts, c.Text)
		case c.Resource != nil && c.Resource.Text != "":
			parts = append(parts, c.Resource.Text)
		case c.URI != "":
			parts = append(parts, "["+c.Type+": "+c.URI+"]")
		default:
			parts = append(parts, "["+c.Type+" "+c.MimeType+"]")
		}
	}
	if len(parts) == 0 && len(r.Structured) > 0 {
		return string(r.Structured)
	}
	return strings.Join(parts, "\n")
}

// RPCError is a JSON-RPC error returned by the server.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("mcp: %s (code %d)", e.Message, e.Code) }

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type transport interface {
	// call sends a request and waits for its result.
	call(ctx context.Context, id int64, method string, params any) (json.RawMessage, error)
	notify(ctx context.Context, method string, params any) error
	close() error
}

// Client is a connection to one MCP server. It is safe for concurrent use.
type Client struct {
	t      transport
	nextID atomic.Int64
	// ServerName is what the server calls itself.
	ServerName string
}

func (c *Client) call(ctx context.Context, method string, params, out any) error {
	raw, err := c.t.call(ctx, c.nextID.Add(1), method, params)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func (c *Client) initialize(ctx context.Context) error {
	var res struct {
		ServerInfo struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "cx", "version": "0.1.0"},
	}, &res)
	if err != nil {
		c.t.close()
		return err
	}
	c.ServerName = res.ServerInfo.Name
	return c.t.notify(ctx, "notifications/initialized", nil)
}

// Tools lists every tool the server offers.
func (c *Client) Tools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Tools...)
		if page.NextCursor == "" || page.NextCursor == cursor {
			return all, nil
		}
		cursor = page.NextCursor
	}
}

// Call invokes a tool. arguments is a JSON object (nil or empty means {}).
func (c *Client) Call(ctx context.Context, name string, arguments json.RawMessage) (*Result, error) {
	if len(bytes.TrimSpace(arguments)) == 0 {
		arguments = json.RawMessage("{}")
	}
	res := &Result{}
	if err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments}, res); err != nil {
		return nil, err
	}
	return res, nil
}

// Close shuts the connection down (and, for stdio, the server process).
func (c *Client) Close() error { return c.t.close() }

// ---- stdio -----------------------------------------------------------------

type stdio struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	wmu   sync.Mutex

	mu      sync.Mutex
	pending map[string]chan message
	dead    chan struct{} // closed when the reader exits
	stderr  tail
}

// tail keeps the last few KB written to it, to explain a dead server.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 4096 {
		t.buf = t.buf[len(t.buf)-4096:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// Stdio starts command as an MCP server and connects to it over its stdin
// and stdout. env entries ("K=V") are added to the current environment.
func Stdio(ctx context.Context, command string, args, env []string) (*Client, error) {
	cmd := exec.Command(command, args...)
	cmd.Env = append(os.Environ(), env...)
	s := &stdio{cmd: cmd, pending: map[string]chan message{}, dead: make(chan struct{})}
	cmd.Stderr = &s.stderr
	var err error
	if s.stdin, err = cmd.StdinPipe(); err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp: starting %s: %w", command, err)
	}
	go s.read(out)
	c := &Client{t: s}
	if err := c.initialize(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *stdio) read(out io.Reader) {
	defer close(s.dead)
	rd := bufio.NewReader(out)
	for {
		line, err := rd.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var m message
			if json.Unmarshal(line, &m) == nil {
				s.dispatch(m)
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *stdio) dispatch(m message) {
	if m.Method != "" {
		if len(m.ID) > 0 { // a request from the server to us
			reply := message{JSONRPC: "2.0", ID: m.ID}
			if m.Method == "ping" {
				reply.Result = json.RawMessage("{}")
			} else {
				reply.Error = &RPCError{Code: -32601, Message: "method not supported: " + m.Method}
			}
			_ = s.write(reply)
		}
		return
	}
	s.mu.Lock()
	ch := s.pending[string(m.ID)]
	delete(s.pending, string(m.ID))
	s.mu.Unlock()
	if ch != nil {
		ch <- m
	}
}

func (s *stdio) write(m message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err = s.stdin.Write(append(b, '\n'))
	return err
}

func (s *stdio) gone() error {
	msg := "mcp: server exited"
	if t := s.stderr.String(); t != "" {
		msg += ": " + t
	}
	return errors.New(msg)
}

func (s *stdio) call(ctx context.Context, id int64, method string, params any) (json.RawMessage, error) {
	key := fmt.Sprint(id)
	ch := make(chan message, 1)
	s.mu.Lock()
	s.pending[key] = ch
	s.mu.Unlock()
	forget := func() {
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
	}
	if err := s.write(message{JSONRPC: "2.0", ID: json.RawMessage(key), Method: method, Params: params}); err != nil {
		forget()
		select {
		case <-s.dead:
			return nil, s.gone()
		case <-time.After(200 * time.Millisecond): // let stderr arrive
			return nil, s.gone()
		}
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, m.Error
		}
		return m.Result, nil
	case <-s.dead:
		forget()
		return nil, s.gone()
	case <-ctx.Done():
		forget()
		_ = s.write(message{JSONRPC: "2.0", Method: "notifications/cancelled", Params: map[string]any{"requestId": id}})
		return nil, ctx.Err()
	}
}

func (s *stdio) notify(_ context.Context, method string, params any) error {
	return s.write(message{JSONRPC: "2.0", Method: method, Params: params})
}

func (s *stdio) close() error {
	_ = s.stdin.Close() // the polite way to ask a stdio server to leave
	done := make(chan struct{})
	go func() { _ = s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
	return nil
}

// ---- streamable HTTP -------------------------------------------------------

type httpTransport struct {
	url     string
	headers map[string]string
	hc      *http.Client

	mu      sync.Mutex
	session string
}

// HTTP connects to a streamable-HTTP MCP server. headers are sent on every
// request (e.g. Authorization).
func HTTP(ctx context.Context, url string, headers map[string]string) (*Client, error) {
	c := &Client{t: &httpTransport{url: url, headers: headers, hc: &http.Client{}}}
	if err := c.initialize(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (h *httpTransport) post(ctx context.Context, m message) (*http.Response, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	h.mu.Lock()
	if h.session != "" {
		req.Header.Set("Mcp-Session-Id", h.session)
	}
	h.mu.Unlock()
	if m.Method != "initialize" {
		req.Header.Set("MCP-Protocol-Version", protocolVersion)
	}
	resp, err := h.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return nil, fmt.Errorf("mcp: %s: http %d: %s", h.url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		h.mu.Lock()
		h.session = id
		h.mu.Unlock()
	}
	return resp, nil
}

func (h *httpTransport) call(ctx context.Context, id int64, method string, params any) (json.RawMessage, error) {
	key := fmt.Sprint(id)
	resp, err := h.post(ctx, message{JSONRPC: "2.0", ID: json.RawMessage(key), Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	answer := func(raw []byte) (json.RawMessage, bool, error) {
		var m message
		if json.Unmarshal(raw, &m) != nil || m.Method != "" || string(m.ID) != key {
			return nil, false, nil
		}
		if m.Error != nil {
			return nil, true, m.Error
		}
		return m.Result, true, nil
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if res, ok, err := answer(body); ok {
			return res, err
		}
		return nil, fmt.Errorf("mcp: unexpected reply to %s: %.200s", method, body)
	}
	// The reply arrives as an event on a stream that may carry other
	// messages (progress, logging) first.
	rd := bufio.NewReader(resp.Body)
	var data []byte
	for {
		line, err := rd.ReadBytes('\n')
		line = bytes.TrimRight(line, "\r\n")
		if v, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = append(data, bytes.TrimPrefix(v, []byte(" "))...)
		} else if len(line) == 0 && data != nil {
			if res, ok, rerr := answer(data); ok {
				return res, rerr
			}
			data = nil
		}
		if err != nil {
			if data != nil {
				if res, ok, rerr := answer(data); ok {
					return res, rerr
				}
			}
			return nil, fmt.Errorf("mcp: stream ended without a reply to %s", method)
		}
	}
}

func (h *httpTransport) notify(ctx context.Context, method string, params any) error {
	resp, err := h.post(ctx, message{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (h *httpTransport) close() error {
	h.mu.Lock()
	session := h.session
	h.mu.Unlock()
	if session == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, h.url, nil)
	if err != nil {
		return err
	}
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Mcp-Session-Id", session)
	if resp, err := h.hc.Do(req); err == nil {
		resp.Body.Close()
	}
	return nil
}
