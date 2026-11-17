package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Reasoning controls how hard the model thinks and whether it reports a
// summary of its reasoning.
type Reasoning struct {
	// Effort: none, minimal, low, medium, high, xhigh (model dependent; see
	// Model.ReasoningLevels).
	Effort string `json:"effort,omitempty"`
	// Summary: auto, concise, detailed. Empty omits summaries.
	Summary string `json:"summary,omitempty"`
}

// Request is a Responses call. Zero values mean "let the client pick".
type Request struct {
	Model string
	// Instructions is the system prompt.
	Instructions string
	Input        []Item
	Tools        []Tool
	// ToolChoice defaults to "auto".
	ToolChoice any
	// ParallelToolCalls defaults to true when tools are present.
	ParallelToolCalls *bool
	Reasoning         *Reasoning
	// Verbosity: low, medium, high (models with verbosity support only).
	Verbosity string
	// Schema, if set, forces the answer to be JSON matching this JSON schema.
	Schema     any
	SchemaName string
	// ServiceTier: "priority" (fast mode) or "flex". Empty is the default tier.
	ServiceTier string
	// PromptCacheKey defaults to the client's session id.
	PromptCacheKey string
	// Include defaults to ["reasoning.encrypted_content"], which is what makes
	// reasoning items replayable on the next turn.
	Include []string
	// Extra is merged over the generated body, for fields not modelled here.
	Extra map[string]any
}

// MarshalJSON renders the wire body. The backend only accepts streaming,
// non-stored requests, so those are fixed.
func (r Request) MarshalJSON() ([]byte, error) {
	input := r.Input
	if input == nil {
		input = []Item{}
	}
	body := map[string]any{
		"model":  r.Model,
		"stream": true,
		"store":  false,
		"input":  input,
	}
	if r.Instructions != "" {
		body["instructions"] = r.Instructions
	}
	if len(r.Tools) > 0 {
		body["tools"] = r.Tools
		body["tool_choice"] = "auto"
		body["parallel_tool_calls"] = true
		if r.ToolChoice != nil {
			body["tool_choice"] = r.ToolChoice
		}
		if r.ParallelToolCalls != nil {
			body["parallel_tool_calls"] = *r.ParallelToolCalls
		}
	}
	if r.Reasoning != nil && (r.Reasoning.Effort != "" || r.Reasoning.Summary != "") {
		body["reasoning"] = r.Reasoning
	}
	if r.Include != nil {
		body["include"] = r.Include
	} else {
		body["include"] = []string{"reasoning.encrypted_content"}
	}
	text := map[string]any{}
	if r.Verbosity != "" {
		text["verbosity"] = r.Verbosity
	}
	if r.Schema != nil {
		name := r.SchemaName
		if name == "" {
			name = "output"
		}
		text["format"] = map[string]any{"type": "json_schema", "name": name, "strict": true, "schema": r.Schema}
	}
	if len(text) > 0 {
		body["text"] = text
	}
	if r.ServiceTier != "" && r.ServiceTier != "default" {
		body["service_tier"] = r.ServiceTier
	}
	if r.PromptCacheKey != "" {
		body["prompt_cache_key"] = r.PromptCacheKey
	}
	for k, v := range r.Extra {
		if v == nil {
			delete(body, k)
		} else {
			body[k] = v
		}
	}
	return json.Marshal(body)
}

