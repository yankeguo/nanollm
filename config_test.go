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
