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
        url: http://primary.example/v1/chat/completions
        model: gpt-4o-mini
        headers:
          Authorization: Bearer sk-primary
          X-Extra: one
      - name: backup
        url: http://backup.example/v1/chat/completions
        model: llama3
        headers:
          Authorization: Bearer sk-backup
  - name: embed
    providers:
      - name: openai
        url: http://embed.example/v1/embeddings
        model: text-embedding-3-small
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
	require.Empty(t, cfg.providers("fast")[0].Format)
	require.Empty(t, cfg.providers("fast")[0].URL)
	require.NotNil(t, cfg.providers("fast")[0].OpenAI)
	require.Equal(t, "http://primary.example/v1/chat/completions", cfg.providers("fast")[0].OpenAI.URL)
	require.Len(t, cfg.providersFor("fast", formatOpenAI), 2)
	require.Empty(t, cfg.providersFor("fast", formatAnthropic))
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
        url: http://a.example
      - name: a
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
        url: http://a.example
`), 0o644))
	cfg, err := loadConfig(path)
	require.NoError(t, err)
	require.Equal(t, "admin", cfg.Admin.Username)
}

func TestLoadConfigProviderFormat(t *testing.T) {
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
      - name: anthropic
        format: anthropic
        url: https://api.anthropic.com/v1/messages
        model: claude-sonnet-4-5
      - name: openrouter
        url: https://openrouter.ai/api/v1/chat/completions
        model: anthropic/claude-sonnet-4-5
`), 0o644))
	cfg, err := loadConfig(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.providers("claude")[0].Anthropic)
	require.Nil(t, cfg.providers("claude")[0].OpenAI)
	require.Equal(t, "https://api.anthropic.com/v1/messages", cfg.providers("claude")[0].Anthropic.URL)
	require.NotNil(t, cfg.providers("claude")[1].OpenAI)
	require.Nil(t, cfg.providers("claude")[1].Anthropic)
	require.Len(t, cfg.providersFor("claude", formatAnthropic), 1)
	require.Equal(t, "anthropic", cfg.providersFor("claude", formatAnthropic)[0].Name)
	require.Len(t, cfg.providersFor("claude", formatOpenAI), 1)
	require.Equal(t, "openrouter", cfg.providersFor("claude", formatOpenAI)[0].Name)

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
        format: grpc
        url: http://a.example
`), 0o644))
	_, err = loadConfig(path)
	require.ErrorContains(t, err, "format must be openai or anthropic")
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
        openai:
          url: https://openrouter.ai/api/v1/chat/completions
          headers:
            X-Shared: openai
        anthropic:
          url: https://openrouter.ai/api/v1/messages
          model: anthropic/claude-override
      - name: anthropic-only
        anthropic:
          url: https://api.anthropic.com/v1/messages
          model: claude-sonnet-4-5
`), 0o644))
	cfg, err := loadConfig(path)
	require.NoError(t, err)

	or := cfg.providers("claude")[0]
	require.Equal(t, "openrouter", or.Name)
	require.NotNil(t, or.OpenAI)
	require.NotNil(t, or.Anthropic)
	require.Empty(t, or.URL)
	require.Empty(t, or.Format)

	openai := cfg.providersFor("claude", formatOpenAI)
	require.Len(t, openai, 1)
	require.Equal(t, "openrouter", openai[0].Name)
	u, model, headers, ok := openai[0].resolve(formatOpenAI)
	require.True(t, ok)
	require.Equal(t, "https://openrouter.ai/api/v1/chat/completions", u)
	require.Equal(t, "anthropic/claude-sonnet-4-5", model)
	require.Equal(t, "Bearer sk-or", headers["Authorization"])
	require.Equal(t, "openai", headers["X-Shared"])

	anth := cfg.providersFor("claude", formatAnthropic)
	require.Len(t, anth, 2)
	require.Equal(t, "openrouter", anth[0].Name)
	require.Equal(t, "anthropic-only", anth[1].Name)
	u, model, headers, ok = anth[0].resolve(formatAnthropic)
	require.True(t, ok)
	require.Equal(t, "https://openrouter.ai/api/v1/messages", u)
	require.Equal(t, "anthropic/claude-override", model)
	require.Equal(t, "Bearer sk-or", headers["Authorization"])
	require.Equal(t, "top", headers["X-Shared"])
}

func TestLoadConfigRejectsMixedURLAndBlock(t *testing.T) {
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
        url: http://a.example/v1/chat/completions
        openai:
          url: http://b.example/v1/chat/completions
`), 0o644))
	_, err := loadConfig(path)
	require.ErrorContains(t, err, "cannot mix top-level url/format")
}
