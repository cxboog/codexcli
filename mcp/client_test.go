package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeReply answers one request the way a small MCP server would.
func fakeReply(method string, params json.RawMessage) (any, *RPCError) {
	switch method {
	case "initialize":
		return map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "fake"}}, nil
	case "tools/list":
		var p struct{ Cursor string }
		_ = json.Unmarshal(params, &p)
		if p.Cursor == "" { // two pages, to exercise pagination
			return map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "echoes", "inputSchema": map[string]any{"type": "object"}}}, "nextCursor": "p2"}, nil
		}
		return map[string]any{"tools": []any{map[string]any{"name": "boom", "inputSchema": map[string]any{"type": "object"}}}}, nil
	case "tools/call":
		var p struct {
			Name      string
			Arguments map[string]any
		}
		_ = json.Unmarshal(params, &p)
		switch p.Name {
		case "echo":
			return map[string]any{"content": []any{
				map[string]any{"type": "text", "text": fmt.Sprint(p.Arguments["msg"])},
				map[string]any{"type": "image", "data": "aGk=", "mimeType": "image/png"},
			}}, nil
		case "boom":
			return map[string]any{"content": []any{map[string]any{"type": "text", "text": "kaboom"}}, "isError": true}, nil
		}
		return nil, &RPCError{Code: -32602, Message: "unknown tool"}
	}
	return nil, &RPCError{Code: -32601, Message: "nope"}
}

// TestMain doubles as the fake stdio server when re-executed with MCP_FAKE.
func TestMain(m *testing.M) {
	switch os.Getenv("MCP_FAKE") {
	case "":
		os.Exit(m.Run())
	case "crash":
		fmt.Fprintln(os.Stderr, "fatal: config missing")
		os.Exit(1)
	}
	in := bufio.NewReader(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	fmt.Println("stray non-JSON log line") // clients must tolerate this
	_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": "srv-1", "method": "ping"})
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			return
		}
		var req struct {
			ID     json.RawMessage
			Method string
			Params json.RawMessage
		}
		if json.Unmarshal(line, &req) != nil || len(req.ID) == 0 || req.Method == "" {
			continue
		}
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{}})
		res, rpcErr := fakeReply(req.Method, req.Params)
		msg := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcErr != nil {
			msg["error"] = rpcErr
		} else {
			msg["result"] = res
		}
		_ = out.Encode(msg)
	}
}

func exercise(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if c.ServerName != "fake" {
		t.Errorf("server name %q", c.ServerName)
	}
	tools, err := c.Tools(ctx)
	if err != nil || len(tools) != 2 || tools[0].Name != "echo" || tools[1].Name != "boom" {
		t.Fatalf("tools=%+v err=%v", tools, err)
	}
	res, err := c.Call(ctx, "echo", json.RawMessage(`{"msg":"hello"}`))
	if err != nil || res.IsError || res.Text() != "hello\n[image image/png]" || res.Content[1].Data != "aGk=" {
		t.Fatalf("echo: %+v err=%v", res, err)
	}
	res, err = c.Call(ctx, "boom", nil)
	if err != nil || !res.IsError || res.Text() != "kaboom" {
		t.Fatalf("boom: %+v err=%v", res, err)
	}
	if _, err = c.Call(ctx, "missing", nil); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("missing tool: err=%v", err)
	}
}

func TestStdio(t *testing.T) {
	c, err := Stdio(context.Background(), os.Args[0], nil, []string{"MCP_FAKE=1"})
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, c)
	done := make(chan struct{})
	go func() { c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
}

func TestStdioServerDies(t *testing.T) {
	_, err := Stdio(context.Background(), os.Args[0], nil, []string{"MCP_FAKE=crash"})
	if err == nil || !strings.Contains(err.Error(), "config missing") {
		t.Fatalf("want the server's stderr in the error, got %v", err)
	}
}

func TestHTTP(t *testing.T) {
	deleted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("header missing on %s", r.Method)
		}
		if r.Method == http.MethodDelete {
			deleted = true
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     json.RawMessage
			Method string
			Params json.RawMessage
		}
		_ = json.Unmarshal(body, &req)
		if req.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != "sess" {
			t.Errorf("%s: session id not echoed", req.Method)
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		res, rpcErr := fakeReply(req.Method, req.Params)
		msg := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcErr != nil {
			msg["error"] = rpcErr
		} else {
			msg["result"] = res
		}
		b, _ := json.Marshal(msg)
		w.Header().Set("Mcp-Session-Id", "sess")
		if req.Method == "tools/call" { // answer on a stream, after a notification
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\nevent: message\ndata: %s\n\n", b)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	c, err := HTTP(context.Background(), srv.URL, map[string]string{"Authorization": "Bearer k"})
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, c)
	c.Close()
	if !deleted {
		t.Error("session was not closed")
	}
}
