package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
mysql:
  dsn: nanollm:REPLACE_ME@tcp(127.0.0.1:3306)/nanollm
admin:
  username: admin
  password: REPLACE_ME
api_keys:
  - name: alice
    value: sk-alice
providers:
  - name: primary
    headers:
      Authorization: Bearer sk-primary
      X-Extra: one
    openai_completions:
      url: http://primary.example/v1/chat/completions
  - name: backup
    headers:
      Authorization: Bearer sk-backup
    openai_completions:
      url: http://backup.example/v1/chat/completions
  - name: openai
    openai_embeddings:
      url: http://embed.example/v1/embeddings
models:
  - name: fast
    providers:
      - name: primary
        model: gpt-4o-mini
        protocols: [openai_completions]
      - name: backup
        model: llama3
        protocols: [openai_completions]
  - name: embed
    providers:
      - name: openai
        model: text-embedding-3-small
        protocols: [openai_embeddings]
`), 0o644))

	cfg, err := loadConfig(path)
	require.NoError(t, err)
	require.Len(t, cfg.Models, 2)
	require.Equal(t, "gpt-4o-mini", cfg.providers("fast")[0].Model)
	require.Equal(t, "alice", cfg.APIKeys[0].Name)
	require.Equal(t, "sk-alice", cfg.APIKeys[0].Value)
	require.Equal(t, "primary", cfg.providers("fast")[0].Name)
	require.Equal(t, "Bearer sk-primary", cfg.providers("fast")[0].Headers["Authorization"])
	require.Len(t, cfg.providers("fast"), 2)
	require.Equal(t, "nanollm:REPLACE_ME@tcp(127.0.0.1:3306)/nanollm", cfg.MySQL.DSN)
	require.Equal(t, 1000, cfg.MySQL.detailRetain())
	require.Equal(t, "admin", cfg.Admin.Username)
	require.Equal(t, "REPLACE_ME", cfg.Admin.Password)
	require.NotNil(t, cfg.providers("fast")[0].OpenAICompletions)
	require.Equal(t, "http://primary.example/v1/chat/completions", cfg.providers("fast")[0].OpenAICompletions.URL)
	require.Len(t, cfg.providersFor("fast", protocolOpenAICompletions), 2)
	require.Empty(t, cfg.providersFor("fast", protocolAnthropicMessages))
	require.NotNil(t, cfg.providers("embed")[0].OpenAIEmbeddings)
	require.Equal(t, "http://embed.example/v1/embeddings", cfg.providers("embed")[0].OpenAIEmbeddings.URL)
	require.Len(t, cfg.providersFor("embed", protocolOpenAIEmbeddings), 1)
	require.Empty(t, cfg.providersFor("embed", protocolOpenAICompletions))
}

func TestLoadConfigRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("models: []\n"), 0o644))
	_, err := loadConfig(path)
	require.Error(t, err)
}

func TestLoadConfigRejectsDuplicateProviderNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
api_keys:
  - name: alice
    value: sk-alice
providers:
  - name: a
    openai_completions:
      url: http://a.example
  - name: a
    openai_completions:
      url: http://b.example
models:
  - name: fast
    providers:
      - name: a
        protocols: [openai_completions]
`), 0o644))
	_, err := loadConfig(path)
	require.ErrorContains(t, err, "duplicate provider name")
}

func TestLoadConfigRejectsDuplicateProviderRefs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
api_keys:
  - name: alice
    value: sk-alice
providers:
  - name: a
    openai_completions:
      url: http://a.example
models:
  - name: fast
    providers:
      - name: a
        protocols: [openai_completions]
      - name: a
        protocols: [openai_completions]
`), 0o644))
	_, err := loadConfig(path)
	require.ErrorContains(t, err, "duplicate provider name")
}

func TestLoadConfigRejectsDuplicateKeyValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
api_keys:
  - name: alice
    value: sk-same
  - name: bob
    value: sk-same
providers:
  - name: a
    openai_completions:
      url: http://a.example
models:
  - name: fast
    providers:
      - name: a
        protocols: [openai_completions]
`), 0o644))
	_, err := loadConfig(path)
	require.Error(t, err)
}

func TestLoadConfigRejectsNonHTTPURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
api_keys:
  - name: alice
    value: sk-alice
providers:
  - name: a
    openai_completions:
      url: ftp://files.example/model
models:
  - name: fast
    providers:
      - name: a
        protocols: [openai_completions]
