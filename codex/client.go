// Package codex talks to the ChatGPT-subscription backend that the codex CLI
// uses, authenticating with the credentials codex stores in ~/.codex.
//
// The backend is private and undocumented; this package mirrors what the
// open-source codex client sends. Expect it to drift.
package codex

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the codex model endpoint root for ChatGPT auth.
	DefaultBaseURL = "https://chatgpt.com/backend-api/codex"
	// DefaultChatGPTURL is the root for the non-model backend (usage, tasks).
	DefaultChatGPTURL = "https://chatgpt.com/backend-api"

	fallbackVersion = "0.153.3"
)

// Originator is sent as the `originator` header and User-Agent product. The
// backend keys behaviour off it, so it defaults to the codex CLI's value;
// CODEX_INTERNAL_ORIGINATOR_OVERRIDE overrides it, same as in codex.
var Originator = func() string {
	if o := os.Getenv("CODEX_INTERNAL_ORIGINATOR_OVERRIDE"); o != "" {
		return o
	}
	return "codex_cli_rs"
}()

var clientVersion = detectVersion()

// detectVersion uses the version of the codex that last wrote the model
// cache, so the backend treats us like the locally installed codex.
func detectVersion() string {
	if v := os.Getenv("CODEXCLI_CLIENT_VERSION"); v != "" {
		return v
	}
	if home, err := CodexHome(); err == nil {
		if b, err := os.ReadFile(filepath.Join(home, "models_cache.json")); err == nil {
			var c struct {
				V string `json:"client_version"`
			}
			if json.Unmarshal(b, &c) == nil && c.V != "" {
				return c.V
			}
		}
	}
	return fallbackVersion
}

func userAgent() string {
	return fmt.Sprintf("%s/%s (%s; %s) codexcli-go", Originator, clientVersion, runtime.GOOS, runtime.GOARCH)
}

// Client is a handle on the backend. It is safe for concurrent use.
type Client struct {
	auth       *Auth
	hc         *http.Client
	baseURL    string
	chatgptURL string
	sessionID  string
	installID  string
	maxRetries int
}

// Option configures a Client.
type Option func(*Client)

// WithAuth uses specific credentials instead of the default codex home.
func WithAuth(a *Auth) Option { return func(c *Client) { c.auth = a } }

// WithHTTPClient replaces the HTTP client. It must not set a total Timeout,
// or long streams will be cut off.
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.hc = hc } }

// WithBaseURL overrides the codex endpoint root.
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithChatGPTURL overrides the non-model backend root.
func WithChatGPTURL(u string) Option {
	return func(c *Client) { c.chatgptURL = strings.TrimRight(u, "/") }
}

// WithSessionID pins the session id, which is also the default prompt cache
// key. Reuse one across the turns of a conversation to keep cache hits.
func WithSessionID(id string) Option { return func(c *Client) { c.sessionID = id } }

// WithMaxRetries sets how often a request is retried on 5xx and transport
// errors before any response body is consumed (default 3).
func WithMaxRetries(n int) Option { return func(c *Client) { c.maxRetries = n } }

// New builds a Client. Without WithAuth it loads ~/.codex/auth.json.
func New(opts ...Option) (*Client, error) {
	c := &Client{baseURL: DefaultBaseURL, chatgptURL: DefaultChatGPTURL, maxRetries: 3}
	for _, o := range opts {
		o(c)
	}
	if c.auth == nil {
		a, err := LoadAuth()
		if err != nil {
			return nil, err
		}
		c.auth = a
	}
	if c.hc == nil {
		// The jar carries Cloudflare's cookies between requests, like codex.
		jar, _ := cookiejar.New(nil)
		c.hc = &http.Client{Jar: jar}
	}
	if c.sessionID == "" {
		c.sessionID = NewID()
	}
	if home, err := CodexHome(); err == nil {
		if b, err := os.ReadFile(filepath.Join(home, "installation_id")); err == nil {
			c.installID = strings.TrimSpace(string(b))
		}
	}
	return c, nil
}

// Auth exposes the credentials the client uses.
func (c *Client) Auth() *Auth { return c.auth }

// SessionID is the client's session id.
func (c *Client) SessionID() string { return c.sessionID }

// NewID returns a random UUIDv4.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// APIError is a non-2xx answer, or a failure reported inside a stream.
type APIError struct {
	Status    int    // HTTP status; 0 for in-stream failures
	Type      string // error.type, e.g. usage_limit_reached
	Code      string // error.code, e.g. context_length_exceeded
	Message   string
	PlanType  string
	ResetsAt  time.Time // when a usage limit lifts, if the server said
	RequestID string
	Body      string // raw body, for anything not parsed above
	// RateLimits holds what the response headers said, if anything.
	RateLimits *RateLimits
}

func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString("codex: ")
	if e.Status != 0 {
		fmt.Fprintf(&b, "http %d: ", e.Status)
	}
	kind := e.Code
	if kind == "" {
		kind = e.Type
	}
	switch {
	case kind != "" && e.Message != "":
		fmt.Fprintf(&b, "%s: %s", kind, e.Message)
	case kind != "":
		b.WriteString(kind)
	case e.Message != "":
		b.WriteString(e.Message)
	default:
		b.WriteString(truncate(e.Body, 400))
	}
	if !e.ResetsAt.IsZero() {
		fmt.Fprintf(&b, " (resets %s)", e.ResetsAt.Local().Format("Mon 15:04"))
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " [request %s]", e.RequestID)
	}
	return b.String()
}

