# nanollm

OpenAI- and Anthropic-compatible reverse proxy for LLM APIs. Point coding tools at nanollm and map one client-facing model name to one or more upstream providers.

Providers for a model are tried **in order**, but only those whose `protocols` list matches the inbound API (`openai_completions`, `openai_responses`, `openai_embeddings`, or `anthropic_messages`). The next provider is used only when the current one is catastrophically unavailable. Rate limits, 4xx, and ordinary 5xx stay on the same provider so prompt cache is not thrown away.

> [!WARNING]
> Protocol conversion is intentionally **not** supported: nanollm never rewrites a body between Chat Completions, Responses, and Anthropic Messages. This is a deliberate design choice to maximize field-level compatibility (every non-`model` field is forwarded untouched) and forwarding performance. If a model has no associated vendor listing the matching protocol, the request fails with `404` instead of being converted.

## Features

- Standard `net/http` server: OpenAI `/v1/chat/completions` (plus completions, embeddings), OpenAI `/v1/responses`, and Anthropic `/v1/messages`
- YAML config: top-level named vendors, named models that associate vendors plus a `protocols` subset, auth merged into `headers`
- No body conversion between protocols; each inbound API only uses matching providers
- Incoming API keys (`api_keys[].{name,value}`)
- Streaming SSE pass-through
- SIGINT/SIGTERM stops accepting connections and waits indefinitely for in-flight requests (including long LLM streams); a second signal terminates
- Failover only on dial/DNS/TLS failure or HTTP 502/503/504
- Every upstream attempt (including failures) is stored in MySQL
- Admin UI at `/admin` (cookie login) for usage charts and call details

## Quick start

```bash
cp config.example.yaml config.yaml
# edit mysql.dsn, admin password, api_keys, providers, and models
go run . -config config.yaml -listen :8080
```

Or with Docker:

```bash
docker run --rm -p 8080:8080 \
  -v "$PWD/config.yaml:/config.yaml:ro" \
  ghcr.io/yankeguo/nanollm:latest
```

Point OpenAI clients at `http://127.0.0.1:8080/v1` with `Authorization: Bearer <api_keys.value>` and `"model": "<models[].name>"`. Chat Completions uses the vendor `openai_completions` block; the Responses API (`POST /v1/responses`) uses the `openai_responses` block; embeddings (`POST /v1/embeddings`) uses the `openai_embeddings` block.

Point Anthropic clients at `http://127.0.0.1:8080` with `x-api-key: <api_keys.value>` and the same `"model"` field. The model must list `anthropic_messages` on at least one associated provider.

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
providers:
  - name: openai
    headers:
      Authorization: Bearer sk-REPLACE_ME
    openai_completions:
      url: https://api.openai.com/v1/chat/completions
    openai_responses:
      url: https://api.openai.com/v1/responses
    openai_embeddings:
      url: https://api.openai.com/v1/embeddings
  - name: openrouter
    headers:
      Authorization: Bearer sk-or-REPLACE_ME
    openai_completions:
      url: https://openrouter.ai/api/v1/chat/completions
    openai_responses:
      url: https://openrouter.ai/api/v1/responses
    anthropic_messages:
      url: https://openrouter.ai/api/v1/messages
      headers:
        anthropic-version: "2023-06-01"
models:
  - name: gpt-4o
    providers:
      - name: openai
        model: gpt-4o
        protocols: [openai_completions, openai_responses]
      - name: openrouter
        model: openai/gpt-4o
        protocols: [openai_completions]
  - name: text-embedding-3-small
    providers:
      - name: openai
        model: text-embedding-3-small
        protocols: [openai_embeddings]
  - name: claude
    providers:
      - name: openrouter
        model: anthropic/claude-sonnet-4-5
        protocols: [openai_completions, openai_responses, anthropic_messages]
