# nanollm

OpenAI- and Anthropic-compatible reverse proxy for LLM APIs. Point coding tools at nanollm and map one client-facing model name to one or more upstream providers.

Providers for a model are tried **in order**, but only those whose `protocols` list matches the inbound API (`openai_completions`, `openai_responses`, `openai_embeddings`, `anthropic_messages`, `bailian_multimodal_embedding`, or `ollama_chat`). The next provider is used only when the current one is catastrophically unavailable. Rate limits, 4xx, and ordinary 5xx stay on the same provider so prompt cache is not thrown away.

> [!WARNING]
> Protocol conversion is intentionally **not** supported: nanollm never rewrites a body between Chat Completions, Responses, Anthropic Messages, and Ollama chat. This is a deliberate design choice to maximize field-level compatibility (every non-`model` field is forwarded untouched) and forwarding performance. If a model has no associated vendor listing the matching protocol, the request fails with `404` instead of being converted.

## Features

- Standard `net/http` server: OpenAI chat/completions, embeddings, and Responses under `/api.openai.com/...`, Anthropic Messages under `/api.anthropic.com/...` (vendor host as the first path segment)
- YAML config: top-level named vendors, named models that associate vendors plus a `protocols` subset, auth merged into `headers`
- No body conversion between protocols; each inbound API only uses matching providers
- Incoming API keys (`api_keys[].{name,value}`)
- Streaming SSE pass-through
- SIGINT/SIGTERM stops accepting connections and waits indefinitely for in-flight requests (including long LLM streams); a second signal terminates
- Failover only on dial/DNS/TLS failure or HTTP 502/503/504
- Every upstream attempt (including failures) is stored in MySQL
- Admin UI at the root (cookie login): `/usage` for charts, `/calls` for call details

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

All LLM routes mirror the vendor's official URL shape: the vendor host is the first path segment, so clients only swap the endpoint host for this proxy. The host prefix also isolates vendor namespaces (OpenAI and Anthropic both have `/v1/files`, for example). No unprefixed legacy routes are served.

Point OpenAI clients at `http://127.0.0.1:8080/api.openai.com/v1` with `Authorization: Bearer <api_keys.value>` and `"model": "<models[].name>"`. Chat Completions uses the vendor `openai_completions` block; the Responses API (`POST /api.openai.com/v1/responses`) uses the `openai_responses` block; embeddings (`POST /api.openai.com/v1/embeddings`) uses the `openai_embeddings` block.

Point Anthropic clients at `http://127.0.0.1:8080/api.anthropic.com` with `x-api-key: <api_keys.value>` and the same `"model"` field. The model must list `anthropic_messages` on at least one associated provider.

For the Aliyun Bailian multimodal embedding API, POST to `http://127.0.0.1:8080/dashscope.aliyuncs.com/api/v1/services/embeddings/multimodal-embedding/multimodal-embedding` with `Authorization: Bearer <api_keys.value>`; the model must list `bailian_multimodal_embedding` on at least one associated provider.

Point Ollama clients at `http://127.0.0.1:8080/ollama.com` with `Authorization: Bearer <api_keys.value>` and the same `"model"` field; `POST /ollama.com/api/chat` uses the vendor `ollama_chat` block. Streaming answers are newline-delimited JSON (`application/x-ndjson`), passed through byte-for-byte.

Open `http://127.0.0.1:8080/` (redirects to `/usage`) to review token usage and call logs.

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
detail_retain: 168h
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
| `detail_retain` | no | Keep request/response JSON and extracted files for this duration (Go duration or `Nd` days; default `168h`; `0` keeps no blobs) |
| `admin.username` | yes | Admin UI login name |
| `admin.password` | yes | Admin UI login password |
| `api_keys[].name` | yes | Identifier for this key (must be unique) |
| `api_keys[].value` | yes | Secret the client must send |
| `providers[].name` | yes | Vendor name (globally unique). Used for `llm_calls.provider` |
| `providers[].model` | no | Default upstream model for associations that omit `model` |
| `providers[].headers` | no | Default extra headers (including upstream auth); a protocol block may override keys |
| `providers[].openai_completions` / `providers[].openai_responses` / `providers[].openai_embeddings` / `providers[].anthropic_messages` / `providers[].bailian_multimodal_embedding` / `providers[].ollama_chat` | at least one | Protocol endpoint URLs. A model only uses a vendor for an inbound API when that protocol is listed on the association |
| `openai_completions.url` / `openai_responses.url` / `openai_embeddings.url` / `anthropic_messages.url` / `bailian_multimodal_embedding.url` / `ollama_chat.url` | yes (when the block is present) | Full `http`/`https` upstream URL (not a base URL) |
| `openai_completions.model` / `openai_responses.model` / `openai_embeddings.model` / `anthropic_messages.model` / `bailian_multimodal_embedding.model` / `ollama_chat.model` | no | Overrides the bound `model` for that protocol |
| `openai_completions.headers` / `openai_responses.headers` / `openai_embeddings.headers` / `anthropic_messages.headers` / `bailian_multimodal_embedding.headers` / `ollama_chat.headers` | no | Overlay on vendor-level `headers` (`Set` per key) |
| `models[].name` | yes | Client-facing model id |
| `models[].providers` | yes | Ordered vendor associations (failover order) |
| `models[].providers[].name` | yes | References a top-level `providers[].name` (unique within the model) |
| `models[].providers[].model` | no | Upstream model for this association; overrides `providers[].model`. If both are empty, the client model is kept |
| `models[].providers[].protocols` | yes | Non-empty subset of the vendor's protocol blocks. Chat/completions only use associations that list `openai_completions`; `POST /api.openai.com/v1/embeddings` only uses `openai_embeddings`; `POST /api.openai.com/v1/responses` only uses `openai_responses`; `POST /api.anthropic.com/v1/messages` only uses `anthropic_messages`; `POST /dashscope.aliyuncs.com/api/v1/services/embeddings/multimodal-embedding/multimodal-embedding` only uses `bailian_multimodal_embedding`; `POST /ollama.com/api/chat` only uses `ollama_chat` |