`), 0o644))
	_, err := loadConfig(path)
	require.ErrorContains(t, err, "http or https")
}

func TestLoadConfigRequiresAPIKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  - name: fast
    providers:
      - name: a
        protocols: [openai_completions]
`), 0o644))
	_, err := loadConfig(path)
	require.ErrorContains(t, err, "api_key")
}

func TestLoadConfigRequiresMySQLDSN(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
api_keys:
  - name: alice
    value: sk-alice
providers:
  - name: a
    openai_completions:
      url: http://a.example
models:
  - name: fast
    providers:
      - name: a
        protocols: [openai_completions]
`), 0o644))
	_, err := loadConfig(path)
	require.ErrorContains(t, err, "mysql.dsn")
}

func TestLoadConfigDetailRetain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
mysql:
  dsn: nanollm:REPLACE_ME@tcp(127.0.0.1:3306)/nanollm
  detail_retain: 0
admin:
  username: admin
  password: REPLACE_ME
api_keys:
  - name: alice
    value: sk-alice
providers:
  - name: a
    openai_completions:
      url: http://a.example
models:
  - name: fast
    providers:
      - name: a
        protocols: [openai_completions]
`), 0o644))
	cfg, err := loadConfig(path)
	require.NoError(t, err)
	require.Equal(t, 0, cfg.MySQL.detailRetain())

	require.NoError(t, os.WriteFile(path, []byte(`
mysql:
  dsn: nanollm:REPLACE_ME@tcp(127.0.0.1:3306)/nanollm
  detail_retain: -1
api_keys:
  - name: alice
    value: sk-alice
providers:
  - name: a
    openai_completions:
      url: http://a.example
models:
  - name: fast
    providers:
      - name: a
        protocols: [openai_completions]
`), 0o644))
	_, err = loadConfig(path)
	require.ErrorContains(t, err, "detail_retain")
}

func TestLoadConfigRequiresAdmin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
mysql:
  dsn: nanollm:REPLACE_ME@tcp(127.0.0.1:3306)/nanollm
api_keys:
  - name: alice
    value: sk-alice
providers:
  - name: a
    openai_completions:
      url: http://a.example
models:
  - name: fast
    providers:
      - name: a
        protocols: [openai_completions]
`), 0o644))
	_, err := loadConfig(path)
	require.ErrorContains(t, err, "admin.username")

	require.NoError(t, os.WriteFile(path, []byte(`
mysql:
  dsn: nanollm:REPLACE_ME@tcp(127.0.0.1:3306)/nanollm
admin:
  username: admin
api_keys:
  - name: alice
    value: sk-alice
providers:
  - name: a
    openai_completions:
      url: http://a.example
models:
  - name: fast
    providers:
      - name: a
        protocols: [openai_completions]
`), 0o644))
	_, err = loadConfig(path)
	require.ErrorContains(t, err, "admin.password")
}

func TestLoadConfigTrimsAdminUsername(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
mysql:
  dsn: nanollm:REPLACE_ME@tcp(127.0.0.1:3306)/nanollm
admin:
  username: "  admin  "
  password: REPLACE_ME
api_keys:
  - name: alice
    value: sk-alice
providers:
  - name: a
    openai_completions:
      url: http://a.example
models:
  - name: fast
    providers:
      - name: a
        protocols: [openai_completions]
`), 0o644))
	cfg, err := loadConfig(path)
	require.NoError(t, err)
	require.Equal(t, "admin", cfg.Admin.Username)
}

func TestLoadConfigRequiresProtocolBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
mysql:
  dsn: nanollm:REPLACE_ME@tcp(127.0.0.1:3306)/nanollm
admin:
  username: admin
  password: REPLACE_ME
api_keys:
  - name: alice
    value: sk-alice
providers:
  - name: a
    url: http://a.example
models:
  - name: claude
    providers:
      - name: a
        protocols: [openai_completions]
