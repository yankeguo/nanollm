package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const defaultDetailRetain = 1000

type Config struct {
	MySQL   MySQLConfig   `yaml:"mysql"`
	Admin   AdminConfig   `yaml:"admin"`
	APIKeys []APIKey      `yaml:"api_keys"`
	Models  []ModelConfig `yaml:"models"`
}

type AdminConfig struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type MySQLConfig struct {
	DSN          string `yaml:"dsn"`
	DetailRetain *int   `yaml:"detail_retain"`
}

func (m MySQLConfig) detailRetain() int {
	if m.DetailRetain == nil {
		return defaultDetailRetain
	}
	return *m.DetailRetain
}

type APIKey struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type ModelConfig struct {
	Name      string     `yaml:"name"`
	Providers []Provider `yaml:"providers"`
}

type Provider struct {
	Name    string            `yaml:"name"`
	URL     string            `yaml:"url"`
	Model   string            `yaml:"model"`
	Headers map[string]string `yaml:"headers"`
}

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if c == nil || len(c.APIKeys) == 0 {
		return fmt.Errorf("config: at least one api_key is required")
	}
	seenKeyName := make(map[string]struct{}, len(c.APIKeys))
	seenKeyValue := make(map[string]struct{}, len(c.APIKeys))
	for i, k := range c.APIKeys {
		if k.Name == "" {
			return fmt.Errorf("config: api_keys[%d] name is required", i)
		}
		if k.Value == "" {
			return fmt.Errorf("config: api_keys[%d] value is required", i)
		}
		if _, dup := seenKeyName[k.Name]; dup {
			return fmt.Errorf("config: duplicate api_key name %q", k.Name)
		}
		if _, dup := seenKeyValue[k.Value]; dup {
			return fmt.Errorf("config: duplicate api_key value for %q", k.Name)
		}
		seenKeyName[k.Name] = struct{}{}
		seenKeyValue[k.Value] = struct{}{}
	}
	if len(c.Models) == 0 {
		return fmt.Errorf("config: at least one model is required")
	}
	seenModel := make(map[string]struct{}, len(c.Models))
	for i, m := range c.Models {
		if m.Name == "" {
			return fmt.Errorf("config: models[%d] name is required", i)
		}
		if _, dup := seenModel[m.Name]; dup {
			return fmt.Errorf("config: duplicate model name %q", m.Name)
		}
		seenModel[m.Name] = struct{}{}
		if len(m.Providers) == 0 {
			return fmt.Errorf("config: model %q has no providers", m.Name)
		}
		seenProvider := make(map[string]struct{}, len(m.Providers))
		for j, p := range m.Providers {
			if p.Name == "" {
				return fmt.Errorf("config: model %q providers[%d] name is required", m.Name, j)
			}
			if _, dup := seenProvider[p.Name]; dup {
				return fmt.Errorf("config: model %q has duplicate provider name %q", m.Name, p.Name)
			}
			seenProvider[p.Name] = struct{}{}
			if p.URL == "" {
				return fmt.Errorf("config: model %q provider %q url is required", m.Name, p.Name)
			}
			u, err := url.Parse(p.URL)
			if err != nil {
				return fmt.Errorf("config: model %q provider %q url is invalid: %w", m.Name, p.Name, err)
			}
			if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("config: model %q provider %q url must be http or https with a host", m.Name, p.Name)
			}
		}
	}
	if strings.TrimSpace(c.MySQL.DSN) == "" {
		return fmt.Errorf("config: mysql.dsn is required")
	}
	if _, err := normalizeMySQLDSN(c.MySQL.DSN); err != nil {
		return fmt.Errorf("config: mysql.dsn is invalid: %w", err)
	}
	if c.MySQL.DetailRetain != nil && *c.MySQL.DetailRetain < 0 {
		return fmt.Errorf("config: mysql.detail_retain must be >= 0")
	}
	c.Admin.Username = strings.TrimSpace(c.Admin.Username)
	if c.Admin.Username == "" {
		return fmt.Errorf("config: admin.username is required")
	}
	if c.Admin.Password == "" {
		return fmt.Errorf("config: admin.password is required")
	}
	return nil
}

func (c *Config) model(name string) *ModelConfig {
	if c == nil {
		return nil
	}
	for i := range c.Models {
		if c.Models[i].Name == name {
			return &c.Models[i]
		}
	}
	return nil
}

func (c *Config) providers(model string) []Provider {
	m := c.model(model)
	if m == nil {
		return nil
	}
	return m.Providers
}

func (c *Config) modelNames() []string {
	if c == nil {
		return nil
	}
	names := make([]string, 0, len(c.Models))
	for _, m := range c.Models {
		names = append(names, m.Name)
	}
	return names
}