Rules:

- Admin username and password are required
- MySQL DSN is required; timestamps are stored in UTC
- At least one API key, one provider, and one model
- Provider names unique globally; model names unique; association names unique **within** a model
- Each vendor **must** set at least one of `openai_completions`, `openai_responses`, `openai_embeddings`, `anthropic_messages`, `bailian_multimodal_embedding`, or `ollama_chat`. Inference and embeddings blocks may coexist on the same vendor; nanollm does not enforce that split. Unused top-level vendors are allowed.
- Each association's `protocols` must be a non-empty subset of that vendor's defined blocks
- API key names unique; API key values unique
- Incoming `Authorization` / `X-Api-Key` / `Api-Key` are **not** forwarded; client `Cookie` is stripped from upstream requests and upstream `Set-Cookie` is dropped from responses
- Request bodies larger than 64 MiB return `413`
- nanollm does **not** convert bodies between chat completions, Responses, Anthropic, and Ollama. If a model has no association listing the matching protocol, the request fails with `404`
- `config.yaml` is gitignored; commit `config.example.yaml` only

See `config.example.yaml`.

## Authentication

LLM API routes except `GET /healthz` require a configured key:

- `Authorization: Bearer <value>` (OpenAI clients; `Bearer` is case-insensitive)
- `X-Api-Key: <value>`
- `Api-Key: <value>`

Unknown or missing keys return `401`. OpenAI routes use `{"error":{"type":"invalid_request_error","message":"invalid api key"}}`. Anthropic `POST /api.anthropic.com/v1/messages` uses `{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`. The Bailian multimodal embedding route uses the DashScope shape `{"code":"...","message":"..."}`.

The admin UI (`/usage`, `/calls`) uses `admin.username` / `admin.password` and an HttpOnly cookie (`nanollm_admin`, 12h, SameSite=Lax). `Secure` is set when the request is TLS or `X-Forwarded-Proto: https`. `/login` is unauthenticated; failed logins are delayed by 1s to damp brute force.

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
| `GET` | `/api.openai.com/v1/models` | yes | Lists configured `models[].name` |
| `GET` | `/api.openai.com/v1/models/{model}` | yes | Single configured model (ids may contain `/`) |
| `POST` | `/api.openai.com/v1/chat/completions` | yes | Only associations that list `openai_completions` |
| `POST` | `/api.openai.com/v1/completions` | yes | Legacy Completions; only associations that list `openai_completions` |
| `POST` | `/api.openai.com/v1/embeddings` | yes | Only associations that list `openai_embeddings` |
| `POST` | `/api.openai.com/v1/responses` | yes | Only associations that list `openai_responses` |
| `POST` | `/api.anthropic.com/v1/messages` | yes | Anthropic Messages; only associations that list `anthropic_messages` |
| `POST` | `/dashscope.aliyuncs.com/api/v1/services/embeddings/multimodal-embedding/multimodal-embedding` | yes | Aliyun Bailian multimodal embedding (official URL shape, host as first path segment); only associations that list `bailian_multimodal_embedding` |
| `POST` | `/ollama.com/api/chat` | yes | Ollama chat; only associations that list `ollama_chat` |
| `GET` | `/` | no | 302 redirect to `/usage` |
| `GET` | `/usage` | cookie | Usage tables and Chart.js graphs |
| `GET` | `/calls` | cookie | Paginated call log |
| `GET` | `/calls/{id}` | cookie | Request/response JSON when retained; inline previews of extracted files |
| `GET` | `/files/{sha}` | cookie | File bytes from `llm_files` (SHA256 hex); safe media types served inline, anything else forced to download as `application/octet-stream` |
| `GET`/`POST` | `/login` | no | Admin sign-in |
| `POST` | `/logout` | cookie | Clear admin cookie |

