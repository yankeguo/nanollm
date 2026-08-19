# nanollm

OpenAI- and Anthropic-compatible reverse proxy for LLM APIs. Point coding tools at nanollm and map one client-facing model name to one or more upstream providers.

Providers for a model are tried **in order**, but only those whose `format` matches the inbound API. The next provider is used only when the current one is catastrophically unavailable. Rate limits, 4xx, and ordinary 5xx stay on the same provider so prompt cache is not thrown away.

## Features

- Standard `net/http` server: OpenAI `/v1/chat/completions` (plus completions, embeddings, responses) and Anthropic `/v1/messages`
- YAML config: named models, named providers, `format` (`openai` or `anthropic`), auth merged into provider `headers`
- No OpenAI ↔ Anthropic body conversion; each inbound API only uses matching providers
- Incoming API keys (`api_keys[].{name,value}`)
- Streaming SSE pass-through
- Failover only on dial/DNS/TLS failure or HTTP 502/503/504
- Every upstream attempt (including failures) is stored in MySQL
- Admin UI at `/admin` (cookie login) for usage charts and call details

## Quick start

```bash
cp config.example.yaml config.yaml
# edit mysql.dsn, admin password, api_keys, models, and upstream headers
go run . -config config.yaml -listen :8080
```

Or with Docker:

```bash
docker run --rm -p 8080:8080 \
  -v "$PWD/config.yaml:/config.yaml:ro" \
  ghcr.io/yankeguo/nanollm:latest
```

Point OpenAI clients at `http://127.0.0.1:8080/v1` with `Authorization: Bearer <api_keys.value>` and `"model": "<models[].name>"`.

Point Anthropic clients at `http://127.0.0.1:8080` with `x-api-key: <api_keys.value>` and the same `"model"` field. The model must have at least one `format: anthropic` provider.

Open `http://127.0.0.1:8080/admin` to review token usage and call logs.

## Flags and environment

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `-config` | `NANOLLM_CONFIG` | `config.yaml` | YAML config path |
| `-listen` | `NANOLLM_LISTEN` | `:8080` | HTTP listen address |

The container image sets `NANOLLM_CONFIG=/config.yaml`.

## Config

```yaml
mysql:
  dsn: nanollm:REPLACE_ME@tcp(127.0.0.1:3306)/nanollm
  detail_retain: 1000
admin:
  username: admin
  password: REPLACE_ME
api_keys:
  - name: alice
    value: sk-alice
models:
  - name: gpt-4o
    providers:
      - name: openai
        url: https://api.openai.com/v1/chat/completions
        model: gpt-4o
        headers:
          Authorization: Bearer sk-REPLACE_ME
      - name: backup
        url: https://openrouter.ai/api/v1/chat/completions
        model: openai/gpt-4o
        headers:
          Authorization: Bearer sk-or-REPLACE_ME
  - name: claude
    providers:
      - name: anthropic
        format: anthropic
        url: https://api.anthropic.com/v1/messages
        model: claude-sonnet-4-5
        headers:
          x-api-key: REPLACE_ME
          anthropic-version: "2023-06-01"
```

| Field | Required | Meaning |
|---|---|---|
| `mysql.dsn` | yes | MySQL DSN (`user:pass@tcp(host:3306)/dbname`). Connection params are forced: `charset=utf8mb4`, `parseTime=true`, `loc=UTC`, `time_zone='UTC'` |
| `mysql.detail_retain` | no | Keep request/response JSON for the latest N rows (default `1000`; `0` keeps no blobs) |
| `admin.username` | yes | Admin UI login name |
| `admin.password` | yes | Admin UI login password |
| `api_keys[].name` | yes | Identifier for this key (must be unique) |
| `api_keys[].value` | yes | Secret the client must send |
| `models[].name` | yes | Client-facing model id |
| `models[].providers` | yes | Ordered upstream list |
| `providers[].name` | yes | Identifier for this provider within a model (must be unique) |
| `providers[].format` | no | `openai` (default) or `anthropic`. OpenAI routes only use `openai` providers; `POST /v1/messages` only uses `anthropic` providers |
| `providers[].url` | yes | Full `http`/`https` upstream URL (not a base URL) |
| `providers[].model` | no | Model string sent upstream; if empty, the client model is kept |
| `providers[].headers` | no | Extra request headers, including upstream auth |

Rules:

