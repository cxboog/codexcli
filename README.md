# codexcli

Your ChatGPT subscription as a Go library (`codex`) and a CLI (`cx`), using the
credentials the codex CLI already keeps in `~/.codex/auth.json`.

It talks to the same private backend codex does (`chatgpt.com/backend-api`).
That API is undocumented and can change without notice; this mirrors what the
open-source codex client sends as of November 2026.

    go install github.com/cxboog/codexcli/cmd/cx@latest

Log in with `codex login` first. Tokens are refreshed and written back in a way
that cooperates with a running codex (same lock files, unknown fields kept).

## CLI

    cx ask "explain this error" < build.log        # stdin is appended to the prompt
    cx ask -m gpt-5.6-luna -e low "quick question"
    cx ask -search "latest stable Go release?"     # model may search the web
    cx ask -i screenshot.png "what's wrong here?"
    cx ask -schema schema.json "largest city in Japan"   # JSON matching the schema
    cx ask -session review "first message"         # named, persisted conversation
    cx ask -mcp "npx -y some-mcp-server" "..."     # give the model an MCP server's tools
    cx ask -json ...                               # full response object
    cx ask -events ...                             # raw stream events, one JSON per line

    cx image -o cat.png -quality low "a cat"       # -i ref.png to edit
    cx search -site go.dev "release history"       # search without a model
    cx models                                      # what your plan can use
    cx usage                                       # limits and reset times
    cx whoami
    cx tasks list | show ID | diff ID | envs | new -env ID "prompt"   # Codex cloud
    cx api GET /backend-api/wham/usage             # raw authenticated request
    cx token                                       # a fresh bearer token

`-mcp` is repeatable and takes a command line (stdio server), an `http(s)://`
URL (streamable HTTP), or `@file.json` in `.mcp.json` format (which is where
headers and env go). The model then calls those tools on its own until it is
done; each call is logged to stderr. There is no approval step, so only hand
it servers you'd let it use unattended.

Defaults for model, effort and fast mode come from `~/.codex/config.toml`.
Exit status is 3 when the plan's usage limit is hit, 1 for other failures.
If stdin is a pipe that never closes (some harnesses), pass `-no-stdin`.

### For other programs and harnesses

`cx serve` runs a local OpenAI-compatible server:

    cx serve                      # http://127.0.0.1:8787/v1
    OPENAI_BASE_URL=http://127.0.0.1:8787/v1 OPENAI_API_KEY=x your-tool

It serves `/v1/responses`, `/v1/chat/completions` (text, images, tools, JSON
schema, streaming) and `/v1/models`. The backend is stateless, so
`previous_response_id` is rejected; `temperature`, `top_p` and a few other
fields it refuses are dropped.

`cx mcp` is an MCP server on stdio with tools `ask`, `web_search`,
`generate_image`, `models` and `usage`:

    claude mcp add cx -- cx mcp

## Library

```go
c, err := codex.New()

resp, err := c.Respond(ctx, codex.Request{
	Model: "gpt-5.6-terra",
	Input: []codex.Item{codex.UserMessage("hello")},
})
fmt.Println(resp.Text())
```

Streaming: `c.Stream(ctx, req)` then `Next()` until `io.EOF`, or `Wait(func(*Event))`.

Tools: `c.Run(ctx, req, handler, nil)` drives the function-call loop and returns
the transcript. To continue a conversation yourself, append `resp.Output` (and
your tool outputs) to the next request's `Input`; nothing is stored server-side.

The `mcp` package is the small MCP client behind `cx ask -mcp`.

Also: `Models`, `Usage`, `GenerateImages`, `Search`, `Tasks`/`Task`/`CreateTask`/
`Environments`, and `Raw` for any endpoint not wrapped. Errors from the backend
are `*codex.APIError` (`UsageLimited()`, `ResetsAt`); a dead refresh token is
`*codex.RefreshError`.

## Not covered

Realtime voice, codex's keyring credential store (`cli_auth_credentials_store`
must be `file`, the default), logging in (use `codex login`), and an
Anthropic-style `/v1/messages` endpoint.
