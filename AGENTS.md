# Agent guide

Reply in the same language as the user.

nanollm is a small Go (1.26+) OpenAI-compatible LLM reverse proxy. Treat the code as source of truth. Keep changes focused; do not add frameworks or extra services unless asked.

## Layout

Single `package main`. No `internal/` split unless the tree clearly outgrows one package.

| File | Role |
|---|---|
| `main.go` | Flags, listen address, graceful shutdown (`github.com/yankeguo/rg`) |
| `config.go` | YAML load/validate |
| `auth.go` | Incoming API keys; do not forward client credentials |
| `server.go` | `net/http` mux (`GET /healthz` unauthenticated) |
| `proxy.go` | Body rewrite, ordered provider attempts, response copy |
| `failover.go` | `isCatastrophic` — only then try the next provider |
| `rewrite.go` | Replace `model`; inject `stream_options.include_usage` when streaming |
| `usage.go` | Parse OpenAI-compatible `usage` (including cache fields) from JSON/SSE |
| `metrics.go` | Custom OTEL counter `nanollm.token.usage` over OTLP HTTP |

Tests live next to the code (`*_test.go`). Use `github.com/stretchr/testify`. Prefer extending an existing test file over adding a new one for the same area.

## Hard constraints

- **std `net/http` only** for the server. No Fiber/Gin/Echo.
- **Do not use OpenTelemetry GenAI semantic conventions** (`gen_ai.*`). Token stats are `nanollm.token.usage` with `nanollm.model`, `nanollm.provider`, `nanollm.api_key`, `nanollm.upstream.model`, `nanollm.token.type`.
- `nanollm.token.type` values: `input`, `output`, `cache_read`, `cache_creation`, `uncached`. `input` **includes** cache; overlapping types are intentional so dashboards can show totals and cache hit rate.
- **Failover is cache-preserving.** Next provider only on catastrophic unavailability: dial/DNS/TLS/other transport failure, or HTTP 502/503/504. Do **not** fail over on 429, 4xx, 500, or timeouts after the provider was reached.
- Config shape is `api_keys[].{name,value}` and `models[].{name,providers[]}` with `providers[].{name,url,model,headers}`. `url` is the full upstream endpoint, not a base URL. Auth to upstream belongs in `headers`.
- Incoming `Authorization` / `X-Api-Key` / `Api-Key` authenticate against `api_keys` and must not be copied upstream.
- At least one API key is required; missing/invalid keys → 401 except `GET /healthz`.
- Do not commit `config.yaml` (secrets). Change `config.example.yaml` and README when the config schema changes.
- Images: `ghcr.io/${{ github.repository }}`. Push `main` → `latest`; push a git tag → that tag. Workflow: `.github/workflows/release.yml`.

## Style

- Match nearby files: `log` package, table-driven tests, `httptest` for HTTP.
- `main` may use `rg.Must` / `rg.Guard`; library-ish functions return `error`.
- Keep YAML config the only user-facing knobs besides listen/config flags and standard `OTEL_*` env vars.
- Public docs and fixtures: placeholders (`example.com`, `sk-REPLACE_ME`), never real secrets.

## Verify

```bash
go test ./...
gofmt -w .
```

After config, metric, failover, or auth changes, update `README.md` (operators) and this file (agents) in the same change when behavior diverges from what they currently say.
