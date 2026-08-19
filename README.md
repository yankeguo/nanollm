# nanollm

OpenAI-compatible reverse proxy for LLM APIs. Point coding tools at nanollm, map one client-facing model name to one or more upstream providers, and export token usage (including cache hits) over OTLP/HTTP so you can see how many tokens an opaque coding plan actually delivers.

Providers for a model are tried **in order**. The next provider is used only when the current one is catastrophically unavailable. Rate limits, 4xx, and ordinary 5xx stay on the same provider so prompt cache is not thrown away.

## Features

- Standard `net/http` server, OpenAI-compatible `/v1/chat/completions` (plus completions, embeddings, responses)
- YAML config: named models, named providers, auth merged into provider `headers`
- Incoming API keys (`api_keys[].{name,value}`)
- Streaming SSE pass-through
- Custom OTEL counters: input / output / cache_read / cache_creation / uncached, labeled by model, provider, and API key
- Failover only on dial/DNS/TLS failure or HTTP 502/503/504

## Quick start

```bash
cp config.example.yaml config.yaml
# edit api_keys, models, and upstream headers
go run . -config config.yaml -listen :8080
```

Or with Docker:

```bash
docker run --rm -p 8080:8080 \
  -v "$PWD/config.yaml:/config.yaml:ro" \
  ghcr.io/yankeguo/nanollm:latest
```

Point the client at `http://127.0.0.1:8080/v1` with `Authorization: Bearer <api_keys.value>` and `"model": "<models[].name>"`.

## Flags and environment

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `-config` | `NANOLLM_CONFIG` | `config.yaml` | YAML config path |
| `-listen` | `NANOLLM_LISTEN` | `:8080` | HTTP listen address |

The container image sets `NANOLLM_CONFIG=/config.yaml`.

OTEL uses the standard SDK / OTLP HTTP environment variables (see [Metrics](#metrics)). `OTEL_SDK_DISABLED=true` turns export off.

## Config

```yaml
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
```

| Field | Required | Meaning |
|---|---|---|
| `api_keys[].name` | yes | Label stored on metrics as `nanollm.api_key` |
| `api_keys[].value` | yes | Secret the client must send |
| `models[].name` | yes | Client-facing model id |
| `models[].providers` | yes | Ordered upstream list |
| `providers[].name` | yes | Label stored on metrics as `nanollm.provider` |
| `providers[].url` | yes | Full upstream URL (not a base URL) |
| `providers[].model` | no | Model string sent upstream; if empty, the client model is kept |
| `providers[].headers` | no | Extra request headers, including upstream auth |

Rules:

- At least one API key and one model
- Model names unique; provider names unique **within** a model
- API key names unique; API key values unique
- Incoming `Authorization` / API key headers are **not** forwarded
- `config.yaml` is gitignored; commit `config.example.yaml` only

See `config.example.yaml`.

## Authentication

Every route except `GET /healthz` requires a configured key:

- `Authorization: Bearer <value>` (OpenAI clients)
- `X-Api-Key: <value>`
- `Api-Key: <value>`

Unknown or missing keys return `401` with `{"error":{"type":"invalid_request_error","message":"invalid api key"}}`.

## Failover

For each request, providers are attempted from first to last.

**Switch to the next provider only when the current one is catastrophically unavailable:**

- TCP dial failure, DNS failure, other transport errors before a usable response
- HTTP `502` / `503` / `504`

**Do not switch** (return the upstream response / error as-is):

- `429` rate limit
- `4xx` client errors
- ordinary `5xx` such as `500`
- timeouts after the provider was reached
- client cancellation

This keeps prefix / prompt cache on the first healthy provider.

## HTTP API

| Method | Path | Auth | Notes |
|---|---|---|---|
| `GET` | `/healthz` | no | `OK` |
| `GET` | `/v1/models` | yes | Lists configured `models[].name` |
| `GET` | `/v1/models/{model}` | yes | Single configured model |
| `POST` | `/v1/chat/completions` | yes | Also `/chat/completions` |
| `POST` | `/v1/completions` | yes | Also `/completions` |
| `POST` | `/v1/embeddings` | yes | Also `/embeddings` |
| `POST` | `/v1/responses` | yes | |

The JSON body `model` field selects `models[].name`. nanollm rewrites it to `providers[].model` and POSTs to `providers[].url`.

Streaming (`"stream": true`) is copied through as SSE. If `stream_options.include_usage` is missing, it is set to `true` so the last chunk can carry token counts.

## Metrics

Custom counters, **not** OpenTelemetry GenAI semantic conventions.

**Instrument:** `nanollm.token.usage`  
**Type:** Counter  
**Unit:** `{token}`

One request can add several data points that share labels and differ by `nanollm.token.type`:

| `nanollm.token.type` | Meaning |
|---|---|
| `input` | All input / prompt tokens (**includes** cache) |
| `output` | All output / completion tokens |
| `cache_read` | Input tokens served from provider cache |
| `cache_creation` | Input tokens written into provider cache |
| `uncached` | Input tokens that were not cache read or cache write |

Attributes:

| Attribute | Source |
|---|---|
| `nanollm.model` | `models[].name` |
| `nanollm.provider` | `providers[].name` |
| `nanollm.api_key` | `api_keys[].name` |
| `nanollm.upstream.model` | `providers[].model` |
| `nanollm.token.type` | see table above |

`input` overlaps the cache breakdown on purpose:

```
cache hit ratio ≈ cache_read / input
uncached ≈ input - cache_read - cache_creation
```

Usage is parsed from common OpenAI-compatible `usage` objects, including:

- OpenAI `prompt_tokens` / `completion_tokens` / `prompt_tokens_details.cached_tokens`
- DeepSeek `prompt_cache_hit_tokens` / `prompt_cache_miss_tokens`
- Anthropic `cache_read_input_tokens` / `cache_creation_input_tokens`

Standard OTLP/HTTP env vars:

- `OTEL_SERVICE_NAME` (default resource service name is `nanollm`)
- `OTEL_RESOURCE_ATTRIBUTES`
- `OTEL_EXPORTER_OTLP_ENDPOINT` (base URL; `/v1/metrics` is appended)
- `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` (full URL, typically `.../v1/metrics`)
- `OTEL_EXPORTER_OTLP_HEADERS` / `OTEL_EXPORTER_OTLP_METRICS_HEADERS`
- `OTEL_EXPORTER_OTLP_TIMEOUT`
- `OTEL_EXPORTER_OTLP_COMPRESSION`
- `OTEL_METRIC_EXPORT_INTERVAL`
- `OTEL_SDK_DISABLED`

```bash
export OTEL_SERVICE_NAME=nanollm
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
go run . -config config.yaml
```

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
