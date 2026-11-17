package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fakeJWT(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "e30." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

func accessJWT(exp time.Time, tag string) string {
	return fakeJWT(map[string]any{"exp": exp.Unix(), "jti": tag})
}

// writeAuth creates a codex home with an auth.json holding an access token
// that expires at exp, plus fields this package doesn't know about.
func writeAuth(t *testing.T, exp time.Time) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	id := fakeJWT(map[string]any{"email": "a@example.com", authClaim: map[string]any{
		"chatgpt_account_id": "acct-1", "chatgpt_plan_type": "pro",
	}})
	auth := map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"future_field":   map[string]any{"keep": "me"},
		"last_refresh":   "2026-01-01T00:00:00Z",
		"tokens": map[string]any{
			"id_token": id, "access_token": accessJWT(exp, "old"), "refresh_token": "rt-old",
			"account_id": "acct-1", "login_owner_id": "owner",
		},
	}
	b, _ := json.MarshalIndent(auth, "", "  ")
	path := filepath.Join(home, "auth.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRefreshRotatesAndPreservesFile(t *testing.T) {
	path := writeAuth(t, time.Now().Add(time.Minute)) // inside the refresh window
	var calls atomic.Int32
	newAccess := accessJWT(time.Now().Add(time.Hour), "new")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["grant_type"] != "refresh_token" || body["refresh_token"] != "rt-old" || body["client_id"] != defaultClientID {
			t.Errorf("unexpected refresh body: %v", body)
		}
		if _, ok := body["scope"]; ok {
			t.Error("scope must not be sent")
		}
		time.Sleep(50 * time.Millisecond) // widen the race window
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": newAccess, "refresh_token": "rt-new"})
	}))
	defer srv.Close()
	t.Setenv(envRefreshURL, srv.URL)

	// Several independent Auths (as separate processes would be) racing to
	// refresh must spend the single-use refresh token exactly once.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := LoadAuthFile(path)
			if err != nil {
				t.Error(err)
				return
			}
			tok, err := a.Token(context.Background())
			if err != nil {
				t.Error(err)
			} else if tok.AccessToken != newAccess && !strings.Contains(tok.AccessToken, ".") {
				t.Errorf("bad token %q", tok.AccessToken)
			}
		}()
	}
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("refresh endpoint hit %d times, want 1", n)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("auth.json mode %v, want 0600", st.Mode().Perm())
	}
	var got map[string]any
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	toks := got["tokens"].(map[string]any)
	if toks["access_token"] != newAccess || toks["refresh_token"] != "rt-new" {
		t.Errorf("tokens not rotated: %v", toks)
	}
	if toks["login_owner_id"] != "owner" || toks["account_id"] != "acct-1" || toks["id_token"] == "" {
		t.Errorf("token fields lost: %v", toks)
	}
	if got["future_field"].(map[string]any)["keep"] != "me" || got["auth_mode"] != "chatgpt" {
		t.Errorf("top-level fields lost: %v", got)
	}
	if v, ok := got["OPENAI_API_KEY"]; !ok || v != nil {
		t.Errorf("OPENAI_API_KEY should stay null, got %v (present=%v)", v, ok)
	}
	if lr, _ := time.Parse(time.RFC3339Nano, got["last_refresh"].(string)); time.Since(lr) > time.Minute {
		t.Errorf("last_refresh not updated: %v", got["last_refresh"])
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".auth-*"))
	if len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

