package codex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultIssuer   = "https://auth.openai.com"
	defaultClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	// Same knobs codex itself honours.
	envRefreshURL = "CODEX_REFRESH_TOKEN_URL_OVERRIDE"
	envClientID   = "CODEX_APP_SERVER_LOGIN_CLIENT_ID"

	// Refresh when the access token expires within this window.
	refreshWindow = 5 * time.Minute
	// Fallback for access tokens with no readable exp claim.
	refreshInterval = 8 * 24 * time.Hour

	authClaim    = "https://api.openai.com/auth"
	profileClaim = "https://api.openai.com/profile"
)

// ErrNotLoggedIn is returned when no usable ChatGPT credentials are found.
var ErrNotLoggedIn = errors.New("codex: no ChatGPT credentials found (run `codex login`)")

// RefreshError is a permanent failure of the refresh token: the user has to
// log in again, retrying won't help.
type RefreshError struct {
	Status int
	Code   string
	Body   string
}

func (e *RefreshError) Error() string {
	why := "was rejected"
	switch strings.ToLower(e.Code) {
	case "refresh_token_expired":
		why = "has expired"
	case "refresh_token_reused":
		why = "was already used"
	case "refresh_token_invalidated":
		why = "was revoked"
	}
	return fmt.Sprintf("codex: refresh token %s (http %d %s); run `codex login`", why, e.Status, e.Code)
}

// Token is a snapshot of the credentials needed to make one request.
type Token struct {
	AccessToken string
	AccountID   string
	FedRAMP     bool
}

// Account is what the stored tokens say about who is logged in.
type Account struct {
	Email     string    `json:"email,omitempty"`
	Plan      string    `json:"plan,omitempty"`
	AccountID string    `json:"account_id,omitempty"`
	UserID    string    `json:"user_id,omitempty"`
	FedRAMP   bool      `json:"fedramp,omitempty"`
	Expires   time.Time `json:"access_token_expires,omitzero"`
	Refreshed time.Time `json:"last_refresh,omitzero"`
	Path      string    `json:"auth_file"`
}

// Auth reads, refreshes and writes back the credentials codex keeps in
// $CODEX_HOME/auth.json. It cooperates with a concurrently running codex: it
// takes the same lock files, re-reads the file before refreshing, and
// preserves every field it doesn't understand.
//
// An Auth is safe for concurrent use.
type Auth struct {
	path string
	hc   *http.Client

	mu  sync.Mutex
	cur *authState
}

type authState struct {
	top    map[string]json.RawMessage // whole file, unknown keys preserved
	tokens map[string]json.RawMessage // top["tokens"], unknown keys preserved

	access, refresh, id, accountID string
	issuer, clientID               string // oauth_provenance, if any
	lastRefresh                    time.Time
}

// CodexHome resolves the codex state directory the way codex does:
// $CODEX_HOME if set, else ~/.codex.
func CodexHome() (string, error) {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

// LoadAuth loads credentials from the default codex home.
func LoadAuth() (*Auth, error) {
	home, err := CodexHome()
	if err != nil {
		return nil, err
	}
	return LoadAuthFile(filepath.Join(home, "auth.json"))
}

// LoadAuthFile loads credentials from a specific auth.json.
func LoadAuthFile(path string) (*Auth, error) {
	// Follow symlinks so the lock files and the atomic rename land next to
	// the real file, like codex does.
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	a := &Auth{path: path, hc: &http.Client{Timeout: 45 * time.Second}}
	st, err := readAuthState(path)
	if err != nil {
		return nil, err
	}
	a.cur = st
	return a, nil
}

// Path is the auth.json this Auth is bound to.
func (a *Auth) Path() string { return a.path }

func readAuthState(path string) (*authState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s does not exist (if codex is set to store credentials in the keyring, set cli_auth_credentials_store = \"file\")", ErrNotLoggedIn, path)
		}
		return nil, err
	}
	st := &authState{}
	if err := json.Unmarshal(b, &st.top); err != nil {
		return nil, fmt.Errorf("codex: parsing %s: %w", path, err)
	}
	var mode string
	if raw, ok := st.top["auth_mode"]; ok {
		_ = json.Unmarshal(raw, &mode)
	}
	if mode != "" && mode != "chatgpt" {
		return nil, fmt.Errorf("%w: %s has auth_mode %q, need \"chatgpt\"", ErrNotLoggedIn, path, mode)
	}
	raw, ok := st.top["tokens"]
	if !ok || string(raw) == "null" {
		return nil, fmt.Errorf("%w: %s has no tokens", ErrNotLoggedIn, path)
	}
	if err := json.Unmarshal(raw, &st.tokens); err != nil {
		return nil, fmt.Errorf("codex: parsing tokens in %s: %w", path, err)
	}
	str := func(m map[string]json.RawMessage, k string) string {
		var s string
		if r, ok := m[k]; ok {
			_ = json.Unmarshal(r, &s)
		}
		return s
	}
	st.access = str(st.tokens, "access_token")
	st.refresh = str(st.tokens, "refresh_token")
	st.id = str(st.tokens, "id_token")
	st.accountID = str(st.tokens, "account_id")
	if st.access == "" || st.refresh == "" {
		return nil, fmt.Errorf("%w: %s is missing access or refresh token", ErrNotLoggedIn, path)
	}
	if r, ok := st.tokens["oauth_provenance"]; ok {
		var p struct {
			Issuer   string `json:"issuer"`
			ClientID string `json:"client_id"`
		}
		if json.Unmarshal(r, &p) == nil {
			st.issuer, st.clientID = strings.TrimRight(p.Issuer, "/"), p.ClientID
		}
	}
	if s := str(st.top, "last_refresh"); s != "" {
		st.lastRefresh, _ = time.Parse(time.RFC3339Nano, s)
	}
	if st.accountID == "" {
		st.accountID, _ = jwtAuthClaims(st.id)["chatgpt_account_id"].(string)
	}
	return st, nil
}