```

| Field | Required | Meaning |
|---|---|---|
| `mysql.dsn` | yes | MySQL DSN (`user:pass@tcp(host:3306)/dbname`). Connection params are forced: `charset=utf8mb4`, `parseTime=true`, `loc=UTC`, `time_zone='UTC'` |
| `mysql.detail_retain` | no | Keep request/response JSON and extracted files for the latest N rows (default `1000`; `0` keeps no blobs) |
| `admin.username` | yes | Admin UI login name |
| `admin.password` | yes | Admin UI login password |
| `api_keys[].name` | yes | Identifier for this key (must be unique) |
| `api_keys[].value` | yes | Secret the client must send |
| `providers[].name` | yes | Vendor name (globally unique). Used for `llm_calls.provider` |
| `providers[].model` | no | Default upstream model for associations that omit `model` |
| `providers[].headers` | no | Default extra headers (including upstream auth); a protocol block may override keys |
| `providers[].openai_completions` / `providers[].openai_responses` / `providers[].openai_embeddings` / `providers[].anthropic_messages` | at least one | Protocol endpoint URLs. A model only uses a vendor for an inbound API when that protocol is listed on the association |
| `openai_completions.url` / `openai_responses.url` / `openai_embeddings.url` / `anthropic_messages.url` | yes (when the block is present) | Full `http`/`https` upstream URL (not a base URL) |
| `openai_completions.model` / `openai_responses.model` / `openai_embeddings.model` / `anthropic_messages.model` | no | Overrides the bound `model` for that protocol |
| `openai_completions.headers` / `openai_responses.headers` / `openai_embeddings.headers` / `anthropic_messages.headers` | no | Overlay on vendor-level `headers` (`Set` per key) |
| `models[].name` | yes | Client-facing model id |
| `models[].providers` | yes | Ordered vendor associations (failover order) |
| `models[].providers[].name` | yes | References a top-level `providers[].name` (unique within the model) |
| `models[].providers[].model` | no | Upstream model for this association; overrides `providers[].model`. If both are empty, the client model is kept |
| `models[].providers[].protocols` | yes | Non-empty subset of the vendor's protocol blocks. Chat/completions only use associations that list `openai_completions`; `POST /v1/embeddings` only uses `openai_embeddings`; `POST /v1/responses` only uses `openai_responses`; `POST /v1/messages` only uses `anthropic_messages` |

Rules:

- Admin username and password are required
- MySQL DSN is required; timestamps are stored in UTC
- At least one API key, one provider, and one model
- Provider names unique globally; model names unique; association names unique **within** a model
- Each vendor **must** set at least one of `openai_completions`, `openai_responses`, `openai_embeddings`, or `anthropic_messages`. Inference and embeddings blocks may coexist on the same vendor; nanollm does not enforce that split. Unused top-level vendors are allowed.
- Each association's `protocols` must be a non-empty subset of that vendor's defined blocks
- API key names unique; API key values unique
- Incoming `Authorization` / `X-Api-Key` / `Api-Key` are **not** forwarded; client `Cookie` is stripped from upstream requests and upstream `Set-Cookie` is dropped from responses
- Request bodies larger than 64 MiB return `413`
- nanollm does **not** convert bodies between chat completions, Responses, and Anthropic. If a model has no association listing the matching protocol, the request fails with `404`
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

For each request, associated vendors that list the matching protocol are attempted from first to last. An association that does not list that protocol is skipped; nanollm does not switch protocols on the same vendor.

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
| `POST` | `/v1/embeddings` | yes | Also `/embeddings`. Only associations that list `openai_embeddings` |
| `POST` | `/v1/responses` | yes | Also `/responses`. Only associations that list `openai_responses` |
| `POST` | `/v1/messages` | yes | Anthropic Messages; only associations that list `anthropic_messages` |
| `GET` | `/admin` | cookie | Usage tables and Chart.js graphs |
| `GET` | `/admin/calls` | cookie | Paginated call log |
| `GET` | `/admin/calls/{id}` | cookie | Request/response JSON when retained; inline previews of extracted files |
| `GET` | `/admin/files/{sha}` | cookie | File bytes from `llm_files` (SHA256 hex); safe media types served inline, anything else forced to download as `application/octet-stream` |
| `GET`/`POST` | `/admin/login` | no | Admin sign-in |
| `POST` | `/admin/logout` | cookie | Clear admin cookie |

The JSON body `model` field selects `models[].name`. nanollm rewrites only the top-level `model` (and, for OpenAI Chat Completions and Completions streaming, injects `stream_options.include_usage` when missing). Responses `stream_options` only documents `include_obfuscation`; Anthropic has no `stream_options`; embeddings are not given that field. Other JSON fields are copied as raw values and are not decoded into a typed tree. Each inbound path only uses associations that list the matching protocol: chat/completions → `openai_completions`, `/v1/embeddings` → `openai_embeddings`, `/v1/responses` → `openai_responses`, `/v1/messages` → `anthropic_messages`.

Streaming (`"stream": true` with `Content-Type: text/event-stream`) is copied through as SSE. JSON error bodies on a `stream: true` request stay JSON (no SSE comments). While the upstream SSE body is silent (long thinking / a long tool-using turn), nanollm writes SSE comments (`:` lines) so the client and any idle proxy do not time out; `Content-Length` is dropped on that path so the extra comments cannot desync HTTP/1.1 clients. Usage is parsed from a copy of the upstream body (including Responses `response.usage` on `response.completed`, whose payload can be larger than a single thinking delta, and embeddings `prompt_tokens` / `total_tokens`); keepalive comments are not stored in the call-log blob. Consecutive same-type text/argument deltas are coalesced in that blob only (Chat Completions `delta.content` / tool-call `arguments` / legacy `function_call.arguments`, Completions `choices[].text`, Anthropic `content_block_delta`, Responses `*.delta` strings); the client still receives the raw stream. Anthropic and Responses bodies are not rewritten beyond `model`.

## Call log (MySQL)

Each provider attempt writes one row to `llm_calls`:

- client model, provider name, upstream model, API key name
- `input_tokens` / `output_tokens` / `cache_tokens` / `uncached_tokens` (0 when usage is missing)
- `http_status` (0 if no HTTP response, e.g. dial failure)
- `error` (transport / rewrite / catastrophic status; `canceled` when the client hung up after the copy started; empty on a completed copy)
- `request_json` / `response_json` (`MEDIUMBLOB`; SSE responses stored as a JSON string, with consecutive same-type text/argument deltas coalesced)
- multimodal base64 in those blobs is replaced with `<file:{sha256}>` before insert (data URLs, Anthropic `type=base64` sources, OpenAI `input_audio`, and `b64_json`); decoded bytes go to `llm_files` (`MEDIUMBLOB`, SHA256 primary key) with `llm_call_files` linking each call. Upstream request/response bytes are not rewritten.

Failures and failover hops are recorded so you can see which hop died. The synthetic “all upstreams unavailable” client error is not an extra row.

Periodically (every 50 inserts), blobs older than the latest `mysql.detail_retain` rows are set to `NULL`, join rows for those calls are deleted, and unreferenced `llm_files` rows older than a minute are removed. Metadata is kept. Bodies larger than 16 MiB skip the blob columns (and file extraction); a single decoded file larger than 16 MiB is left as base64 in the JSON.

Insert/prune errors are logged and do not change the client response.

Browse the same data at `/admin` after signing in with `admin.username` / `admin.password`. Usage and Calls share a time window (quick ranges or a custom range with a timezone, defaulting to the browser's) and combinable filters for model, provider, API key, and outcome (`ok` / `canceled` / `no_response` / `error`, partitioning every recorded call). Usage is grouped by hour, day, or week, derived from the window span.

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

Go 1.27+. `config.yaml` holds secrets and is gitignored.

## License

MIT, GUO YANKE