func TestRefreshPermanentFailure(t *testing.T) {
	path := writeAuth(t, time.Now().Add(-time.Minute)) // already expired
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"code":"refresh_token_reused","message":"nope"}}`)
	}))
	defer srv.Close()
	t.Setenv(envRefreshURL, srv.URL)
	before, _ := os.ReadFile(path)
	a, err := LoadAuthFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Token(context.Background())
	var re *RefreshError
	if !errors.As(err, &re) || re.Code != "refresh_token_reused" {
		t.Fatalf("want RefreshError(refresh_token_reused), got %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("auth.json was modified by a failed refresh")
	}
}

func TestFreshTokenDoesNotRefresh(t *testing.T) {
	path := writeAuth(t, time.Now().Add(time.Hour))
	t.Setenv(envRefreshURL, "http://127.0.0.1:1/unreachable")
	a, err := LoadAuthFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := a.Token(context.Background())
	if err != nil || tok.AccountID != "acct-1" {
		t.Fatalf("tok=%+v err=%v", tok, err)
	}
	if acct := a.Account(); acct.Email != "a@example.com" || acct.Plan != "pro" {
		t.Errorf("account = %+v", acct)
	}
}

// backend fakes the codex endpoint for client-level tests.
func backend(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	path := writeAuth(t, time.Now().Add(time.Hour))
	a, err := LoadAuthFile(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(WithAuth(a), WithBaseURL(srv.URL), WithChatGPTURL(srv.URL+"/backend-api"), WithMaxRetries(1))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func sse(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		fmt.Fprintf(w, "event: x\ndata: %s\n\n", e)
	}
}

func TestStreamAndToolLoop(t *testing.T) {
	var bodies []map[string]any
	c := backend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" || r.Header.Get("chatgpt-account-id") != "acct-1" || r.Header.Get("originator") == "" {
			t.Errorf("missing auth headers: %v", r.Header)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("x-codex-primary-used-percent", "42")
		w.Header().Set("x-codex-primary-window-minutes", "10080")
		w.Header().Set("x-codex-active-limit", "premium")
		if len(bodies) == 1 {
			sse(w,
				`{"type":"response.created","response":{"id":"resp_1"}}`,
				`{"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_1","encrypted_content":"zzz","summary":[]}}`,
				`{"type":"response.output_item.done","item":{"type":"function_call","call_id":"c1","name":"add","arguments":"{\"a\":1,\"b\":2}"}}`,
				`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":4},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":2},"total_tokens":15}}}`)
			return
		}
		sse(w,
			`{"type":"response.output_text.delta","delta":"3"}`,
			`not json, skipped`,
			`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"3"}]}}`,
			`{"type":"response.completed","response":{"id":"resp_2","status":"completed","usage":{"input_tokens":20,"output_tokens":1,"total_tokens":21}}}`)
	})
	var deltas strings.Builder
	resp, history, err := c.Run(context.Background(),
		Request{Model: "m", Instructions: "sys", Input: []Item{UserMessage("1+2?")}, Tools: []Tool{FunctionTool("add", "adds", nil)}},
		func(_ context.Context, call FunctionCall) (any, error) {
			if call.Name != "add" || call.Arguments != `{"a":1,"b":2}` {
				t.Errorf("call = %+v", call)
			}
			return "3", nil
		},
		&RunOptions{OnEvent: func(ev *Event) { deltas.WriteString(ev.Delta) }})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text() != "3" || deltas.String() != "3" || resp.ID != "resp_2" {
		t.Errorf("text=%q deltas=%q id=%q", resp.Text(), deltas.String(), resp.ID)
	}
	if resp.Usage.InputTokens != 30 || resp.Usage.CachedTokens != 4 || resp.Usage.ReasoningTokens != 2 {
		t.Errorf("usage not summed: %+v", resp.Usage)
	}
	if rl := resp.RateLimits; rl == nil || rl.Primary == nil || rl.Primary.UsedPercent != 42 || rl.Secondary != nil {
		t.Errorf("rate limits = %+v", rl)
	}
	// user, reasoning, call, output, answer
	var types []string
	for _, it := range history {
		types = append(types, it.Type())
	}
	if got := strings.Join(types, ","); got != "message,reasoning,function_call,function_call_output,message" {
		t.Errorf("history = %s", got)
	}
	if len(bodies) != 2 {
		t.Fatalf("%d requests", len(bodies))
	}
	b := bodies[1]
	if b["stream"] != true || b["store"] != false || b["instructions"] != "sys" || b["prompt_cache_key"] != c.SessionID() {
		t.Errorf("fixed fields wrong: %v", b)
	}
	in := b["input"].([]any)
	if len(in) != 4 || in[1].(map[string]any)["encrypted_content"] != "zzz" || in[3].(map[string]any)["output"] != "3" {
		t.Errorf("second turn did not replay the first: %v", in)
	}
}

func TestStreamFailures(t *testing.T) {
	cases := map[string]struct {
		events []string
		check  func(error) bool
	}{
		"failed": {
			[]string{`{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"too long"}}}`},
			func(err error) bool {
				var e *APIError
				return errors.As(err, &e) && e.Code == "context_length_exceeded"
			},
		},
		"truncated": {
			[]string{`{"type":"response.output_text.delta","delta":"x"}`},
			func(err error) bool { return err != nil && strings.Contains(err.Error(), "before response.completed") },
		},
		"incomplete": {
			[]string{`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"}}}`},
			func(err error) bool { return err != nil && strings.Contains(err.Error(), "max_output_tokens") },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := backend(t, func(w http.ResponseWriter, r *http.Request) { sse(w, tc.events...) })
			_, err := c.Respond(context.Background(), Request{Model: "m"})
			if !tc.check(err) {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestHTTPErrorsAndRetry(t *testing.T) {
	var n atomic.Int32
	c := backend(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			if n.Add(1) == 1 {
				w.WriteHeader(503) // retried once
				return
			}
			if r.URL.Query().Get("client_version") == "" {
				t.Error("client_version missing")
			}
			fmt.Fprint(w, `{"models":[{"slug":"m1","display_name":"M1","visibility":"list","supported_reasoning_levels":[{"effort":"low"}],"brand_new_field":1}]}`)
		case "/backend-api/wham/usage":
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":{"type":"usage_limit_reached","plan_type":"pro","resets_at":1900000000}}`)
		}
	})
	models, err := c.Models(context.Background())
	if err != nil || len(models) != 1 || models[0].Slug != "m1" || !strings.Contains(string(models[0].Raw), "brand_new_field") {
		t.Fatalf("models=%+v err=%v", models, err)
	}
	_, err = c.Usage(context.Background())
	var e *APIError
	if !errors.As(err, &e) || !e.UsageLimited() || e.ResetsAt.Unix() != 1900000000 || e.Status != 429 {
		t.Fatalf("want usage limit error, got %v", err)
	}
}

func TestUnauthorizedTriggersRefresh(t *testing.T) {
	newAccess := accessJWT(time.Now().Add(2*time.Hour), "new")
	tokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), "unauthorized_recovery") {
			t.Errorf("refresh reason missing: %s", b)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": newAccess})
	}))
	defer tokSrv.Close()
	c := backend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+newAccess {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `[]`)
	})
	t.Setenv(envRefreshURL, tokSrv.URL)
	if _, err := c.Environments(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRequestBody(t *testing.T) {
	b, _ := json.Marshal(Request{Model: "m", Schema: map[string]any{"type": "object"}, ServiceTier: "default",
		Extra: map[string]any{"include": nil, "x": 1}})
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	if _, ok := got["include"]; ok {
		t.Error("nil Extra value should delete the field")
	}
	if _, ok := got["service_tier"]; ok {
		t.Error("default tier should be omitted")
	}
	if _, ok := got["tools"]; ok {
		t.Error("tools should be omitted when empty")
	}
	f := got["text"].(map[string]any)["format"].(map[string]any)
	if f["type"] != "json_schema" || f["strict"] != true || got["x"] != float64(1) || got["input"] == nil {
		t.Errorf("body = %s", b)
	}
}
