package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cxboog/codexcli/codex"
)

// session is a conversation persisted between `cx ask` invocations. The
// backend stores nothing, so the transcript lives here and is replayed.
type session struct {
	ID           string       `json:"id"`
	Model        string       `json:"model,omitempty"`
	Instructions string       `json:"instructions,omitempty"`
	Items        []codex.Item `json:"items"`
}

func sessionPath(name string) (string, error) {
	if strings.ContainsAny(name, "/\\") || name == "" || name[0] == '.' {
		return "", fmt.Errorf("bad session name %q", name)
	}
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "codexcli", "sessions", name+".json"), nil
}

func loadSession(name string) (*session, string, error) {
	path, err := sessionPath(name)
	if err != nil {
		return nil, "", err
	}
	s := &session{ID: codex.NewID()}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, path, nil
	}
	if err != nil {
		return nil, "", err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, "", fmt.Errorf("session %s: %w", path, err)
	}
	return s, path, nil
}

func (s *session) save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func cmdAsk(ctx context.Context, args []string) error {
	cfg := codex.LoadConfig()
	fs := newFlags("ask", "ask [flags] [prompt...]   (stdin, if piped, is appended to the prompt)")
	model := fs.String("m", cfg.Model, "model slug (see 'cx models')")
	effort := fs.String("e", cfg.ReasoningEffort, "reasoning effort: none|minimal|low|medium|high|xhigh|... (model dependent)")
	system := fs.String("s", "", "system prompt (instructions); @file reads it from a file")
	var images multi
	fs.Var(&images, "i", "attach an image file or URL (repeatable)")
	search := fs.Bool("search", false, "let the model search the web")
	var mcps multi
	fs.Var(&mcps, "mcp", "give the model the tools of an MCP server (repeatable): a command line to run, an http(s) URL, or @file.json in .mcp.json format")
	maxTurns := fs.Int("max-turns", 32, "with -mcp: most model round trips before giving up")
	fast := fs.Bool("fast", cfg.ServiceTier == "priority", "use the priority service tier (burns usage faster)")
	verbosity := fs.String("verbosity", "", "answer verbosity: low|medium|high")
	schema := fs.String("schema", "", "JSON schema file; the answer will be JSON matching it")
	sess := fs.String("session", "", "named conversation to continue (created if new)")
	asJSON := fs.Bool("json", false, "print the full response as one JSON object instead of text")
	events := fs.Bool("events", false, "print raw stream events as JSON lines instead of text")
	thinking := fs.Bool("thinking", false, "show the model's reasoning summary on stderr")
	showUsage := fs.Bool("usage", false, "print token usage and rate-limit state on stderr")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	prompt, err := readPrompt(pos)
	if err != nil {
		return err
	}

	req := codex.Request{Model: *model, Verbosity: *verbosity}
	if *system != "" {
		if req.Instructions, err = textOrFile(*system); err != nil {
			return err
		}
	}
	if *effort != "" || *thinking {
		req.Reasoning = &codex.Reasoning{Effort: *effort}
		if *thinking {
			req.Reasoning.Summary = "auto"
		}
	}
	if *fast {
		req.ServiceTier = "priority"
	}
	if *search {
		req.Tools = append(req.Tools, codex.WebSearchTool(true))
	}
	if *schema != "" {
		b, err := os.ReadFile(*schema)
		if err != nil {
			return err
		}
		var sc any
		if err := json.Unmarshal(b, &sc); err != nil {
			return fmt.Errorf("%s: %w", *schema, err)
		}
		req.Schema = sc
	}

	var parts []map[string]any
	for _, im := range images {
		if strings.HasPrefix(im, "http://") || strings.HasPrefix(im, "https://") || strings.HasPrefix(im, "data:") {
			parts = append(parts, codex.ImageURL(im))
			continue
		}
		p, err := codex.ImageFile(im)
		if err != nil {
			return err
		}
		parts = append(parts, p)
	}
	turn := codex.UserMessage(prompt, parts...)

	var opts []codex.Option
	var st *session
	var stPath string
	if *sess != "" {
		if st, stPath, err = loadSession(*sess); err != nil {
			return err
		}
		opts = append(opts, codex.WithSessionID(st.ID)) // stable id keeps the prompt cache warm
		if req.Instructions == "" {
			req.Instructions = st.Instructions
		}
		req.Input = append(req.Input, st.Items...)
	}
	req.Input = append(req.Input, turn)

	c, err := codex.New(opts...)
	if err != nil {
		return err
	}
	var handler codex.ToolHandler
	if len(mcps) > 0 {
		ts, err := connectMCP(ctx, mcps)
		if err != nil {
			return err
		}
		defer ts.close()
		req.Tools = append(req.Tools, ts.tools...)
		handler = ts.handle
	}

	text := !*asJSON && !*events
	// atLineStart tracks stdout so that text from separate turns of a tool
	// loop doesn't run together.
	atLineStart, inThinking := true, false
	enc := json.NewEncoder(os.Stdout)
	onEvent := func(ev *codex.Event) {
		switch {
		case *events:
			_ = enc.Encode(ev.Raw)
		case ev.Type == codex.EventReasoningSummaryDelta && *thinking:
			if !inThinking {
				fmt.Fprint(os.Stderr, "\x1b[2m")
				inThinking = true
			}
			fmt.Fprint(os.Stderr, ev.Delta)
		case ev.Type == codex.EventTextDelta && text && ev.Delta != "":
			if inThinking {
				fmt.Fprint(os.Stderr, "\x1b[0m\n\n")
				inThinking = false
			}
			fmt.Print(ev.Delta)
			atLineStart = strings.HasSuffix(ev.Delta, "\n")
		case ev.Type == codex.EventItemDone && text:
			if inThinking {
				fmt.Fprint(os.Stderr, "\x1b[0m\n")
				inThinking = false
			}
			if !atLineStart {
				fmt.Println()
				atLineStart = true
			}
		}
	}
	resp, history, err := c.Run(ctx, req, handler, &codex.RunOptions{OnEvent: onEvent, MaxTurns: *maxTurns})
	if inThinking {
		fmt.Fprint(os.Stderr, "\x1b[0m\n")
	}
	if err != nil {
		return err
	}
	if *asJSON {
		out := struct {
			Text string `json:"text"`
			*codex.Response
		}{resp.Text(), resp}
		if err := printJSON(out); err != nil {
			return err
		}
	}
	if st != nil {
		st.Model, st.Instructions = req.Model, req.Instructions
		st.Items = history // prior items, this turn, and everything it produced
		if err := st.save(stPath); err != nil {
			return fmt.Errorf("saving session: %w", err)
		}
	}
	if *showUsage {
		printTurnUsage(resp)
	}
	return nil
}

