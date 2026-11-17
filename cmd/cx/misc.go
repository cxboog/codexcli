package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cxboog/codexcli/codex"
)

func cmdImage(ctx context.Context, args []string) error {
	fs := newFlags("image", "image [flags] [prompt...]")
	out := fs.String("o", "", "output file (default image-<timestamp>.png; with -n >1 an index is added; '-' is stdout)")
	n := fs.Int("n", 1, "number of images")
	size := fs.String("size", "", "e.g. 1024x1024, 1536x1024, 1024x1536, auto")
	quality := fs.String("quality", "", "low|medium|high|auto")
	background := fs.String("background", "", "transparent|opaque|auto")
	model := fs.String("m", codex.DefaultImageModel, "image model")
	var refs multi
	fs.Var(&refs, "i", "reference image file or URL to edit or draw from (repeatable)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	prompt, err := readPrompt(pos)
	if err != nil {
		return err
	}
	req := codex.ImageRequest{Prompt: prompt, Model: *model, Size: *size, Quality: *quality, Background: *background}
	if *n > 1 {
		req.N = *n
	}
	for _, r := range refs {
		if strings.HasPrefix(r, "http://") || strings.HasPrefix(r, "https://") {
			req.Images = append(req.Images, codex.ImageURL(r))
			continue
		}
		p, err := codex.ImageFile(r)
		if err != nil {
			return err
		}
		req.Images = append(req.Images, p)
	}
	c, err := codex.New()
	if err != nil {
		return err
	}
	imgs, err := c.GenerateImages(ctx, req)
	if err != nil {
		return err
	}
	paths, err := saveImages(imgs, *out)
	if err != nil {
		return err
	}
	for _, p := range paths {
		fmt.Println(p)
	}
	return nil
}

// saveImages writes images to out (or a timestamped default) and returns
// the paths written. out "-" writes the first image to stdout.
func saveImages(imgs []codex.Image, out string) ([]string, error) {
	if out == "-" {
		_, err := os.Stdout.Write(imgs[0].Data)
		return nil, err
	}
	var paths []string
	for i, im := range imgs {
		path := out
		if path == "" {
			path = fmt.Sprintf("image-%s.%s", time.Now().Format("20060102-150405"), im.Format)
		}
		if len(imgs) > 1 {
			ext := filepath.Ext(path)
			path = fmt.Sprintf("%s-%d%s", strings.TrimSuffix(path, ext), i+1, ext)
		}
		if err := os.WriteFile(path, im.Data, 0o644); err != nil {
			return paths, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func cmdSearch(ctx context.Context, args []string) error {
	fs := newFlags("search", "search [flags] query...")
	asJSON := fs.Bool("json", false, "print results as JSON")
	raw := fs.Bool("text", false, "print the model-oriented text rendering (page excerpts) instead of a result list")
	cached := fs.Bool("cached", false, "search OpenAI's index only, no live fetches")
	var domains multi
	fs.Var(&domains, "site", "restrict to a domain (repeatable)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New("no query given")
	}
	c, err := codex.New()
	if err != nil {
		return err
	}
	res, err := c.Search(ctx, codex.SearchRequest{Queries: []string{strings.Join(pos, " ")}, Domains: domains, Cached: *cached})
	if err != nil {
		return err
	}
	switch {
	case *asJSON:
		return printJSON(res)
	case *raw:
		fmt.Println(res.Output)
	default:
		for _, h := range res.Results {
			if h.URL == "" {
				continue
			}
			fmt.Printf("%s\n  %s\n", h.Title, h.URL)
			if h.Snippet != "" {
				fmt.Printf("  %s\n", strings.Join(strings.Fields(h.Snippet), " "))
			}
			fmt.Println()
		}
	}
	return nil
}

func cmdModels(ctx context.Context, args []string) error {
	fs := newFlags("models", "models [flags]")
	asJSON := fs.Bool("json", false, "print the full catalog entries as JSON")
	all := fs.Bool("all", false, "include hidden models")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c, err := codex.New()
	if err != nil {
		return err
	}
	models, err := c.Models(ctx)
	if err != nil {
		return err
	}
	var shown []codex.Model
	for _, m := range models {
		if *all || m.Visibility != "hide" {
			shown = append(shown, m)
		}
	}
	if *asJSON {
		raws := make([]any, len(shown))
		for i, m := range shown {
			raws[i] = m.Raw
		}
		return printJSON(raws)
	}
	def := codex.LoadConfig().Model
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tCONTEXT\tEFFORT (default*)\tDESCRIPTION")
	for _, m := range shown {
		var lv []string
		for _, l := range m.ReasoningLevels {
			if l.Effort == m.DefaultReasoning {
				lv = append(lv, l.Effort+"*")
			} else {
				lv = append(lv, l.Effort)
			}
		}
		name := m.Slug
		if name == def {
			name += " (default)"
		}
		fmt.Fprintf(tw, "%s\t%dk\t%s\t%s\n", name, m.ContextWindow/1000, strings.Join(lv, " "), m.Description)
	}
	return tw.Flush()
}

func cmdUsage(ctx context.Context, args []string) error {
	fs := newFlags("usage", "usage [flags]")
	asJSON := fs.Bool("json", false, "print the raw usage payload as JSON")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c, err := codex.New()
	if err != nil {
		return err
	}
	u, err := c.Usage(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(u.Raw)
	}
	fmt.Printf("plan: %s\n", u.PlanType)
	printLimit := func(label string, l *codex.UsageLimit) {
		if l == nil {
			return
		}
		for _, w := range []*codex.UsageWindow{l.Primary, l.Secondary} {
			if w == nil {
				continue
			}
			fmt.Printf("%s%s: %.0f%% used, resets %s (in %s)\n", label, windowName(w.LimitWindowSeconds), w.UsedPercent,
				w.Resets().Local().Format("Mon Jan 2 15:04"), (time.Duration(w.ResetAfterSeconds) * time.Second).Round(time.Minute))
		}
		if l.LimitReached || !l.Allowed {
			fmt.Printf("%slimit reached\n", label)
		}
	}
	printLimit("", u.RateLimit)
	for _, a := range u.Additional {
		printLimit(a.LimitName+" ", a.RateLimit)
	}
	if cr := u.Credits; cr != nil {
		switch {
		case cr.Unlimited:
			fmt.Println("credits: unlimited")
		case cr.HasCredits:
			fmt.Printf("credits: %s\n", cr.Balance)
		default:
			fmt.Println("credits: none")
		}
	}
	return nil
}

func cmdWhoami(_ context.Context, args []string) error {
	fs := newFlags("whoami", "whoami [flags]")
	asJSON := fs.Bool("json", false, "print as JSON")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	a, err := codex.LoadAuth()
	if err != nil {
		return err
	}
	acct := a.Account()
	if *asJSON {
		return printJSON(acct)
	}
	fmt.Printf("%s (%s plan)\n", acct.Email, acct.Plan)
	fmt.Printf("account: %s\n", acct.AccountID)
	fmt.Printf("credentials: %s\n", acct.Path)
	if !acct.Expires.IsZero() {
		fmt.Printf("access token expires: %s (refreshed automatically)\n", acct.Expires.Local().Format("Mon Jan 2 15:04"))
	}
	return nil
}

func cmdToken(ctx context.Context, args []string) error {
	fs := newFlags("token", "token [flags]   (prints a bearer token for chatgpt.com/backend-api; treat it as a secret)")
	refresh := fs.Bool("refresh", false, "force a refresh first")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	a, err := codex.LoadAuth()
	if err != nil {
		return err
	}
	var tok codex.Token
	if *refresh {
		tok, err = a.Refresh(ctx, "")
	} else {
		tok, err = a.Token(ctx)
	}
	if err != nil {
		return err
	}
	fmt.Println(tok.AccessToken)
	return nil
}

func cmdAPI(ctx context.Context, args []string) error {
	fs := newFlags("api", `api [flags] METHOD PATH [BODY]

PATH is relative to the codex endpoint root (e.g. /models), or starts with
/backend-api/ for the rest of the ChatGPT backend (e.g. /backend-api/wham/usage).
BODY is JSON, @file, or - for stdin.`)
	var hdrs multi
	fs.Var(&hdrs, "H", "extra header 'Name: value' (repeatable)")
	include := fs.Bool("include", false, "print status and response headers on stderr")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 || len(pos) > 3 {
		fs.Usage()
		return errors.New("need METHOD and PATH")
	}
	var body []byte
	if len(pos) == 3 {
		switch {
		case pos[2] == "-":
			body, err = io.ReadAll(os.Stdin)
		case strings.HasPrefix(pos[2], "@"):
			body, err = os.ReadFile(pos[2][1:])
		default:
			body = []byte(pos[2])
		}
		if err != nil {
			return err
		}
	}
	h := http.Header{}
	for _, kv := range hdrs {
		k, v, ok := strings.Cut(kv, ":")
		if !ok {
			return fmt.Errorf("bad header %q", kv)
		}
		h.Add(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	c, err := codex.New()
	if err != nil {
		return err
	}
	resp, err := c.Raw(ctx, strings.ToUpper(pos[0]), pos[1], body, h)
	if err != nil {
		var api *codex.APIError
		if errors.As(err, &api) && api.Body != "" {
			fmt.Fprintln(os.Stdout, api.Body) // the body is the useful part here
		}
		return err
	}
	defer resp.Body.Close()
	if *include {
		fmt.Fprintln(os.Stderr, resp.Status)
		_ = resp.Header.Write(os.Stderr)
		fmt.Fprintln(os.Stderr)
	}
	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}

func cmdTasks(ctx context.Context, args []string) error {
	const synopsis = `tasks <subcommand>

  tasks list [-n N] [-env ID] [-json]      recent cloud tasks
  tasks show ID [-json]                    status and messages of a task
  tasks diff ID                            the task's diff (pipe to 'git apply')
  tasks envs [-json]                       cloud environments
  tasks new -env ID [-branch B] [-attempts N] prompt...   start a task`
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, "usage: cx "+synopsis)
		return nil
	}
	sub, args := args[0], args[1:]
	fs := newFlags("tasks "+sub, synopsis)
	asJSON := fs.Bool("json", false, "print as JSON")
	n := fs.Int("n", 20, "how many tasks to list")
	env := fs.String("env", "", "environment id")
	branch := fs.String("branch", "main", "git ref to start from")
	attempts := fs.Int("attempts", 1, "parallel attempts (best of N)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	c, err := codex.New()
	if err != nil {
		return err
	}
	needID := func() (string, error) {
		if len(pos) != 1 {
			return "", errors.New("need exactly one task id")
		}
		return pos[0], nil
	}
	switch sub {
	case "list":
		list, err := c.Tasks(ctx, *n, *env, "")
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(list)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, it := range list.Items {
			status := ""
			if d, ok := it["task_status_display"].(map[string]any); ok {
				if lt, ok := d["latest_turn_status_display"].(map[string]any); ok {
					status, _ = lt["turn_status"].(string)
				}
			}
			fmt.Fprintf(tw, "%v\t%s\t%v\n", it["id"], status, it["title"])
		}
		return tw.Flush()
	case "show":
		id, err := needID()
		if err != nil {
			return err
		}
		t, err := c.Task(ctx, id)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(t.Raw)
		}
		fmt.Printf("%s  [%s]\n%s\n", t.ID(), t.Status(), t.Title())
		for _, m := range t.Messages() {
			fmt.Printf("\n%s\n", m)
		}
		if t.Diff() != "" {
			fmt.Printf("\n(has a diff: cx tasks diff %s)\n", t.ID())
		}
	case "diff":
		id, err := needID()
		if err != nil {
			return err
		}
		t, err := c.Task(ctx, id)
		if err != nil {
			return err
		}
		if t.Diff() == "" {
			return fmt.Errorf("task %s has no diff (status: %s)", t.ID(), t.Status())
		}
		fmt.Print(t.Diff())
	case "envs":
		envs, err := c.Environments(ctx)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(envs)
		}
		for _, e := range envs {
			fmt.Printf("%s\t%s\n", e.ID, e.Label)
		}
	case "new":
		if *env == "" {
			return errors.New("-env is required (see 'cx tasks envs')")
		}
		prompt, err := readPrompt(pos)
		if err != nil {
			return err
		}
		id, err := c.CreateTask(ctx, codex.NewTask{EnvironmentID: *env, Branch: *branch, Prompt: prompt, Attempts: *attempts})
		if err != nil {
			return err
		}
		fmt.Println(id)
	default:
		return fmt.Errorf("unknown tasks subcommand %q", sub)
	}
	return nil
}