// Usage is the token accounting of one response.
type Usage struct {
	InputTokens     int64 `json:"input_tokens"`
	CachedTokens    int64 `json:"cached_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
}

func (u *Usage) UnmarshalJSON(b []byte) error {
	var w struct {
		In  int64 `json:"input_tokens"`
		InD struct {
			Cached int64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		Flat int64 `json:"cached_tokens"`
		Out  int64 `json:"output_tokens"`
		OutD struct {
			Reasoning int64 `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
		FlatR int64 `json:"reasoning_tokens"`
		Total int64 `json:"total_tokens"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*u = Usage{w.In, w.InD.Cached + w.Flat, w.Out, w.OutD.Reasoning + w.FlatR, w.Total}
	return nil
}

// Response is a finished model turn.
type Response struct {
	ID string `json:"id"`
	// Model is the model the server says it used.
	Model  string `json:"model,omitempty"`
	Status string `json:"status,omitempty"`
	// Output is every item the model produced, in order. Append it to the
	// input (plus your tool outputs) to continue the conversation.
	Output     []Item      `json:"output"`
	Usage      *Usage      `json:"usage,omitempty"`
	RateLimits *RateLimits `json:"rate_limits,omitempty"`
	RequestID  string      `json:"request_id,omitempty"`
	// Envelope is the server's own final response object, verbatim. Its
	// output list is usually empty on this backend; use Output.
	Envelope json.RawMessage `json:"-"`
}

// Text is the model's answer: the text of all assistant messages.
func (r *Response) Text() string {
	var b bytes.Buffer
	for _, it := range r.Output {
		if it.Type() == "message" {
			b.WriteString(it.Text())
		}
	}
	return b.String()
}

// ReasoningSummary is the concatenated reasoning summaries, if requested.
func (r *Response) ReasoningSummary() string {
	var b bytes.Buffer
	for _, it := range r.Output {
		if it.Type() == "reasoning" {
			if t := it.Text(); t != "" {
				if b.Len() > 0 {
					b.WriteString("\n\n")
				}
				b.WriteString(t)
			}
		}
	}
	return b.String()
}

// FunctionCalls lists the tool calls waiting for an answer.
func (r *Response) FunctionCalls() []FunctionCall {
	var out []FunctionCall
	for _, it := range r.Output {
		if fc, ok := it.AsFunctionCall(); ok {
			out = append(out, fc)
		}
	}
	return out
}

// Image is one generated image.
type Image struct {
	Data          []byte
	RevisedPrompt string
	Format        string
}

// Images decodes the results of image_generation tool calls.
func (r *Response) Images() []Image {
	var out []Image
	for _, it := range r.Output {
		if it.Type() != "image_generation_call" {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(it.str("result"))
		if err != nil || len(data) == 0 {
			continue
		}
		f := it.str("output_format")
		if f == "" {
			f = "png"
		}
		out = append(out, Image{Data: data, RevisedPrompt: it.str("revised_prompt"), Format: f})
	}
	return out
}

// Event is one server-sent event of a response stream.
type Event struct {
	// Type is the event type, e.g. response.output_text.delta.
	Type string
	// Delta is the text fragment of *.delta events.
	Delta string
	// Item is set on response.output_item.added / .done.
	Item Item
	// Raw is the full event payload.
	Raw json.RawMessage
}

// Common event types.
const (
	EventTextDelta             = "response.output_text.delta"
	EventReasoningSummaryDelta = "response.reasoning_summary_text.delta"
	EventReasoningDelta        = "response.reasoning_text.delta"
	EventItemAdded             = "response.output_item.added"
	EventItemDone              = "response.output_item.done"
	EventCompleted             = "response.completed"
)

// IdleTimeout is how long a stream may stay silent before it is abandoned.
var IdleTimeout = 5 * time.Minute

// Stream is an in-flight response. Call Next until io.EOF, then Response;
// or just call Wait.
type Stream struct {
	body   io.ReadCloser
	rd     *bufio.Reader
	cancel context.CancelFunc
	idle   *time.Timer
	resp   Response
	done   bool
	err    error
}

// Stream starts a response and returns its event stream. The caller must
// drain it or Close it.
func (c *Client) Stream(ctx context.Context, req Request) (*Stream, error) {
	if req.PromptCacheKey == "" {
		req.PromptCacheKey = c.sessionID
	}
	if req.Extra == nil || req.Extra["client_metadata"] == nil {
		meta := map[string]string{"session_id": c.sessionID, "thread_id": c.sessionID}
		if c.installID != "" {
			meta["x-codex-installation-id"] = c.installID
		}
		extra := map[string]any{"client_metadata": meta}
		for k, v := range req.Extra {
			extra[k] = v
		}
		req.Extra = extra
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return c.StreamRaw(ctx, body)
}

// StreamRaw is Stream with a caller-built JSON body, sent untouched.
func (c *Client) StreamRaw(ctx context.Context, body []byte) (*Stream, error) {
	ctx, cancel := context.WithCancel(ctx)
	hdr := http.Header{
		"Accept":              {"text/event-stream"},
		"Session-Id":          {c.sessionID},
		"Thread-Id":           {c.sessionID},
		"X-Client-Request-Id": {c.sessionID},
	}
	resp, err := c.do(ctx, http.MethodPost, c.baseURL+"/responses", body, hdr)
	if err != nil {
		cancel()
		return nil, err
	}
	s := &Stream{body: resp.Body, rd: bufio.NewReaderSize(resp.Body, 64<<10), cancel: cancel}
	s.resp.RateLimits = parseRateLimitHeaders(resp.Header)
	s.resp.Model = resp.Header.Get("openai-model")
	s.resp.RequestID = resp.Header.Get("x-request-id")
	if s.resp.RequestID == "" {
		s.resp.RequestID = resp.Header.Get("x-oai-request-id")
	}
	s.idle = time.AfterFunc(IdleTimeout, cancel)
	return s, nil
}

// Close abandons the stream.
func (s *Stream) Close() error {
	s.idle.Stop()
	s.cancel()
	return s.body.Close()
}

// Next returns the next event, or io.EOF once the response has completed.
// Failures the server reports in-stream come back as *APIError.
func (s *Stream) Next() (*Event, error) {
	if s.err != nil {
		return nil, s.err
	}
	ev, err := s.next()
	if err != nil {
		s.err = err
		s.Close()
	}
	return ev, err
}

func (s *Stream) next() (*Event, error) {
	if s.done {
		return nil, io.EOF
	}
	for {
		data, err := s.readData()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errors.New("codex: stream closed before response.completed")
			}
			if !s.idle.Stop() {
				return nil, fmt.Errorf("codex: stream idle for %s: %w", IdleTimeout, err)
			}
			return nil, fmt.Errorf("codex: reading stream: %w", err)
		}
		s.idle.Reset(IdleTimeout)
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		var w struct {
			Type     string          `json:"type"`
			Delta    json.RawMessage `json:"delta"`
			Item     Item            `json:"item"`
			Response json.RawMessage `json:"response"`
			Error    json.RawMessage `json:"error"`
			Message  string          `json:"message"`
			Code     string          `json:"code"`
		}
		if json.Unmarshal(data, &w) != nil || w.Type == "" {
			continue // codex skips lines it can't parse, so do we
		}
		ev := &Event{Type: w.Type, Item: w.Item, Raw: data}
		_ = json.Unmarshal(w.Delta, &ev.Delta) // non-string deltas stay in Raw
		switch w.Type {
		case EventItemDone:
			if w.Item != nil {
				s.resp.Output = append(s.resp.Output, w.Item)
			}
		case "response.created":
			s.absorb(w.Response)
		case EventCompleted:
			s.absorb(w.Response)
			s.resp.Envelope = w.Response
			s.done = true
		case "response.incomplete":
			s.absorb(w.Response)
			var r struct {
				D struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
			}
			_ = json.Unmarshal(w.Response, &r)
			return nil, &APIError{Code: "incomplete", Message: "response incomplete: " + r.D.Reason, RequestID: s.resp.RequestID, Body: string(data)}
		case "response.failed":
			s.absorb(w.Response)
			var r struct {
				Error json.RawMessage `json:"error"`
			}
			_ = json.Unmarshal(w.Response, &r)
			e := &APIError{RequestID: s.resp.RequestID, Body: string(data), RateLimits: s.resp.RateLimits}
			fillErrorFields(e, r.Error)
			return nil, e
		case "error":
			e := &APIError{RequestID: s.resp.RequestID, Body: string(data), RateLimits: s.resp.RateLimits}
			fillErrorFields(e, w.Error)
			if e.Message == "" {
				e.Message = w.Message
			}
			if e.Code == "" {
				e.Code = w.Code
			}
			return nil, e
		}
		return ev, nil
	}
}

// absorb takes over id/status/usage (and output, if the server lists any we
// didn't see as item events) from a response envelope.
func (s *Stream) absorb(raw json.RawMessage) {
	var r struct {
		ID     string `json:"id"`
		Model  string `json:"model"`
		Status string `json:"status"`
		Output []Item `json:"output"`
		Usage  *Usage `json:"usage"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return
	}
	if r.ID != "" {
		s.resp.ID = r.ID
	}
	if r.Model != "" && s.resp.Model == "" {
		s.resp.Model = r.Model
	}
	if r.Status != "" {
		s.resp.Status = r.Status
	}
	if r.Usage != nil {
		s.resp.Usage = r.Usage
	}
	if len(s.resp.Output) == 0 && len(r.Output) > 0 {
		s.resp.Output = r.Output
	}
}

