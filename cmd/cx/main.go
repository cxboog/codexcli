// cx exposes a ChatGPT subscription (via codex's stored credentials) as a
// scriptable command line tool.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cxboog/codexcli/codex"
)

const usageText = `cx - your ChatGPT subscription, as a command line tool

usage: cx <command> [flags] [args]

  ask      send a prompt to a model (text, images, web search, JSON schema, sessions)
  image    generate or edit images
  search   web search, without a model in the loop
  models   list the models your plan can use
  usage    show usage against your plan's limits
  whoami   show the logged-in account
  tasks    list, inspect and start Codex cloud tasks
  serve    run a local OpenAI-compatible API server (responses + chat completions)
  mcp      run as an MCP server on stdio, for other agent harnesses
  api      make a raw authenticated request to the backend
  token    print a fresh access token

Run 'cx <command> -h' for the flags of a command. Credentials come from
codex's auth.json ($CODEX_HOME or ~/.codex); log in with 'codex login'.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmds := map[string]func(context.Context, []string) error{
		"ask": cmdAsk, "image": cmdImage, "search": cmdSearch, "models": cmdModels,
		"usage": cmdUsage, "whoami": cmdWhoami, "tasks": cmdTasks, "serve": cmdServe,
		"mcp": cmdMCP, "api": cmdAPI, "token": cmdToken,
	}
	name := os.Args[1]
	switch name {
	case "-h", "--help", "help":
		fmt.Print(usageText)
		return
	}
	cmd, ok := cmds[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "cx: unknown command %q\n\n%s", name, usageText)
		os.Exit(2)
	}
	if err := cmd(ctx, os.Args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "cx:", strings.TrimPrefix(err.Error(), "codex: "))
		code := 1
		var api *codex.APIError
		if errors.As(err, &api) && api.UsageLimited() {
			code = 3 // scripts can tell "out of quota" from other failures
		}
		os.Exit(code)
	}
}

// parse parses flags that may appear before, between or after positional
// arguments, and returns the positionals.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(os.Stderr)
	var pos, tail []string
	for i, a := range args {
		if a == "--" {
			args, tail = args[:i], args[i+1:]
			break
		}
	}
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return append(pos, tail...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func newFlags(name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: cx %s\n\n", synopsis)
		fs.PrintDefaults()
	}
	fs.BoolVar(&noStdin, "no-stdin", false, "never read the prompt from stdin, even if it is piped")
	return fs
}

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// noStdin is set by -no-stdin, for callers whose stdin is a pipe that
// never closes.
var noStdin bool

// stdinPiped reports whether stdin is a pipe or file, i.e. something that
// was deliberately fed to us (not a terminal, /dev/null or a socket).
func stdinPiped() bool {
	st, err := os.Stdin.Stat()
	return err == nil && (st.Mode()&os.ModeNamedPipe != 0 || st.Mode().IsRegular())
}

// readPrompt joins the positional words and, if stdin is piped, appends it.
func readPrompt(pos []string) (string, error) {
	prompt := strings.Join(pos, " ")
	if !noStdin && stdinPiped() {
		hint := time.AfterFunc(5*time.Second, func() {
			fmt.Fprintln(os.Stderr, "cx: still reading the prompt from stdin (pass -no-stdin if that is not intended)")
		})
		b, err := io.ReadAll(os.Stdin)
		hint.Stop()
		if err != nil {
			return "", err
		}
		if in := strings.TrimSpace(string(b)); in != "" {
			if prompt != "" {
				prompt += "\n\n"
			}
			prompt += in
		}
	}
	if prompt == "" {
		return "", errors.New("no prompt given (pass it as arguments or on stdin)")
	}
	return prompt, nil
}