The JSON body `model` field selects `models[].name`. nanollm rewrites only the top-level `model` (and, for OpenAI Chat Completions and Completions streaming, injects `stream_options.include_usage` when missing). Responses `stream_options` only documents `include_obfuscation`; Anthropic has no `stream_options`; embeddings and Ollama are not given that field. Other JSON fields are copied as raw values and are not decoded into a typed tree. Each inbound path only uses associations that list the matching protocol: chat/completions → `openai_completions`, `/api.openai.com/v1/embeddings` → `openai_embeddings`, `/api.openai.com/v1/responses` → `openai_responses`, `/api.anthropic.com/v1/messages` → `anthropic_messages`, the DashScope multimodal embedding path → `bailian_multimodal_embedding`, `/ollama.com/api/chat` → `ollama_chat`.

Streaming (`"stream": true` with `Content-Type: text/event-stream`) is copied through as SSE. JSON error bodies on a `stream: true` request stay JSON (no SSE comments). While the upstream SSE body is silent (long thinking / a long tool-using turn), nanollm writes SSE comments (`:` lines) so the client and any idle proxy do not time out; `Content-Length` is dropped on that path so the extra comments cannot desync HTTP/1.1 clients. Usage is parsed from a copy of the upstream body (including Responses `response.usage` on `response.completed`, whose payload can be larger than a single thinking delta, embeddings `prompt_tokens` / `total_tokens`, and DashScope multimodal embedding `input_tokens` + top-level `image_tokens`); keepalive comments are not stored in the call-log blob. Consecutive same-type text/argument deltas are coalesced in that blob only (Chat Completions `delta.content` / tool-call `arguments` / legacy `function_call.arguments`, Completions `choices[].text`, Anthropic `content_block_delta`, Responses `*.delta` strings); the client still receives the raw stream. Anthropic and Responses bodies are not rewritten beyond `model`.

Ollama `/api/chat` streams newline-delimited JSON (`application/x-ndjson`; `stream` defaults to true upstream when the field is omitted). NDJSON has no comment syntax, so no keepalive bytes can be injected and deltas are not coalesced; the transcript is still stored as a JSON string in the call log. Usage comes from the final `done` chunk's flat `prompt_eval_count` / `eval_count` (also present on non-streaming responses).

## Call log (MySQL)

Each provider attempt writes one row to `llm_calls`:

- client model, provider name, upstream model, API key name
- `input_tokens` / `output_tokens` / `cache_tokens` / `uncached_tokens` (0 when usage is missing)
- `first_token_ms` (request sent → first token: the first SSE `data:` line, or the first response body byte for non-streaming; 0 when no token ever arrived)
- `output_speed` (output tokens per second, measured from the first token to the end of the stream; 0 when usage or the first token is missing)
- `http_status` (0 if no HTTP response, e.g. dial failure)
- `error` (transport / rewrite / catastrophic status; `canceled` when the client hung up after the copy started; empty on a completed copy)
- `request_json` / `response_json` (`MEDIUMBLOB`; SSE responses stored as a JSON string, with consecutive same-type text/argument deltas coalesced)
- multimodal base64 in those blobs is replaced with `<file:{sha256}>` before insert (data URLs, Anthropic `type=base64` sources, OpenAI `input_audio`, and `b64_json`); decoded bytes go to `llm_files` (`MEDIUMBLOB`, SHA256 primary key) with `llm_call_files` linking each call. Upstream request/response bytes are not rewritten.

Failures and failover hops are recorded so you can see which hop died. The synthetic “all upstreams unavailable” client error is not an extra row.

Periodically (every 50 inserts), blobs and `llm_call_files` rows older than `detail_retain` are cleared, then unreferenced `llm_files` whose `visited_at` is older than that same cutoff are removed. Metadata is kept. A concurrent insert refreshes `llm_files.visited_at` before writing the join, so files still inside the window are not GC'd even if their join is not committed yet. Bodies larger than 16 MiB skip the blob columns (and file extraction); a single decoded file larger than 16 MiB is left as base64 in the JSON.

Insert/prune errors are logged and do not change the client response.

Browse the same data at `/usage` after signing in with `admin.username` / `admin.password`. Usage and Calls share a time window (quick ranges or a custom range with a timezone, defaulting to the browser's) and combinable filters for model, provider, API key, and outcome (`ok` / `canceled` / `no_response` / `error`, partitioning every recorded call). Usage is grouped by hour, day, or week, derived from the window span. Besides token and call charts, the page charts per-bucket average `first_token_ms` and `output_speed` (calls where the metric is unavailable, e.g. failures, are excluded from the averages).

## Docker / GHCR

Images: `ghcr.io/yankeguo/nanollm`

| Git event | Image tag |
|---|---|
| Push `main` | `latest` |
| Push a git tag | that tag (e.g. `v1.0.0`) |

Workflow: `.github/workflows/release.yml`.

## Development

```bash
(cd web && bun install && bun run build)  # admin UI bundles (Tailwind + lucide + Chart.js)
go test ./...
```

Go 1.27+. The admin UI assets live in `web/` (bun + TypeScript; `web/dist` is git-ignored and embedded at build time), so run the frontend build before `go build` — the Dockerfile does it in an `oven/bun` stage. `config.yaml` holds secrets and is gitignored.

## License

MIT, GUO YANKE
