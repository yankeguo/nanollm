# nanollm

OpenAI-compatible LLM reverse proxy. Forwards chat/completions (and related) requests to configured providers, and exports token usage over OTLP/HTTP so you can see how many tokens an opaque coding plan actually delivers.

A model can list multiple providers. They are tried in order. The next provider is used only when the current one is catastrophically unavailable (unreachable, DNS/TLS failure, or HTTP 502/503/504). Rate limits, 4xx, and ordinary 5xx are returned as-is so prompt cache stays on the same provider.

## Usage

```bash
cp config.example.yaml config.yaml
go run . -config config.yaml -listen :8080
```

Environment variables `NANOLLM_CONFIG` and `NANOLLM_LISTEN` can be used instead of flags.

## Config

```yaml
api_keys:
  - name: default
    value: sk-your-key
models:
  - name: gpt-4o
    providers:
      - name: openai
        url: http://url.to/v1/chat/completions
        model: gpt-4o
        headers:
          Authorization: Bearer XXXXXX
          Some-Other-Header: value
      - name: backup
        url: http://backup.to/v1/chat/completions
        model: other-model
        headers:
          Authorization: Bearer YYYYYY
```

Clients authenticate with `Authorization: Bearer <api_keys.value>` (or `X-Api-Key` / `Api-Key`). Incoming credentials are not forwarded; upstream auth stays in provider `headers`.

Clients call `POST /v1/chat/completions` with `"model": "<name>"`. nanollm rewrites `model` to the provider's upstream model and POSTs to that `url`.

Also served:

- `GET /healthz`
- `GET /v1/models`
- `POST /v1/completions`
- `POST /v1/embeddings`
- `POST /v1/responses`

Streaming (`"stream": true`) is passed through. `stream_options.include_usage` is set when missing so token counts can be read from the last SSE chunk.

## Metrics

Custom counters (not GenAI semconv), exported with the standard OTLP/HTTP exporter.

**Instrument:** `nanollm.token.usage`  
**Type:** Counter  
**Unit:** `{token}`

Each request may add several data points that share model/provider labels and differ by `nanollm.token.type`:

| `nanollm.token.type` | Meaning |
|---|---|
| `input` | All input / prompt tokens (includes cache) |
| `output` | All output / completion tokens |
| `cache_read` | Input tokens served from provider cache |
| `cache_creation` | Input tokens written into provider cache |
| `uncached` | Input tokens that were not cache read or cache write |

Attributes:

- `nanollm.model` — client-facing model `name`
- `nanollm.provider` — provider `name`
- `nanollm.api_key` — matching `api_keys.name`
- `nanollm.upstream.model` — model string sent upstream
- `nanollm.token.type` — one of the values above

`input` overlaps the cache breakdown on purpose so dashboards can show both totals and cache hit rate:

```
cache hit ratio ≈ cache_read / input
uncached ≈ input - cache_read - cache_creation
```

Parsed from common OpenAI-compatible `usage` fields, including `prompt_tokens_details.cached_tokens`, DeepSeek `prompt_cache_hit_tokens` / `prompt_cache_miss_tokens`, and Anthropic `cache_read_input_tokens` / `cache_creation_input_tokens`.

The exporter reads the standard environment variables, including:

- `OTEL_SERVICE_NAME`
- `OTEL_RESOURCE_ATTRIBUTES`
- `OTEL_EXPORTER_OTLP_ENDPOINT` (base URL; `/v1/metrics` is appended)
- `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` (full URL, typically ending with `/v1/metrics`)
- `OTEL_EXPORTER_OTLP_HEADERS` / `OTEL_EXPORTER_OTLP_METRICS_HEADERS`
- `OTEL_EXPORTER_OTLP_TIMEOUT`
- `OTEL_EXPORTER_OTLP_COMPRESSION`
- `OTEL_METRIC_EXPORT_INTERVAL`
- `OTEL_SDK_DISABLED`

Example:

```bash
export OTEL_SERVICE_NAME=nanollm
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
go run . -config config.yaml
```

## License

MIT, GUO YANKE
