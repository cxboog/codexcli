package codex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ---- models ----------------------------------------------------------------

// ReasoningLevel is one effort setting a model supports.
type ReasoningLevel struct {
	Effort      string `json:"effort"`
	Description string `json:"description,omitempty"`
}

// Model is one entry of the subscription's model catalog.
type Model struct {
	Slug             string           `json:"slug"`
	DisplayName      string           `json:"display_name"`
	Description      string           `json:"description,omitempty"`
	Visibility       string           `json:"visibility,omitempty"`
	Priority         int              `json:"priority"`
	DefaultReasoning string           `json:"default_reasoning_level,omitempty"`
	ReasoningLevels  []ReasoningLevel `json:"supported_reasoning_levels,omitempty"`
	ContextWindow    int64            `json:"context_window,omitempty"`
	MaxContextWindow int64            `json:"max_context_window,omitempty"`
	InputModalities  []string         `json:"input_modalities,omitempty"`
	SupportVerbosity bool             `json:"support_verbosity,omitempty"`
	// ServiceTiers is left loose: its shape has changed between releases.
	ServiceTiers json.RawMessage `json:"service_tiers,omitempty"`
	// Raw is the full catalog entry, including fields not modelled here.
	Raw json.RawMessage `json:"-"`
}

// Models lists the models the account can use.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	var out struct {
		Models []json.RawMessage `json:"models"`
	}
	u := c.baseURL + "/models?client_version=" + url.QueryEscape(clientVersion)
	if _, err := c.getJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	models := make([]Model, 0, len(out.Models))
	for _, raw := range out.Models {
		var m Model
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("codex: decoding model: %w", err)
		}
		m.Raw = raw
		models = append(models, m)
	}
	return models, nil
}

// ---- usage -----------------------------------------------------------------

// UsageWindow is a rate-limit window as reported by the usage endpoint.
type UsageWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

// Resets is when the window's usage returns to zero.
func (w *UsageWindow) Resets() time.Time { return time.Unix(w.ResetAt, 0) }

// UsageLimit is a pair of windows guarding one metered thing.
type UsageLimit struct {
	Allowed      bool         `json:"allowed"`
	LimitReached bool         `json:"limit_reached"`
	Primary      *UsageWindow `json:"primary_window,omitempty"`
	Secondary    *UsageWindow `json:"secondary_window,omitempty"`
}

// AccountUsage is the subscription's current usage and limits.
type AccountUsage struct {
	PlanType   string      `json:"plan_type"`
	RateLimit  *UsageLimit `json:"rate_limit,omitempty"`
	Credits    *Credits    `json:"credits,omitempty"`
	Additional []struct {
		LimitName      string      `json:"limit_name"`
		MeteredFeature string      `json:"metered_feature"`
		RateLimit      *UsageLimit `json:"rate_limit,omitempty"`
	} `json:"additional_rate_limits,omitempty"`
	// Raw is the full payload.
	Raw json.RawMessage `json:"-"`
}

// Usage fetches the account's usage against its plan limits.
func (c *Client) Usage(ctx context.Context) (*AccountUsage, error) {
	var raw json.RawMessage
	if _, err := c.getJSON(ctx, c.chatgptURL+"/wham/usage", &raw); err != nil {
		return nil, err
	}
	u := &AccountUsage{}
	if err := json.Unmarshal(raw, u); err != nil {
		return nil, fmt.Errorf("codex: decoding usage: %w", err)
	}
	u.Raw = raw
	return u, nil
}

// ---- images ----------------------------------------------------------------

// ImageRequest asks the standalone image endpoint for images.
type ImageRequest struct {
	Prompt string `json:"prompt"`
	// Model defaults to DefaultImageModel.
	Model string `json:"model"`
	// N is the number of images (default 1).
	N int `json:"n,omitempty"`
	// Size is e.g. "1024x1024", "1536x1024", or "auto".
	Size string `json:"size,omitempty"`
	// Quality: low, medium, high, auto.
	Quality string `json:"quality,omitempty"`
	// Background: transparent, opaque, auto.
	Background string `json:"background,omitempty"`
	// Images are references to edit or draw from (see ImageBytes, ImageFile,
	// ImageURL). When present the edits endpoint is used.
	Images []map[string]any `json:"images,omitempty"`
}

// DefaultImageModel is the image model codex itself asks for.
const DefaultImageModel = "gpt-image-2"