`), 0o644))
	_, err := loadConfig(path)
	require.ErrorContains(t, err, "must set openai_completions, openai_responses, openai_embeddings, or anthropic_messages")
}

func TestLoadConfigNestedProviderEndpoints(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
mysql:
  dsn: nanollm:REPLACE_ME@tcp(127.0.0.1:3306)/nanollm
admin:
  username: admin
  password: REPLACE_ME
api_keys:
  - name: alice
    value: sk-alice
providers:
  - name: openrouter
    headers:
      Authorization: Bearer sk-or
      X-Shared: top
    openai_completions:
      url: https://openrouter.ai/api/v1/chat/completions
      headers:
        X-Shared: openai
    openai_responses:
      url: https://openrouter.ai/api/v1/responses
      model: openai/gpt-4o
    anthropic_messages:
      url: https://openrouter.ai/api/v1/messages
      model: anthropic/claude-override
  - name: anthropic-only
    anthropic_messages:
      url: https://api.anthropic.com/v1/messages
      model: claude-sonnet-4-5
models:
  - name: claude
    providers:
      - name: openrouter
        model: anthropic/claude-sonnet-4-5
        protocols: [openai_completions, openai_responses, anthropic_messages]
      - name: anthropic-only
        protocols: [anthropic_messages]
`), 0o644))
	cfg, err := loadConfig(path)
	require.NoError(t, err)

	or := cfg.providers("claude")[0]
	require.Equal(t, "openrouter", or.Name)
	require.NotNil(t, or.OpenAICompletions)
	require.NotNil(t, or.OpenAIResponses)
	require.NotNil(t, or.AnthropicMessages)

	openai := cfg.providersFor("claude", protocolOpenAICompletions)
	require.Len(t, openai, 1)
	require.Equal(t, "openrouter", openai[0].Name)
	u, model, headers, ok := openai[0].resolve(protocolOpenAICompletions)
	require.True(t, ok)
	require.Equal(t, "https://openrouter.ai/api/v1/chat/completions", u)
	require.Equal(t, "anthropic/claude-sonnet-4-5", model)
	require.Equal(t, "Bearer sk-or", headers["Authorization"])
	require.Equal(t, "openai", headers["X-Shared"])

	responses := cfg.providersFor("claude", protocolOpenAIResponses)
	require.Len(t, responses, 1)
	require.Equal(t, "openrouter", responses[0].Name)
	u, model, headers, ok = responses[0].resolve(protocolOpenAIResponses)
	require.True(t, ok)
	require.Equal(t, "https://openrouter.ai/api/v1/responses", u)
	require.Equal(t, "openai/gpt-4o", model)
	require.Equal(t, "Bearer sk-or", headers["Authorization"])
	require.Equal(t, "top", headers["X-Shared"])

	anth := cfg.providersFor("claude", protocolAnthropicMessages)
	require.Len(t, anth, 2)
	require.Equal(t, "openrouter", anth[0].Name)
	require.Equal(t, "anthropic-only", anth[1].Name)
	u, model, headers, ok = anth[0].resolve(protocolAnthropicMessages)
	require.True(t, ok)
	require.Equal(t, "https://openrouter.ai/api/v1/messages", u)
	require.Equal(t, "anthropic/claude-override", model)
	require.Equal(t, "Bearer sk-or", headers["Authorization"])
	require.Equal(t, "top", headers["X-Shared"])
}

func TestLoadConfigAllowsEmbeddingsWithCompletions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
mysql:
  dsn: nanollm:REPLACE_ME@tcp(127.0.0.1:3306)/nanollm
admin:
  username: admin
  password: REPLACE_ME
api_keys:
  - name: alice
    value: sk-alice
providers:
  - name: openai
    openai_completions:
      url: https://api.openai.com/v1/chat/completions
    openai_embeddings:
      url: https://api.openai.com/v1/embeddings
      model: text-embedding-3-small
models:
  - name: dual
    providers:
      - name: openai
        protocols: [openai_completions, openai_embeddings]