func printTurnUsage(resp *codex.Response) {
	if u := resp.Usage; u != nil {
		fmt.Fprintf(os.Stderr, "tokens: %d in (%d cached), %d out (%d reasoning)\n",
			u.InputTokens, u.CachedTokens, u.OutputTokens, u.ReasoningTokens)
	}
	if rl := resp.RateLimits; rl != nil {
		for _, w := range []*codex.Window{rl.Primary, rl.Secondary} {
			if w != nil {
				fmt.Fprintf(os.Stderr, "limit: %s window %.0f%% used, resets %s\n",
					windowName(w.WindowMinutes*60), w.UsedPercent, w.ResetsAt.Local().Format("Mon Jan 2 15:04"))
			}
		}
	}
}

func windowName(seconds int64) string {
	switch {
	case seconds <= 0:
		return "current"
	case seconds%(7*86400) == 0 && seconds/(7*86400) == 1:
		return "weekly"
	case seconds%86400 == 0:
		return fmt.Sprintf("%dd", seconds/86400)
	case seconds%3600 == 0:
		return fmt.Sprintf("%dh", seconds/3600)
	}
	return fmt.Sprintf("%dm", seconds/60)
}

// textOrFile returns s, or the contents of the file named by "@path".
func textOrFile(s string) (string, error) {
	if path, ok := strings.CutPrefix(s, "@"); ok {
		b, err := os.ReadFile(path)
		return string(b), err
	}
	return s, nil
}