// readData returns the data payload of the next SSE event.
func (s *Stream) readData() ([]byte, error) {
	var data []byte
	for {
		line, err := s.rd.ReadBytes('\n')
		if err != nil && (len(line) == 0 || !errors.Is(err, io.EOF)) {
			return nil, err
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(line) == 0 {
			if data != nil {
				return data, nil
			}
			if err != nil {
				return nil, err
			}
			continue
		}
		if v, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			v = bytes.TrimPrefix(v, []byte(" "))
			if data != nil {
				data = append(data, '\n')
			}
			data = append(data, v...)
		}
		if err != nil { // EOF right after a final unterminated line
			if data != nil {
				return data, nil
			}
			return nil, err
		}
	}
}

// Response is the result so far; complete once Next has returned io.EOF.
func (s *Stream) Response() *Response { return &s.resp }

// Wait drains the stream, calling on (if non-nil) for every event, and
// returns the finished response.
func (s *Stream) Wait(on func(*Event)) (*Response, error) {
	defer s.Close()
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			return &s.resp, nil
		}
		if err != nil {
			return &s.resp, err
		}
		if on != nil {
			on(ev)
		}
	}
}

// Respond runs one request to completion.
func (c *Client) Respond(ctx context.Context, req Request) (*Response, error) {
	s, err := c.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	return s.Wait(nil)
}

