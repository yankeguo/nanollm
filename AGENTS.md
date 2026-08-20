# Agent guide

Reply in the same language as the user.

nanollm is a small Go (1.26+) OpenAI- and Anthropic-compatible LLM reverse proxy. Treat the code as source of truth. Keep changes focused; do not add frameworks or extra services unless asked.

## Layout

Single `package main`. No `internal/` split unless the tree clearly outgrows one package.

| File | Role |
|---|---|
| `main.go` | Flags, listen address, graceful shutdown (`github.com/yankeguo/rg`) |
| `config.go` | YAML load/validate |
| `auth.go` | Incoming API keys; do not forward client credentials |
| `server.go` | `net/http` mux (`GET /healthz` unauthenticated) |
| `admin.go` / `admin_auth.go` | Admin cookie login, usage dashboard, call viewer |
| `admin/*.html` | Embedded HTML for `/admin` |
| `proxy.go` | Body rewrite, ordered provider attempts, response copy, call logging |
| `failover.go` | `isCatastrophic` — only then try the next provider |
| `rewrite.go` | Patch top-level `model` (and OpenAI chat streaming `stream_options.include_usage`); copy other JSON fields as `json.RawMessage` |
| `usage.go` | Parse OpenAI- and Anthropic-compatible `usage` (including cache fields and Responses `response.usage`) from JSON/SSE |
| `db.go` | GORM MySQL: `llm_calls` AutoMigrate, Record, prune detail blobs |

Tests live next to the code (`*_test.go`). Use `github.com/stretchr/testify`. Prefer extending an existing test file over adding a new one for the same area.

## Hard constraints

- **std `net/http` only** for the server. No Fiber/Gin/Echo.
- **Failover is cache-preserving.** Next provider only on catastrophic unavailability: dial/DNS/TLS/other transport failure, or HTTP 502/503/504. Do **not** fail over on 429, 4xx, 500, or timeouts after the provider was reached. Once the client response has started, never try another provider.
- Config shape is `mysql.{dsn,detail_retain}`, `admin.{username,password}`, `api_keys[].{name,value}` and `models[].{name,providers[]}` with `providers[].{name,model,headers,openai,responses,anthropic}`. Protocol blocks are optional `{url,model,headers}`. Legacy top-level `format`+`url` is still accepted and normalized into the matching block. `url` is the full `http`/`https` upstream endpoint, not a base URL. Auth to upstream belongs in `headers`.
- `providers[].name` is the vendor: unique within a model, failover order, and `llm_calls.provider`. OpenAI chat/completions/embeddings routes only attempt providers that have an `openai` block; `POST /v1/responses` only attempts those with a `responses` block; `POST /v1/messages` only attempts those with an `anthropic` block. Skip vendors missing that block; do **not** convert bodies or fail over to the other protocol. Missing matching providers → 404.
- Request rewrite only patches `model` and, for OpenAI chat streaming, `stream_options.include_usage`. Responses and Anthropic bodies are not given `stream_options`. Other JSON fields must be copied as raw values (`json.RawMessage`), not `map[string]any` round-tripped. Upstream responses are copied byte-for-byte; usage parsing is inspect-only. Streaming copies emit SSE comments while the upstream body is idle so long thinking turns do not die on client/proxy idle timeouts. There is no upstream `Timeout` / `ResponseHeaderTimeout`; wait as long as the client stays connected. Client cancel after HTTP 200 is logged as `canceled`, not a transport failure.
- MySQL is required. DSN params `charset=utf8mb4`, `parseTime=true`, `loc=UTC`, `time_zone='UTC'` are forced. All DB timestamps are UTC.
- `/admin` is a cookie-authenticated dashboard (HMAC, HttpOnly, SameSite=Lax; Secure when TLS or `X-Forwarded-Proto: https`). It is not API-key auth. `admin.username` and `admin.password` are required.
- Every provider attempt (success, 4xx/5xx, failover, dial failure, rewrite error, client cancel) is inserted into `llm_calls`. Detail JSON blobs are kept only for the latest `mysql.detail_retain` rows (default 1000).
- Incoming `Authorization` / `X-Api-Key` / `Api-Key` authenticate against `api_keys` and must not be copied upstream. Client `Cookie` is stripped from upstream requests; upstream `Set-Cookie` is dropped from responses.
- At least one API key is required; missing/invalid keys → 401 except `GET /healthz` and `/admin*`.
- Do not commit `config.yaml` (secrets). Change `config.example.yaml` and README when the config schema changes.
- Images: `ghcr.io/${{ github.repository }}`. Push `main` → `latest`; push a git tag → that tag. Workflow: `.github/workflows/release.yml`.

## Style

- Match nearby files: `log` package, table-driven tests, `httptest` for HTTP.
- `main` may use `rg.Must` / `rg.Guard`; library-ish functions return `error`.
- Keep YAML config the only user-facing knobs besides listen/config flags.
- Public docs and fixtures: placeholders (`example.com`, `sk-REPLACE_ME`), never real secrets.
- Do not start MySQL in unit tests; inject `CallLogger` (nil is a no-op).

## Verify

```bash
go test ./...
gofmt -w .
```

After config, failover, auth, or call-logging changes, update `README.md` (operators) and this file (agents) in the same change when behavior diverges from what they currently say.
