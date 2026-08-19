# nanollm

OpenAI-compatible reverse proxy for LLM APIs. Point coding tools at nanollm and map one client-facing model name to one or more upstream providers.

Providers for a model are tried **in order**. The next provider is used only when the current one is catastrophically unavailable. Rate limits, 4xx, and ordinary 5xx stay on the same provider so prompt cache is not thrown away.

## Features

- Standard `net/http` server, OpenAI-compatible `/v1/chat/completions` (plus completions, embeddings, responses)
- YAML config: named models, named providers, auth merged into provider `headers`
- Incoming API keys (`api_keys[].{name,value}`)
- Streaming SSE pass-through
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
| `api_keys[].name` | yes | Identifier for this key (must be unique) |
| `api_keys[].value` | yes | Secret the client must send |
| `models[].name` | yes | Client-facing model id |
| `models[].providers` | yes | Ordered upstream list |
| `providers[].name` | yes | Identifier for this provider within a model (must be unique) |
| `providers[].url` | yes | Full `http`/`https` upstream URL (not a base URL) |
| `providers[].model` | no | Model string sent upstream; if empty, the client model is kept |
| `providers[].headers` | no | Extra request headers, including upstream auth |

Rules:

- At least one API key and one model
- Model names unique; provider names unique **within** a model
- API key names unique; API key values unique
- Incoming `Authorization` / `X-Api-Key` / `Api-Key` are **not** forwarded
- Request bodies larger than 64 MiB return `413`
- `config.yaml` is gitignored; commit `config.example.yaml` only

See `config.example.yaml`.

## Authentication

Every route except `GET /healthz` requires a configured key:

- `Authorization: Bearer <value>` (OpenAI clients; `Bearer` is case-insensitive)
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
| `GET` | `/v1/models/{model}` | yes | Single configured model (ids may contain `/`) |
| `POST` | `/v1/chat/completions` | yes | Also `/chat/completions` |
| `POST` | `/v1/completions` | yes | Also `/completions` |
| `POST` | `/v1/embeddings` | yes | Also `/embeddings` |
| `POST` | `/v1/responses` | yes | |

The JSON body `model` field selects `models[].name`. nanollm rewrites it to `providers[].model` and POSTs to `providers[].url`.

Streaming (`"stream": true`) is copied through as SSE. Request fields such as `stream_options` are forwarded as-is.

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
