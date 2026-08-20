# nanollm

OpenAI- and Anthropic-compatible reverse proxy for LLM APIs. Point coding tools at nanollm and map one client-facing model name to one or more upstream providers.

Providers for a model are tried **in order**, but only those that have a protocol block matching the inbound API (`openai`, `responses`, or `anthropic`). The next provider is used only when the current one is catastrophically unavailable. Rate limits, 4xx, and ordinary 5xx stay on the same provider so prompt cache is not thrown away.

## Features

- Standard `net/http` server: OpenAI `/v1/chat/completions` (plus completions, embeddings), OpenAI `/v1/responses`, and Anthropic `/v1/messages`
- YAML config: named models, named vendors, optional `openai` / `responses` / `anthropic` endpoint blocks, auth merged into `headers`
- No body conversion between protocols; each inbound API only uses matching providers
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

Point OpenAI clients at `http://127.0.0.1:8080/v1` with `Authorization: Bearer <api_keys.value>` and `"model": "<models[].name>"`. Chat Completions uses the vendor `openai` block; the Responses API (`POST /v1/responses`) uses the `responses` block.

Point Anthropic clients at `http://127.0.0.1:8080` with `x-api-key: <api_keys.value>` and the same `"model"` field. The model must have at least one provider with an `anthropic` block.

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
        model: gpt-4o
        headers:
          Authorization: Bearer sk-REPLACE_ME
        openai:
          url: https://api.openai.com/v1/chat/completions
        responses:
          url: https://api.openai.com/v1/responses
  - name: claude
    providers:
      - name: openrouter
        model: anthropic/claude-sonnet-4-5
        headers:
          Authorization: Bearer sk-or-REPLACE_ME
          anthropic-version: "2023-06-01"
        openai:
          url: https://openrouter.ai/api/v1/chat/completions
        responses:
          url: https://openrouter.ai/api/v1/responses
        anthropic:
          url: https://openrouter.ai/api/v1/messages
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
| `providers[].name` | yes | Vendor name within a model (must be unique). Used for failover order and `llm_calls.provider` |
| `providers[].model` | no | Default upstream model; a protocol block may override it. If both are empty, the client model is kept |
| `providers[].headers` | no | Default extra headers (including upstream auth); a protocol block may override keys |
| `providers[].openai` / `providers[].responses` / `providers[].anthropic` | one of these, or legacy `url` | Protocol endpoint. Chat/completions/embeddings only use vendors with `openai`; `POST /v1/responses` only uses vendors with `responses`; `POST /v1/messages` only uses vendors with `anthropic` |
| `openai.url` / `responses.url` / `anthropic.url` | yes (when the block is present) | Full `http`/`https` upstream URL (not a base URL) |
| `openai.model` / `responses.model` / `anthropic.model` | no | Overrides the vendor-level `model` for that protocol |
| `openai.headers` / `responses.headers` / `anthropic.headers` | no | Overlay on vendor-level `headers` (`Set` per key) |
| `providers[].url` | legacy | Flat form: required when no protocol blocks are set. Normalized into `openai` (default), `responses`, or `anthropic` |
| `providers[].format` | no (legacy) | Flat form only: `openai` (default), `responses`, or `anthropic`. Cannot be mixed with protocol blocks |

Rules:

- Admin username and password are required
- MySQL DSN is required; timestamps are stored in UTC
- At least one API key and one model
- Model names unique; provider names unique **within** a model
- Nested `openai`/`responses`/`anthropic` blocks cannot be mixed with top-level `url`/`format` on the same vendor
- API key names unique; API key values unique
- Incoming `Authorization` / `X-Api-Key` / `Api-Key` are **not** forwarded; client `Cookie` is stripped from upstream requests and upstream `Set-Cookie` is dropped from responses
- Request bodies larger than 64 MiB return `413`
- nanollm does **not** convert bodies between chat completions, Responses, and Anthropic. If a model has no vendor with a matching protocol block, the request fails with `404`
- `config.yaml` is gitignored; commit `config.example.yaml` only

See `config.example.yaml`.

## Authentication

LLM API routes except `GET /healthz` require a configured key:

- `Authorization: Bearer <value>` (OpenAI clients; `Bearer` is case-insensitive)
- `X-Api-Key: <value>`
- `Api-Key: <value>`

Unknown or missing keys return `401`. OpenAI routes use `{"error":{"type":"invalid_request_error","message":"invalid api key"}}`. Anthropic `POST /v1/messages` uses `{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`.

The admin UI (`/admin`) uses `admin.username` / `admin.password` and an HttpOnly cookie (`nanollm_admin`, 12h, SameSite=Lax). `Secure` is set when the request is TLS or `X-Forwarded-Proto: https`. `/admin/login` is unauthenticated; failed logins are delayed by 1s to damp brute force.

## Failover

For each request, vendors that have a matching protocol block are attempted from first to last. A vendor without that block is skipped; nanollm does not switch protocols on the same vendor.

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
| `POST` | `/v1/responses` | yes | Also `/responses`. Only providers with a `responses` block |
| `POST` | `/v1/messages` | yes | Anthropic Messages; only providers with an `anthropic` block |
| `GET` | `/admin` | cookie | Usage tables and Chart.js graphs |
| `GET` | `/admin/calls` | cookie | Paginated call log |
| `GET` | `/admin/calls/{id}` | cookie | Request/response JSON when retained |
| `GET`/`POST` | `/admin/login` | no | Admin sign-in |
| `POST` | `/admin/logout` | cookie | Clear admin cookie |

The JSON body `model` field selects `models[].name`. nanollm rewrites only the top-level `model` (and, for OpenAI chat streaming, injects `stream_options.include_usage` when missing). Responses and Anthropic bodies are not given `stream_options`. Other JSON fields are copied as raw values and are not decoded into a typed tree. Each inbound path only uses vendors with the matching protocol block: chat/completions/embeddings → `openai`, `/v1/responses` → `responses`, `/v1/messages` → `anthropic`.

Streaming (`"stream": true`) is copied through as SSE. While the upstream is silent (long thinking / a long tool-using turn), nanollm writes SSE comments (`:` lines) so the client and any idle proxy do not time out. Usage is parsed from a copy of the upstream body (including Responses `response.usage` on `response.completed`); keepalive comments are not stored in the call-log blob. Anthropic and Responses bodies are not rewritten beyond `model`.

## Call log (MySQL)

Each provider attempt writes one row to `llm_calls`:

- client model, provider name, upstream model, API key name
- `input_tokens` / `output_tokens` / `cache_tokens` / `uncached_tokens` (0 when usage is missing)
- `http_status` (0 if no HTTP response, e.g. dial failure)
- `error` (transport / rewrite / catastrophic status; `canceled` when the client hung up after the copy started; empty on a completed copy)
- `request_json` / `response_json` (`MEDIUMBLOB`; SSE responses stored as a JSON string)

Failures and failover hops are recorded so you can see which hop died. The synthetic “all upstreams unavailable” client error is not an extra row.

Periodically (every 50 inserts), blobs older than the latest `mysql.detail_retain` rows are set to `NULL`. Metadata is kept. Bodies larger than 16 MiB skip the blob columns.

Insert/prune errors are logged and do not change the client response.

Browse the same data at `/admin` after signing in with `admin.username` / `admin.password`. Usage and Calls share a time window (presets, a custom range, or all-time on Calls) and combinable filters for model, provider, API key, and outcome. Usage can be grouped by hour, day, week, or month.

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