// GenerateImages creates (or, with req.Images, edits) images.
func (c *Client) GenerateImages(ctx context.Context, req ImageRequest) ([]Image, error) {
	if req.Model == "" {
		req.Model = DefaultImageModel
	}
	path := "/images/generations"
	if len(req.Images) > 0 {
		path = "/images/edits"
		refs := make([]map[string]any, len(req.Images))
		for i, im := range req.Images {
			ref := map[string]any{}
			for _, k := range []string{"image_url", "file_id"} {
				if v, ok := im[k]; ok {
					ref[k] = v
				}
			}
			refs[i] = ref
		}
		req.Images = refs
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodPost, c.baseURL+path, body, http.Header{"Accept": {"application/json"}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	type datum struct {
		B64           string `json:"b64_json"`
		RevisedPrompt string `json:"revised_prompt"`
		OutputFormat  string `json:"output_format"`
	}
	var data []datum
	format := ""
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '{' {
		var out struct {
			Data         []datum `json:"data"`
			OutputFormat string  `json:"output_format"`
		}
		if err := json.Unmarshal(trimmed, &out); err != nil {
			return nil, fmt.Errorf("codex: decoding image response: %w", err)
		}
		data, format = out.Data, out.OutputFormat
	} else {
		// The endpoint may answer as an event stream instead, one
		// image_generation.completed event per image.
		for _, line := range bytes.Split(raw, []byte("\n")) {
			v, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
			if !ok {
				continue
			}
			var d datum
			if json.Unmarshal(v, &d) == nil && d.B64 != "" {
				data = append(data, d)
			}
		}
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("codex: image endpoint returned no images: %s", truncate(string(raw), 300))
	}
	imgs := make([]Image, 0, len(data))
	for _, d := range data {
		b, err := base64.StdEncoding.DecodeString(d.B64)
		if err != nil {
			return nil, fmt.Errorf("codex: decoding image: %w", err)
		}
		f := d.OutputFormat
		if f == "" {
			f = format
		}
		if f == "" {
			f = "png"
		}
		imgs = append(imgs, Image{Data: b, RevisedPrompt: d.RevisedPrompt, Format: f})
	}
	return imgs, nil
}

// ---- web search ------------------------------------------------------------

// SearchRequest drives the standalone search endpoint. Set Queries for the
// plain case; Commands gives access to everything else it can do (open,
// find, image_query, finance, weather, ... as in codex's `web.run` tool).
type SearchRequest struct {
	Queries []string
	// Domains restricts results to these sites.
	Domains []string
	// Cached answers from OpenAI's index only, without live fetches.
	Cached bool
	// Model defaults to the configured default model.
	Model string
	// ID ties requests together so that ref_ids from one result can be
	// opened in the next. Defaults to the client's session id.
	ID string
	// Commands is merged over the generated commands.
	Commands map[string]any
}

// SearchHit is one search result.
type SearchHit struct {
	Type    string `json:"type"`
	Title   string `json:"title,omitempty"`
	URL     string `json:"url,omitempty"`
	Domain  string `json:"domain,omitempty"`
	Snippet string `json:"snippet,omitempty"`
	RefID   string `json:"ref_id,omitempty"`
}

// SearchResult is what a search returned.
type SearchResult struct {
	// Output is the model-oriented text rendering of the results.
	Output  string      `json:"output"`
	Results []SearchHit `json:"results"`
}

// Search runs web searches (and related commands) server-side.
func (c *Client) Search(ctx context.Context, req SearchRequest) (*SearchResult, error) {
	commands := map[string]any{}
	if len(req.Queries) > 0 {
		qs := make([]map[string]any, len(req.Queries))
		for i, q := range req.Queries {
			qs[i] = map[string]any{"q": q}
		}
		commands["search_query"] = qs
	}
	for k, v := range req.Commands {
		commands[k] = v
	}
	settings := map[string]any{"allowed_callers": []string{"direct"}, "external_web_access": !req.Cached}
	if len(req.Domains) > 0 {
		settings["filters"] = map[string]any{"allowed_domains": req.Domains}
	}
	if req.Model == "" {
		req.Model = LoadConfig().Model
	}
	if req.ID == "" {
		req.ID = c.sessionID
	}
	body := map[string]any{"id": req.ID, "model": req.Model, "commands": commands, "settings": settings}
	out := &SearchResult{}
	if _, err := c.callJSON(ctx, http.MethodPost, c.baseURL+"/alpha/search", body, out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- cloud tasks -----------------------------------------------------------

// Environment is a Codex cloud environment (a configured repo).
type Environment struct {
	ID       string `json:"id"`
	Label    string `json:"label,omitempty"`
	IsPinned bool   `json:"is_pinned,omitempty"`
}

// Environments lists the account's Codex cloud environments.
func (c *Client) Environments(ctx context.Context) ([]Environment, error) {
	var out []Environment
	_, err := c.getJSON(ctx, c.chatgptURL+"/wham/environments", &out)
	return out, err
}

// TaskList is one page of cloud tasks. Items are loose JSON; the fields
// codex relies on are id, title, updated_at and task_status_display.
type TaskList struct {
	Items  []map[string]any `json:"items"`
	Cursor string           `json:"cursor,omitempty"`
}

// Tasks lists Codex cloud tasks, newest first. limit 0 uses the server
// default; environmentID and cursor are optional.
func (c *Client) Tasks(ctx context.Context, limit int, environmentID, cursor string) (*TaskList, error) {
	q := url.Values{"task_filter": {"current"}}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if environmentID != "" {
		q.Set("environment_id", environmentID)
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	out := &TaskList{}
	_, err := c.getJSON(ctx, c.chatgptURL+"/wham/tasks/list?"+q.Encode(), out)
	return out, err
}

// Task is the detail of one cloud task.
type Task struct {
	// Raw is the full payload.
	Raw json.RawMessage
	doc struct {
		Task struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"task"`
		Assistant *taskTurn `json:"current_assistant_turn"`
		Diff      *taskTurn `json:"current_diff_task_turn"`
	}
}

type taskTurn struct {
	ID     string           `json:"id"`
	Status string           `json:"turn_status"`
	Output []map[string]any `json:"output_items"`
}

// ID is the task id.
func (t *Task) ID() string { return t.doc.Task.ID }

// Title is the task title.
func (t *Task) Title() string { return t.doc.Task.Title }

// Status is the state of the latest assistant turn (pending, in_progress,
// completed, failed), or "" if there is none yet.
func (t *Task) Status() string {
	if t.doc.Assistant != nil {
		return t.doc.Assistant.Status
	}
	return ""
}

// Diff is the unified diff the task produced, or "".
func (t *Task) Diff() string {
	for _, turn := range []*taskTurn{t.doc.Diff, t.doc.Assistant} {
		if turn == nil {
			continue
		}
		for _, it := range turn.Output {
			switch it["type"] {
			case "output_diff":
				if d, _ := it["diff"].(string); d != "" {
					return d
				}
			case "pr":
				od, _ := it["output_diff"].(map[string]any)
				if d, _ := od["diff"].(string); d != "" {
					return d
				}
			}
		}
	}
	return ""
}

// Messages are the assistant's text messages of the latest turn.
func (t *Task) Messages() []string {
	var out []string
	if t.doc.Assistant == nil {
		return nil
	}
	for _, it := range t.doc.Assistant.Output {
		if it["type"] != "message" {
			continue
		}
		parts, _ := it["content"].([]any)
		for _, p := range parts {
			m, _ := p.(map[string]any)
			if m["content_type"] == "text" {
				if s, _ := m["text"].(string); s != "" {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// Task fetches one cloud task. id may be a bare id or a chatgpt.com task URL.
func (c *Client) Task(ctx context.Context, id string) (*Task, error) {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	t := &Task{}
	if _, err := c.getJSON(ctx, c.chatgptURL+"/wham/tasks/"+url.PathEscape(id), &t.Raw); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(t.Raw, &t.doc); err != nil {
		return nil, fmt.Errorf("codex: decoding task: %w", err)
	}
	return t, nil
}

// NewTask describes a cloud task to start.
type NewTask struct {
	EnvironmentID string
	// Branch is the git ref to start from (default "main").
	Branch string
	Prompt string
	// Attempts is how many parallel attempts to run (best of N); 0 or 1 is one.
	Attempts int
}

// CreateTask starts a Codex cloud task and returns its id. This runs an
// agent against the environment's repository and consumes usage.
func (c *Client) CreateTask(ctx context.Context, nt NewTask) (string, error) {
	if nt.Branch == "" {
		nt.Branch = "main"
	}
	body := map[string]any{
		"new_task": map[string]any{
			"environment_id":             nt.EnvironmentID,
			"branch":                     nt.Branch,
			"run_environment_in_qa_mode": false,
		},
		"input_items": []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"content_type": "text", "text": nt.Prompt}},
		}},
	}
	if nt.Attempts > 1 {
		body["metadata"] = map[string]any{"best_of_n": nt.Attempts}
	}
	var out struct {
		ID   string `json:"id"`
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if _, err := c.callJSON(ctx, http.MethodPost, c.chatgptURL+"/wham/tasks", body, &out); err != nil {
		return "", err
	}
	if out.Task.ID != "" {
		return out.Task.ID, nil
	}
	return out.ID, nil
}