- Admin username and password are required
- MySQL DSN is required; timestamps are stored in UTC
- At least one API key and one model
- Model names unique; provider names unique **within** a model
- API key names unique; API key values unique
- Incoming `Authorization` / `X-Api-Key` / `Api-Key` are **not** forwarded
- Request bodies larger than 64 MiB return `413`
- nanollm does **not** convert OpenAI and Anthropic bodies. If a model has no provider for the inbound format, the request fails with `404`
- `config.yaml` is gitignored; commit `config.example.yaml` only

See `config.example.yaml`.

## Authentication

LLM API routes except `GET /healthz` require a configured key:

- `Authorization: Bearer <value>` (OpenAI clients; `Bearer` is case-insensitive)
- `X-Api-Key: <value>`
- `Api-Key: <value>`

Unknown or missing keys return `401`. OpenAI routes use `{"error":{"type":"invalid_request_error","message":"invalid api key"}}`. Anthropic `POST /v1/messages` uses `{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`.

The admin UI (`/admin`) uses `admin.username` / `admin.password` and an HttpOnly cookie (`nanollm_admin`, 12h, SameSite=Lax). `Secure` is set when the request is TLS or `X-Forwarded-Proto: https`. `/admin/login` is unauthenticated.

## Failover

For each request, providers with a matching `format` are attempted from first to last.

**Switch to the next provider only when the current one is catastrophically unavailable:**

- TCP dial failure, DNS failure, other transport errors before a usable response
- HTTP `502` / `503` / `504`

**Do not switch** (return the upstream response / error as-is):

- `429` rate limit
- `4xx` client errors
- ordinary `5xx` such as `500`
- timeouts after the provider was reached
- client cancellation
- any error after the response status has already been written to the client

This keeps prefix / prompt cache on the first healthy provider.

## HTTP API

| Method | Path | Auth | Notes |
|---|---|---|---|
| `GET` | `/healthz` | no | `OK` |
| `GET` | `/v1/models` | yes | Lists configured `models[].name` |
| `GET` | `/v1/models/{model}` | yes | Single configured model (ids may contain `/`) |
| `POST` | `/v1/chat/completions` | yes | Also `/chat/completions` |
| `POST` | `/v1/completions` | yes | Also `/completions` |
| `POST` | `/v1/embeddings` | yes | Also `/embeddings` |
| `POST` | `/v1/responses` | yes | |
| `POST` | `/v1/messages` | yes | Anthropic Messages; only `format: anthropic` providers |
| `GET` | `/admin` | cookie | Usage tables and Chart.js graphs |
| `GET` | `/admin/calls` | cookie | Paginated call log |
| `GET` | `/admin/calls/{id}` | cookie | Request/response JSON when retained |
| `GET`/`POST` | `/admin/login` | no | Admin sign-in |
| `POST` | `/admin/logout` | cookie | Clear admin cookie |

The JSON body `model` field selects `models[].name`. nanollm rewrites it to `providers[].model` and POSTs to `providers[].url`. OpenAI routes never call `format: anthropic` providers, and `/v1/messages` never calls `format: openai` providers.

Streaming (`"stream": true`) is copied through as SSE. On OpenAI streaming, if `stream_options.include_usage` is missing, it is set to `true` so the last chunk can carry token counts. Anthropic bodies are not rewritten beyond `model`.

## Call log (MySQL)

Each provider attempt writes one row to `llm_calls`:

- client model, provider name, upstream model, API key name
- `input_tokens` / `output_tokens` / `cache_tokens` / `uncached_tokens` (0 when usage is missing)
- `http_status` (0 if no HTTP response, e.g. dial failure)
- `error` (transport / rewrite / canceled / catastrophic status; empty on a completed copy)
- `request_json` / `response_json` (`MEDIUMBLOB`; SSE responses stored as a JSON string)

Failures and failover hops are recorded so you can see which hop died. The synthetic “all upstreams unavailable” client error is not an extra row.

After each insert, blobs older than the latest `mysql.detail_retain` rows are set to `NULL`. Metadata is kept. Bodies larger than 16 MiB skip the blob columns.

Insert/prune errors are logged and do not change the client response.

Browse the same data at `/admin` after signing in with `admin.username` / `admin.password`. Usage can be grouped by hour, day, week, or month.

## Docker / GHCR

Images: `ghcr.io/yankeguo/nanollm`

| Git event | Image tag |
|---|---|
| Push `main` | `latest` |
| Push a git tag | that tag (e.g. `v1.0.0`) |

Workflow: `.github/workflows/release.yml`.

## Development

```bash
go test ./...
```

Go 1.26+. `config.yaml` holds secrets and is gitignored.

## License

MIT, GUO YANKE