func jwtClaims(tok string) map[string]any {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func jwtAuthClaims(tok string) map[string]any {
	m, _ := jwtClaims(tok)[authClaim].(map[string]any)
	return m
}

func jwtExpiry(tok string) time.Time {
	if exp, ok := jwtClaims(tok)["exp"].(float64); ok && exp > 0 {
		return time.Unix(int64(exp), 0)
	}
	return time.Time{}
}

func (st *authState) stale(now time.Time) bool {
	if exp := jwtExpiry(st.access); !exp.IsZero() {
		return !exp.After(now.Add(refreshWindow))
	}
	return st.lastRefresh.Before(now.Add(-refreshInterval))
}

func (st *authState) token() Token {
	fed, _ := jwtAuthClaims(st.id)["chatgpt_account_is_fedramp"].(bool)
	return Token{AccessToken: st.access, AccountID: st.accountID, FedRAMP: fed}
}

// Account describes the logged-in account, from the stored tokens (no network).
func (a *Auth) Account() Account {
	a.mu.Lock()
	st := a.cur
	a.mu.Unlock()
	idc := jwtClaims(st.id)
	ac := jwtAuthClaims(st.id)
	s := func(m map[string]any, k string) string { v, _ := m[k].(string); return v }
	acct := Account{
		Email:     s(idc, "email"),
		Plan:      s(ac, "chatgpt_plan_type"),
		AccountID: st.accountID,
		UserID:    s(ac, "chatgpt_user_id"),
		Expires:   jwtExpiry(st.access),
		Refreshed: st.lastRefresh,
		Path:      a.path,
	}
	if acct.Email == "" {
		p, _ := idc[profileClaim].(map[string]any)
		acct.Email = s(p, "email")
	}
	if acct.UserID == "" {
		acct.UserID = s(ac, "user_id")
	}
	acct.FedRAMP, _ = ac["chatgpt_account_is_fedramp"].(bool)
	return acct
}

// Token returns credentials for a request, refreshing first if the access
// token is about to expire. A failed proactive refresh is not fatal while the
// current token is still valid.
func (a *Auth) Token(ctx context.Context) (Token, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if !a.cur.stale(now) {
		return a.cur.token(), nil
	}
	reason := "proactive_expiry"
	if jwtExpiry(a.cur.access).IsZero() {
		reason = "scheduled"
	}
	if err := a.refreshLocked(ctx, a.cur.access, reason); err != nil {
		if exp := jwtExpiry(a.cur.access); exp.IsZero() || exp.After(now) {
			return a.cur.token(), nil
		}
		return Token{}, err
	}
	return a.cur.token(), nil
}

// Refresh forces a refresh after the server rejected rejectedAccessToken (or
// unconditionally if it is empty). If another process or goroutine already
// rotated the tokens, theirs are picked up and no network call is made.
func (a *Auth) Refresh(ctx context.Context, rejectedAccessToken string) (Token, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if rejectedAccessToken != "" && a.cur.access != rejectedAccessToken {
		return a.cur.token(), nil
	}
	reason := "unauthorized_recovery"
	if rejectedAccessToken == "" {
		reason = "forced"
	}
	if err := a.refreshLocked(ctx, a.cur.access, reason); err != nil {
		return Token{}, err
	}
	return a.cur.token(), nil
}

// refreshLocked replaces a.cur with fresh credentials. stale is the access
// token we want to get rid of. Caller holds a.mu.
func (a *Auth) refreshLocked(ctx context.Context, stale, reason string) error {
	// auth.lock is the lock codex holds for its own read-modify-write. We hold
	// it across the network call too, so two of our processes can't both spend
	// the same single-use refresh token.
	unlock, err := lockFile(ctx, filepath.Join(filepath.Dir(a.path), "auth.lock"), 0)
	if err != nil {
		return fmt.Errorf("codex: locking auth: %w", err)
	}
	defer unlock()

	disk, err := readAuthState(a.path)
	if err != nil {
		return err
	}
	if a.cur.accountID != "" && disk.accountID != a.cur.accountID {
		return fmt.Errorf("codex: %s now belongs to a different account; reload credentials", a.path)
	}
	if disk.access != stale {
		// Someone else already rotated. Use theirs.
		a.cur = disk
		return nil
	}

	url := os.Getenv(envRefreshURL)
	if url == "" {
		iss := disk.issuer
		if iss == "" {
			iss = defaultIssuer
		}
		url = iss + "/oauth/token"
	}
	clientID := disk.clientID
	if clientID == "" {
		clientID = strings.TrimSpace(os.Getenv(envClientID))
	}
	if clientID == "" {
		clientID = defaultClientID
	}
	body, _ := json.Marshal(map[string]string{
		"client_id":      clientID,
		"grant_type":     "refresh_token",
		"refresh_reason": reason,
		"refresh_token":  disk.refresh,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("originator", Originator)
	req.Header.Set("User-Agent", userAgent())
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("codex: refreshing token: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		code := oauthErrorCode(rb)
		lc := strings.ToLower(code)
		if resp.StatusCode == 401 || strings.HasPrefix(lc, "refresh_token_") ||
			(resp.StatusCode == 400 && lc == "invalid_grant") {
			return &RefreshError{Status: resp.StatusCode, Code: code, Body: string(rb)}
		}
		return fmt.Errorf("codex: refreshing token: http %d: %s", resp.StatusCode, truncate(string(rb), 300))
	}
	var out struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return fmt.Errorf("codex: refresh response: %w", err)
	}

	set := func(m map[string]json.RawMessage, k, v string) {
		if v != "" {
			m[k], _ = json.Marshal(v)
		}
	}
	set(disk.tokens, "id_token", out.IDToken)
	set(disk.tokens, "access_token", out.AccessToken)
	set(disk.tokens, "refresh_token", out.RefreshToken)
	disk.top["tokens"], _ = json.Marshal(disk.tokens)
	now := time.Now().UTC()
	disk.top["last_refresh"], _ = json.Marshal(now.Format(time.RFC3339Nano))
	if err := a.write(ctx, disk.top); err != nil {
		// The old refresh token is spent, so the new tokens only live in this
		// process now. Keep going with them, but say so loudly.
		fmt.Fprintf(os.Stderr, "codex: WARNING: refreshed tokens could not be saved to %s: %v\n", a.path, err)
	}
	if out.IDToken != "" {
		disk.id = out.IDToken
	}
	if out.AccessToken != "" {
		disk.access = out.AccessToken
	}
	if out.RefreshToken != "" {
		disk.refresh = out.RefreshToken
	}
	disk.lastRefresh = now
	a.cur = disk
	return nil
}

// write atomically replaces auth.json (0600), under codex's commit lock.
func (a *Auth) write(ctx context.Context, top map[string]json.RawMessage) error {
	unlock, err := lockFile(ctx, strings.TrimSuffix(a.path, ".json")+".json.lock", 5*time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	b, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(a.path), ".auth-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

func oauthErrorCode(body []byte) string {
	var m struct {
		Error any    `json:"error"`
		Code  string `json:"code"`
	}
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	switch e := m.Error.(type) {
	case string:
		return e
	case map[string]any:
		if c, ok := e["code"].(string); ok {
			return c
		}
	}
	return m.Code
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