// ToolHandler answers one tool call with a string, or with a list of content
// parts (input_text / input_image maps, see ImageURL) for rich results. A
// returned error is reported to the model as the tool's output rather than
// aborting the run.
type ToolHandler func(ctx context.Context, call FunctionCall) (any, error)

// RunOptions tunes Run.
type RunOptions struct {
	// OnEvent sees every stream event of every turn.
	OnEvent func(*Event)
	// MaxTurns caps model round trips (default 32).
	MaxTurns int
}

// Run drives the tool loop: it sends req, answers the model's function calls
// with handler, and repeats until the model stops calling tools. It returns
// the last response and the full transcript (req.Input plus everything
// produced), which can be used as the Input of a follow-up request.
func (c *Client) Run(ctx context.Context, req Request, handler ToolHandler, opt *RunOptions) (*Response, []Item, error) {
	if opt == nil {
		opt = &RunOptions{}
	}
	max := opt.MaxTurns
	if max <= 0 {
		max = 32
	}
	history := append([]Item(nil), req.Input...)
	total := &Usage{}
	for turn := 0; ; turn++ {
		req.Input = history
		s, err := c.Stream(ctx, req)
		if err != nil {
			return nil, history, err
		}
		resp, err := s.Wait(opt.OnEvent)
		if err != nil {
			return resp, history, err
		}
		history = append(history, resp.Output...)
		if u := resp.Usage; u != nil {
			total.InputTokens += u.InputTokens
			total.CachedTokens += u.CachedTokens
			total.OutputTokens += u.OutputTokens
			total.ReasoningTokens += u.ReasoningTokens
			total.TotalTokens += u.TotalTokens
			resp.Usage = total
		}
		calls := resp.FunctionCalls()
		if len(calls) == 0 || handler == nil {
			return resp, history, nil
		}
		if turn+1 >= max {
			return resp, history, fmt.Errorf("codex: tool loop did not finish in %d turns", max)
		}
		for _, call := range calls {
			out, err := handler(ctx, call)
			if err != nil {
				if ctx.Err() != nil {
					return resp, history, ctx.Err()
				}
				out = "error: " + err.Error()
			}
			if out == nil {
				out = ""
			}
			history = append(history, call.Output(out))
		}
	}
}