// UsageLimited reports whether the subscription's usage limit was hit.
func (e *APIError) UsageLimited() bool {
	return e.Type == "usage_limit_reached" || e.Code == "usage_limit_reached"
}

func parseAPIError(status int, h http.Header, body []byte) *APIError {
	e := &APIError{Status: status, Body: string(body)}
	if h != nil {
		e.RequestID = h.Get("x-request-id")
		if e.RequestID == "" {
			e.RequestID = h.Get("x-oai-request-id")
		}
		e.RateLimits = parseRateLimitHeaders(h)
	}
	var m struct {
		Error  json.RawMessage `json:"error"`
		Detail json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(body, &m) != nil {
		return e
	}
	fillErrorFields(e, m.Error)
	if e.Message == "" && e.Code == "" && len(m.Detail) > 0 {
		var s string
		if json.Unmarshal(m.Detail, &s) == nil {
			e.Message = s
		} else {
			fillErrorFields(e, m.Detail)
		}
	}
	return e
}

func fillErrorFields(e *APIError, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		e.Message = s
		return
	}
	var d struct {
		Type     string  `json:"type"`
		Code     any     `json:"code"`
		Message  string  `json:"message"`
		PlanType string  `json:"plan_type"`
		ResetsAt float64 `json:"resets_at"`
	}
	if json.Unmarshal(raw, &d) != nil {
		return
	}
	e.Type, e.Message, e.PlanType = d.Type, d.Message, d.PlanType
	if c, ok := d.Code.(string); ok {
		e.Code = c
	}
	if d.ResetsAt > 0 {
		e.ResetsAt = time.Unix(int64(d.ResetsAt), 0)
	}
}

// do sends an authenticated request. body may be nil. On 401 it refreshes
// the token once and retries; on 5xx and transport errors it backs off and
// retries. The caller owns the returned 2xx response body; any other status
// comes back as *APIError.
func (c *Client) do(ctx context.Context, method, url string, body []byte, hdr http.Header) (*http.Response, error) {
	refreshed := false
	var lastErr error
	for attempt := 0; ; attempt++ {
		tok, err := c.auth.Token(ctx)
		if err != nil {
			return nil, err
		}
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rd)
		if err != nil {
			return nil, err
		}
		for k, v := range hdr {
			req.Header[k] = v
		}
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		if tok.AccountID != "" {
			req.Header.Set("chatgpt-account-id", tok.AccountID)
		}
		if tok.FedRAMP {
			req.Header.Set("X-OpenAI-Fedramp", "true")
		}
		req.Header.Set("originator", Originator)
		req.Header.Set("User-Agent", userAgent())
		req.Header.Set("version", clientVersion)
		if body != nil && req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.hc.Do(req)
		var wait time.Duration
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("codex: %s %s: %w", method, url, err)
		case resp.StatusCode/100 == 2:
			return resp, nil
		default:
			rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			apiErr := parseAPIError(resp.StatusCode, resp.Header, rb)
			if resp.StatusCode == 401 && !refreshed {
				refreshed = true
				if _, err := c.auth.Refresh(ctx, tok.AccessToken); err != nil {
					return nil, err
				}
				attempt--
				continue
			}
			if resp.StatusCode < 500 {
				return nil, apiErr
			}
			lastErr = apiErr
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s >= 0 {
				wait = time.Duration(s) * time.Second
			}
		}
		if attempt >= c.maxRetries {
			return nil, lastErr
		}
		if wait == 0 {
			wait = time.Duration(200*math.Pow(2, float64(attempt))) * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// getJSON GETs url and decodes the body into out.
func (c *Client) getJSON(ctx context.Context, url string, out any) (http.Header, error) {
	return c.callJSON(ctx, http.MethodGet, url, nil, out)
}

// callJSON sends an optional JSON body and decodes the JSON answer into out
// (which may be nil to discard it).
func (c *Client) callJSON(ctx context.Context, method, url string, in, out any) (http.Header, error) {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return nil, err
		}
	}
	resp, err := c.do(ctx, method, url, body, http.Header{"Accept": {"application/json"}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.Header, err
	}
	if out == nil {
		return resp.Header, nil
	}
	if raw, ok := out.(*json.RawMessage); ok {
		*raw = b
		return resp.Header, nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return resp.Header, fmt.Errorf("codex: decoding %s: %w (body: %s)", url, err, truncate(string(b), 200))
	}
	return resp.Header, nil
}

// Raw performs an arbitrary authenticated request against the backend. path
// is appended to the codex base URL, or used as-is if it is a full URL; a
// path starting with "/backend-api/" is resolved against the ChatGPT host.
// It is the escape hatch for endpoints this package doesn't wrap.
func (c *Client) Raw(ctx context.Context, method, path string, body []byte, hdr http.Header) (*http.Response, error) {
	return c.do(ctx, method, c.resolve(path), body, hdr)
}

func (c *Client) resolve(path string) string {
	switch {
	case strings.HasPrefix(path, "http://"), strings.HasPrefix(path, "https://"):
		return path
	case strings.HasPrefix(path, "/backend-api/"):
		return strings.TrimSuffix(c.chatgptURL, "/backend-api") + path
	case strings.HasPrefix(path, "/"):
		return c.baseURL + path
	}
	return c.baseURL + "/" + path
}
