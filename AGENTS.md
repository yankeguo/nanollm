# Agent guide

Reply in the same language as the user.

nanollm is a small Go (1.27+) OpenAI- and Anthropic-compatible LLM reverse proxy. Treat the code as source of truth. Keep changes focused; do not add frameworks or extra services unless asked.

## Layout

Single `package main`. No `internal/` split unless the tree clearly outgrows one package.

| File | Role |
|---|---|
| `main.go` | Flags, listen address, graceful shutdown with no deadline (`github.com/yankeguo/rg`) |
| `config.go` | YAML load/validate |
| `auth.go` | Incoming API keys; do not forward client credentials |
| `server.go` | `net/http` mux (`GET /healthz` unauthenticated) |
| `admin.go` / `admin_auth.go` / `admin_filter.go` / `admin_query.go` | Admin cookie login, usage dashboard, call viewer. Usage and Calls share GET filters: `range`/`from`/`to`/`tz`, `model`, `provider`, `api_key`, `outcome` (from `http_status`/`error`; the four outcomes partition `llm_calls`: ok / canceled / no_response / error). Usage windows are always bounded (custom max 366d); the chart bucket is derived from the span (≤48h hour, ≤62d day, else week), never user-selected. Naive `from`/`to` are interpreted in `tz` (IANA, default UTC; `time/tzdata` is embedded). Call detail lists extracted files (`<img>` / `<video>` / `<audio>` / download) from `llm_call_files`. `/files/{sha}` serves bytes via `http.ServeContent` (Range support); stored MIME types are client-controlled, so only a safe inline allowlist is honored (`inlineFileMime`), everything else downloads as `application/octet-stream`, and `image/svg+xml` never renders inline. |
| `web/view/*.html` | Embedded HTML views for the admin dashboard (`/usage`, `/calls`, `/login`). Dark UI styled with Tailwind utilities plus the component classes from `main.css` (`.card`, `.btn*`, `.input`, `.tbl` with a sticky header, `.page-btn`, `.badge*`, token bars, `.codebox`, `.stat-icon`, `.empty`, `.copybtn`/`.copy-inline`); icons are `<span class="icon-[lucide--NAME]"></span>` (Iconify masks, colored by `currentColor`) |
| `admin_tmpl.go` | `//go:embed web/view/*.html`; template funcs `formatNum`, `formatMs`, `formatSpeed`, `inputBar`, `outputBar`, `statusClass`, `callErrorClass`, `jsAsset`, `cssAsset` |
| `web_static.go` | `//go:embed all:web/dist`; unauthenticated `GET /static/` with immutable cache; `jsAsset`/`cssAsset` match `<entry>-<hash>.js`/`.css` |
| `web/` | bun + TypeScript frontend for the admin UI plus the HTML views: entries in `src/entries/` (`main.css`, `admin.ts`, `usage.ts`) bundled by `build.ts` (`Bun.build`, IIFE, `[name]-[hash].[ext]`) into `dist/` — `main.css` is the full Tailwind v4 build via `bun-plugin-tailwind` plus `@iconify/tailwind4` lucide icons (`@source` scans `view/` and `src/`; dark palette and component classes defined there; keep utility-looking tokens such as `bg-*` out of CSS comments — the scanner reads candidates from comments too and the build fails); `admin.ts` runs on every page: timezone/range-filter logic, `[data-copy]` copy buttons (copies the previous sibling's text), and `tr[data-href]` row-click navigation; `usage.ts` renders the Chart.js charts from the inert `#chart-data` JSON block (empty series swap the canvas for an `.empty` block); `bun run build` / `bun run dev` (watch) / `bun run typecheck`; `dist` is git-ignored except `.gitkeep` |
| `proxy.go` | Body rewrite, ordered provider attempts, response copy, call logging |
| `failover.go` | `isCatastrophic` — only then try the next provider |
| `rewrite.go` | Patch top-level `model` (and OpenAI Chat Completions / Completions streaming `stream_options.include_usage`); copy other JSON fields as `json.RawMessage` |
| `usage.go` | Parse OpenAI- and Anthropic-compatible `usage` (including cache fields, Responses `response.usage`, embeddings `prompt_tokens`/`total_tokens`, and DashScope multimodal embedding `input_tokens` + top-level `image_tokens`; the tongyi `input_tokens_details` breakdown is never double-counted) from JSON/SSE. `copySSE` also returns the time of the first SSE `data:` line (keepalive comments excluded) for the TTFT metric |
| `sse_log.go` | Call-log only: coalesce consecutive same-type SSE text/argument deltas (including Chat Completions `function_call.arguments`) before storing `response_json`; does not change pass-through |
| `files.go` | Call-log only: extract multimodal base64 from request/response JSON into SHA256-deduped files; replace with `<file:{sha256}>` |
| `db.go` | GORM MySQL: `llm_calls` / `llm_files` / `llm_call_files` AutoMigrate, Record, prune detail blobs and unreferenced files |

Tests live next to the code (`*_test.go`). Use `github.com/stretchr/testify`. Prefer extending an existing test file over adding a new one for the same area.

## Hard constraints

- **std `net/http` only** for the server. No Fiber/Gin/Echo.
- **Failover is cache-preserving.** Next provider only on catastrophic unavailability: dial/DNS/TLS/other transport failure, or HTTP 502/503/504. Do **not** fail over on 429, 4xx, 500, or timeouts after the provider was reached. Once the client response has started, never try another provider.
- Config shape is `mysql.{dsn}`, top-level `detail_retain`, `admin.{username,password}`, `api_keys[].{name,value}`, top-level `providers[].{name,model,headers,openai_completions,openai_responses,openai_embeddings,anthropic_messages,bailian_multimodal_embedding}` and `models[].{name,providers[]}` with `models[].providers[].{name,model,protocols}`. Each vendor must set at least one protocol block `{url,model,headers}`. Inference and embeddings blocks may coexist; that split is not enforced. `url` is the full `http`/`https` upstream endpoint, not a base URL. Auth to upstream belongs in `headers`. Association `protocols` is a required non-empty subset of that vendor's blocks (chat-only vs embeddings-only stays a 404 at the proxy, not an upstream error). Unused top-level vendors are allowed.
- `providers[].name` is the vendor: globally unique, and `llm_calls.provider`. `models[].providers` order is failover order; names unique within a model. Every LLM route mirrors the vendor's official URL shape with the vendor host as the first path segment (`/api.openai.com/v1/...`, `/api.anthropic.com/v1/messages`, `/dashscope.aliyuncs.com/api/v1/services/embeddings/multimodal-embedding/multimodal-embedding`); this isolates vendor namespaces (e.g. both OpenAI and Anthropic have `/v1/files`) and no unprefixed legacy routes are served. OpenAI chat/completions routes only attempt associations that list `openai_completions`; `POST /api.openai.com/v1/embeddings` only those with `openai_embeddings`; `POST /api.openai.com/v1/responses` only those with `openai_responses`; `POST /api.anthropic.com/v1/messages` only those with `anthropic_messages`; the DashScope multimodal embedding route only those with `bailian_multimodal_embedding`. Proxy-generated error bodies match the vendor's shape (DashScope `{"code","message"}`). Skip associations that omit that protocol; do **not** convert bodies or fail over to the other protocol. Missing matching associations → 404.
- Request rewrite only patches `model` and, for OpenAI Chat Completions and Completions streaming, `stream_options.include_usage`. Responses (`stream_options.include_obfuscation` only) and Anthropic bodies are not given `include_usage`; embeddings never get `stream_options`. Other JSON fields must be copied as raw values (`json.RawMessage`), not `map[string]any` round-tripped. Upstream responses are copied byte-for-byte; usage parsing is inspect-only. Treat a body as SSE when `Content-Type` is `text/event-stream` (or `stream: true` and the type is not JSON); JSON errors on a streaming request stay JSON. Streaming copies emit SSE comments while the upstream body is idle so long thinking turns do not die on client/proxy idle timeouts, and drop `Content-Length` so those comments do not desync the client. There is no upstream `Timeout` / `ResponseHeaderTimeout`; wait as long as the client stays connected. Client cancel (before or after the response starts) is logged as `canceled`, not a transport failure, and never triggers failover. `http.Server.Shutdown` uses an unbounded context: SIGINT/SIGTERM stops accepting connections and waits indefinitely for in-flight requests; do not add a Shutdown timeout. After the first signal, SIGINT/SIGTERM is unregistered so a second signal can terminate.
- MySQL is required. DSN params `charset=utf8mb4`, `parseTime=true`, `loc=UTC`, `time_zone='UTC'` are forced. All DB timestamps are UTC.
- The admin dashboard lives at the root: `GET /` → 302 `/usage`; `/usage`, `/calls`, `/calls/{id}`, `/files/{sha}` are cookie-authenticated (HMAC, HttpOnly, SameSite=Lax; Secure when TLS or `X-Forwarded-Proto: https`), `/login` and `/logout` handle the session. It is not API-key auth. `admin.username` and `admin.password` are required. `GET /static/` is unauthenticated — the login page itself links the stylesheet and the content-hashed bundles carry no secrets. Everything is self-hosted (no CDN): admin CSP is `script-src 'self'`, `style-src 'self' 'unsafe-inline'` (icon masks are `data:` URLs under `img-src`); page JS lives in `web/src/entries/*.ts`, never inline, and the usage chart series is passed through an inert `<script type="application/json">` data block. `web/dist` is git-ignored (only `.gitkeep` is committed), so run the frontend build before `go build` — the Dockerfile does it in an `oven/bun` stage that mirrors the repo layout. Keep that layout: `main.css` pins its tailwind scan base with `@import "tailwindcss" source("../..")` and its `@source` globs resolve relative to `web/src/entries/`, so the build works at any absolute path, but a layout that moves `view/`/`src/` away from `web/` breaks the scan (and a base that escapes to a filesystem root makes tailwind walk everything and hang).
- Every provider attempt (success, 4xx/5xx, failover, dial failure, rewrite error, client cancel) is inserted into `llm_calls`. Each row also records `first_token_ms` (request sent → first token: first SSE `data:` line, or first body byte for non-streaming; 0 when no token arrived) and `output_speed` (output tokens/sec from first token to stream end; 0 when usage or the first token is missing). Detail JSON blobs are kept only for calls younger than `detail_retain` (default `168h`; `0` keeps no blobs). SSE `response_json` is stored as a JSON string; consecutive same-type text/argument deltas are coalesced in that transcript only (client still gets the raw stream). Before insert, request/response JSON copies have multimodal base64 replaced with `<file:{sha256}>`; decoded bytes are stored in `llm_files` (deduped) and linked via `llm_call_files`. This does not change upstream pass-through. File rows are GC'd with the same time cutoff using `llm_files.visited_at` (refreshed on reuse so a concurrent insert/join is not deleted; `created_at` stays the first insert). Blobs too large for `MEDIUMBLOB` skip extraction entirely.
- Incoming `Authorization` / `X-Api-Key` / `Api-Key` authenticate against `api_keys` and must not be copied upstream. Client `Cookie` is stripped from upstream requests; upstream `Set-Cookie` is dropped from responses.
- At least one API key is required; missing/invalid keys → 401 except `GET /healthz` and the admin pages (`/login` is public; `/usage` / `/calls` / `/files` redirect to it).
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
(cd web && bun run typecheck && bun run build)
go test ./...
gofmt -w .
```

After config, failover, auth, or call-logging changes, update `README.md` (operators) and this file (agents) in the same change when behavior diverges from what they currently say.
