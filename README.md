# usend CLI

The command-line tool for uSend: send email, follow delivery, and manage domains, webhooks and API keys from a terminal,
a script or an AI agent. Standard library only; it talks to the API through the Go SDK in `../sdk/go`.

```bash
go build -o usend ./cmd/usend      # or: go install ./cmd/usend
usend login                        # asks for an API key, checks it, saves it (0600)
usend send --from hello@acme.com --to delivered@test.usend.et --subject Hi --text "It works"
usend domains get acme.com         # DNS records as a table
usend doctor                       # config, network and key checks
```

Commands: `login`, `logout`, `whoami`, `send`, `emails list|get|events|cancel`, `domains list|get|add|verify`,
`inbound list|get`, `webhooks list|create|delete`, `api-keys list|create|delete`, `logs list`, `doctor`, `version`.
Every command takes `--help`, `--json`, `--profile` and `--base-url`. Output is a table in a terminal and the API's JSON
when stdout is not a terminal or with `--json`. Errors go to stderr; exit status 1 for API and network errors, 2 for usage
errors. `USEND_API_KEY` and `USEND_BASE_URL` override the saved profile (an explicit `--profile` wins over the variable).

Customer documentation: `web/docs/content/tools/cli.md` (served at `/docs/tools/cli`).

## Layout

- `cmd/usend`: `main`, a one-liner.
- `internal/app`: everything else. `Main(args, *Env)` runs one invocation against injected streams, environment, config
  directory and HTTP client, so the tests drive it end to end against an `httptest` API.

## Development

```bash
go vet ./... && go test ./...
go build -ldflags "-X usend.et/cli/internal/app.Version=1.0.0" -o usend ./cmd/usend
```

`go.mod` replaces `usend.et/sdk/go` with `../sdk/go`. `go install usend.et/cli/cmd/usend@latest` will only work once the
CLI is published as its own module without that `replace` line (Go refuses to install a module that has one) and
`usend.et/cli` answers `?go-get=1` like the SDK's vanity path does (`web/landing/go-import.html`).
