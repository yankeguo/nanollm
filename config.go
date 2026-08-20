package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	defaultDetailRetain = 1000
	formatOpenAI        = "openai"
	formatAnthropic     = "anthropic"
	formatResponses     = "responses"
)

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

type ProviderEndpoint struct {
	URL     string            `yaml:"url"`
	Model   string            `yaml:"model"`
	Headers map[string]string `yaml:"headers"`
}

type Provider struct {
	Name      string            `yaml:"name"`
	Model     string            `yaml:"model"`
	Headers   map[string]string `yaml:"headers"`
	OpenAI    *ProviderEndpoint `yaml:"openai"`
	Responses *ProviderEndpoint `yaml:"responses"`
	Anthropic *ProviderEndpoint `yaml:"anthropic"`
	Format    string            `yaml:"format"`
	URL       string            `yaml:"url"`
}

// endpoint returns the protocol block for format. The top-level url/format
// fallback only matters for Provider values that never went through
// normalize (e.g. hand-built test fixtures); validate() folds the legacy
// fields into the matching block.
func (p Provider) endpoint(format string) *ProviderEndpoint {
	if format == "" {
		format = formatOpenAI
	}
	switch format {
	case formatOpenAI:
		if p.OpenAI != nil {
			return p.OpenAI
		}
	case formatResponses:
		if p.Responses != nil {
			return p.Responses
		}
	case formatAnthropic:
		if p.Anthropic != nil {
			return p.Anthropic
		}
	}
	if p.OpenAI == nil && p.Anthropic == nil && p.Responses == nil && p.URL != "" {
		legacy := strings.ToLower(strings.TrimSpace(p.Format))
		if legacy == "" {
			legacy = formatOpenAI
		}
		if legacy == format {
			return &ProviderEndpoint{URL: p.URL, Model: p.Model, Headers: p.Headers}
		}
	}
	return nil
}

func (p Provider) resolve(format string) (upstreamURL, model string, headers map[string]string, ok bool) {
	ep := p.endpoint(format)
	if ep == nil {
		return "", "", nil, false
	}
	model = ep.Model
	if model == "" {
		model = p.Model
	}
	return ep.URL, model, mergeHeaders(p.Headers, ep.Headers), true
}

func mergeHeaders(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
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
			if err := c.Models[i].Providers[j].normalize(m.Name); err != nil {
				return err
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

func (p *Provider) normalize(model string) error {
	nested := p.OpenAI != nil || p.Anthropic != nil || p.Responses != nil
	if nested {
		if p.URL != "" || strings.TrimSpace(p.Format) != "" {
			return fmt.Errorf("config: model %q provider %q cannot mix top-level url/format with openai/responses/anthropic blocks", model, p.Name)
		}
		if err := validateEndpointURL(model, p.Name, formatOpenAI, p.OpenAI); err != nil {
			return err
		}
		if err := validateEndpointURL(model, p.Name, formatResponses, p.Responses); err != nil {
			return err
		}
		if err := validateEndpointURL(model, p.Name, formatAnthropic, p.Anthropic); err != nil {
			return err
		}
		return nil
	}
	if p.URL == "" {
		return fmt.Errorf("config: model %q provider %q url is required", model, p.Name)
	}
	format := strings.ToLower(strings.TrimSpace(p.Format))
	if format == "" {
		format = formatOpenAI
	}
	if format != formatOpenAI && format != formatAnthropic && format != formatResponses {
		return fmt.Errorf("config: model %q provider %q format must be openai, responses, or anthropic", model, p.Name)
	}
	if err := validateHTTPURL(model, p.Name, "", p.URL); err != nil {
		return err
	}
	ep := &ProviderEndpoint{URL: p.URL}
	switch format {
	case formatAnthropic:
		p.Anthropic = ep
	case formatResponses:
		p.Responses = ep
	default:
		p.OpenAI = ep
	}
	p.URL = ""
	p.Format = ""
	return nil
}

func validateEndpointURL(model, provider, format string, ep *ProviderEndpoint) error {
	if ep == nil {
		return nil
	}
	return validateHTTPURL(model, provider, format, ep.URL)
}

func validateHTTPURL(model, provider, format, raw string) error {
	what := "url"
	if format != "" {
		what = format + " url"
	}
	if raw == "" {
		return fmt.Errorf("config: model %q provider %q %s is required", model, provider, what)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("config: model %q provider %q %s is invalid: %w", model, provider, what, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("config: model %q provider %q %s must be http or https with a host", model, provider, what)
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

func (c *Config) providersFor(model, format string) []Provider {
	all := c.providers(model)
	if format == "" {
		format = formatOpenAI
	}
	out := make([]Provider, 0, len(all))
	for _, p := range all {
		if p.endpoint(format) != nil {
			out = append(out, p)
		}
	}
	return out
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