`), 0o644))
	cfg, err := loadConfig(path)
	require.NoError(t, err)

	p := cfg.providers("dual")[0]
	require.NotNil(t, p.OpenAICompletions)
	require.NotNil(t, p.OpenAIEmbeddings)
	require.Len(t, cfg.providersFor("dual", protocolOpenAICompletions), 1)
	require.Len(t, cfg.providersFor("dual", protocolOpenAIEmbeddings), 1)
	u, model, _, ok := p.resolve(protocolOpenAIEmbeddings)
	require.True(t, ok)
	require.Equal(t, "https://api.openai.com/v1/embeddings", u)
	require.Equal(t, "text-embedding-3-small", model)
}

func TestLoadConfigSharedProviderProtocolSubset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
mysql:
  dsn: nanollm:REPLACE_ME@tcp(127.0.0.1:3306)/nanollm
admin:
  username: admin
  password: REPLACE_ME
api_keys:
  - name: alice
    value: sk-alice
providers:
  - name: openai
    headers:
      Authorization: Bearer sk-up
    openai_completions:
      url: http://chat.example/v1/chat/completions
    openai_embeddings:
      url: http://embed.example/v1/embeddings
models:
  - name: gpt-4o
    providers:
      - name: openai
        model: gpt-4o
        protocols: [openai_completions]
  - name: embed
    providers:
      - name: openai
        model: text-embedding-3-small
        protocols: [openai_embeddings]
`), 0o644))
	cfg, err := loadConfig(path)
	require.NoError(t, err)

	chat := cfg.providers("gpt-4o")
	require.Len(t, chat, 1)
	require.Equal(t, "gpt-4o", chat[0].Model)
	require.NotNil(t, chat[0].OpenAICompletions)
	require.Nil(t, chat[0].OpenAIEmbeddings)
	require.Len(t, cfg.providersFor("gpt-4o", protocolOpenAICompletions), 1)
	require.Empty(t, cfg.providersFor("gpt-4o", protocolOpenAIEmbeddings))

	embed := cfg.providers("embed")
	require.Len(t, embed, 1)
	require.Equal(t, "text-embedding-3-small", embed[0].Model)
	require.Nil(t, embed[0].OpenAICompletions)
	require.NotNil(t, embed[0].OpenAIEmbeddings)
	require.Empty(t, cfg.providersFor("embed", protocolOpenAICompletions))
	require.Len(t, cfg.providersFor("embed", protocolOpenAIEmbeddings), 1)
	require.Equal(t, "Bearer sk-up", embed[0].Headers["Authorization"])
}

func TestLoadConfigRejectsUnknownProviderRef(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
mysql:
  dsn: nanollm:REPLACE_ME@tcp(127.0.0.1:3306)/nanollm
admin:
  username: admin
  password: REPLACE_ME
api_keys:
  - name: alice
    value: sk-alice
providers:
  - name: openai
    openai_completions:
      url: http://a.example
models:
  - name: fast
    providers:
      - name: missing
        protocols: [openai_completions]
`), 0o644))
	_, err := loadConfig(path)
	require.ErrorContains(t, err, "unknown provider")
}

func TestLoadConfigRejectsProtocolNotOnProvider(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
mysql:
  dsn: nanollm:REPLACE_ME@tcp(127.0.0.1:3306)/nanollm
admin:
  username: admin
  password: REPLACE_ME
api_keys:
  - name: alice
    value: sk-alice
providers:
  - name: openai
    openai_completions:
      url: http://a.example
models:
  - name: fast
    providers:
      - name: openai
        protocols: [openai_embeddings]
`), 0o644))
	_, err := loadConfig(path)
	require.ErrorContains(t, err, "has no openai_embeddings block")
}

func TestLoadConfigRejectsMissingProtocols(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
mysql:
  dsn: nanollm:REPLACE_ME@tcp(127.0.0.1:3306)/nanollm
admin:
  username: admin
  password: REPLACE_ME
api_keys:
  - name: alice
    value: sk-alice
providers:
  - name: openai
    openai_completions:
      url: http://a.example
models:
  - name: fast
    providers:
      - name: openai
`), 0o644))
	_, err := loadConfig(path)
	require.ErrorContains(t, err, "protocols is required")
}

func TestLoadConfigRejectsInvalidProtocol(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
mysql:
  dsn: nanollm:REPLACE_ME@tcp(127.0.0.1:3306)/nanollm
admin:
  username: admin
  password: REPLACE_ME
api_keys:
  - name: alice
    value: sk-alice
providers:
  - name: openai
    openai_completions:
      url: http://a.example
models:
  - name: fast
    providers:
      - name: openai
        protocols: [not_a_protocol]
`), 0o644))
	_, err := loadConfig(path)
	require.ErrorContains(t, err, "is invalid")
}

func TestLoadConfigRequiresEndpointURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
mysql:
  dsn: nanollm:REPLACE_ME@tcp(127.0.0.1:3306)/nanollm
admin:
  username: admin
  password: REPLACE_ME
api_keys:
  - name: alice
    value: sk-alice
providers:
  - name: a
    openai_completions: {}
models:
  - name: claude
    providers:
      - name: a
        protocols: [openai_completions]
`), 0o644))
	_, err := loadConfig(path)
	require.ErrorContains(t, err, "openai_completions url is required")
}
