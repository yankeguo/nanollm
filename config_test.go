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
models:
  - name: fast
    providers:
      - name: primary
        model: gpt-4o-mini
        headers:
          Authorization: Bearer sk-primary
          X-Extra: one
        openai_completions:
          url: http://primary.example/v1/chat/completions
      - name: backup
        model: llama3
        headers:
          Authorization: Bearer sk-backup
        openai_completions:
          url: http://backup.example/v1/chat/completions
  - name: embed
    providers:
      - name: openai
        model: text-embedding-3-small
        openai_completions:
          url: http://embed.example/v1/embeddings
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
	require.Len(t, cfg.providersFor("fast", formatOpenAICompletions), 2)
	require.Empty(t, cfg.providersFor("fast", formatAnthropicMessages))
}

func TestLoadConfigRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("models: []\n"), 0o644))
	_, err := loadConfig(path)
	require.Error(t, err)
}

func TestLoadConfigRejectsDuplicateNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
api_keys:
  - name: alice
    value: sk-alice
models:
  - name: fast
    providers:
      - name: a
        openai_completions:
          url: http://a.example
      - name: a
        openai_completions:
          url: http://b.example
`), 0o644))
	_, err := loadConfig(path)
	require.Error(t, err)
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
models:
  - name: fast
    providers:
      - name: a
        openai_completions:
          url: http://a.example
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
models:
  - name: fast
    providers:
      - name: a
        openai_completions:
          url: ftp://files.example/model
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
        openai_completions:
          url: http://a.example
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
models:
  - name: fast
    providers:
      - name: a
        openai_completions:
          url: http://a.example
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
models:
  - name: fast
    providers:
      - name: a
        openai_completions:
          url: http://a.example
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
models:
  - name: fast
    providers:
      - name: a
        openai_completions:
          url: http://a.example
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
models:
  - name: fast
    providers:
      - name: a
        openai_completions:
          url: http://a.example
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
models:
  - name: fast
    providers:
      - name: a
        openai_completions:
          url: http://a.example
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
models:
  - name: fast
    providers:
      - name: a
        openai_completions:
          url: http://a.example
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
models:
  - name: claude
    providers:
      - name: a
        url: http://a.example
`), 0o644))
	_, err := loadConfig(path)
	require.ErrorContains(t, err, "must set openai_completions, openai_responses, or anthropic_messages")
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
models:
  - name: claude
    providers:
      - name: openrouter
        model: anthropic/claude-sonnet-4-5
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
`), 0o644))
	cfg, err := loadConfig(path)
	require.NoError(t, err)

	or := cfg.providers("claude")[0]
	require.Equal(t, "openrouter", or.Name)
	require.NotNil(t, or.OpenAICompletions)
	require.NotNil(t, or.OpenAIResponses)
	require.NotNil(t, or.AnthropicMessages)

	openai := cfg.providersFor("claude", formatOpenAICompletions)
	require.Len(t, openai, 1)
	require.Equal(t, "openrouter", openai[0].Name)
	u, model, headers, ok := openai[0].resolve(formatOpenAICompletions)
	require.True(t, ok)
	require.Equal(t, "https://openrouter.ai/api/v1/chat/completions", u)
	require.Equal(t, "anthropic/claude-sonnet-4-5", model)
	require.Equal(t, "Bearer sk-or", headers["Authorization"])
	require.Equal(t, "openai", headers["X-Shared"])

	responses := cfg.providersFor("claude", formatOpenAIResponses)
	require.Len(t, responses, 1)
	require.Equal(t, "openrouter", responses[0].Name)
	u, model, headers, ok = responses[0].resolve(formatOpenAIResponses)
	require.True(t, ok)
	require.Equal(t, "https://openrouter.ai/api/v1/responses", u)
	require.Equal(t, "openai/gpt-4o", model)
	require.Equal(t, "Bearer sk-or", headers["Authorization"])
	require.Equal(t, "top", headers["X-Shared"])

	anth := cfg.providersFor("claude", formatAnthropicMessages)
	require.Len(t, anth, 2)
	require.Equal(t, "openrouter", anth[0].Name)
	require.Equal(t, "anthropic-only", anth[1].Name)
	u, model, headers, ok = anth[0].resolve(formatAnthropicMessages)
	require.True(t, ok)
	require.Equal(t, "https://openrouter.ai/api/v1/messages", u)
	require.Equal(t, "anthropic/claude-override", model)
	require.Equal(t, "Bearer sk-or", headers["Authorization"])
	require.Equal(t, "top", headers["X-Shared"])
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
models:
  - name: claude
    providers:
      - name: a
        openai_completions: {}
`), 0o644))
	_, err := loadConfig(path)
	require.ErrorContains(t, err, "openai_completions url is required")
}
