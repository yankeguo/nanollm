package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultDetailRetain = 168 * time.Hour

	protocolOpenAICompletions          = "openai_completions"
	protocolOpenAIResponses            = "openai_responses"
	protocolOpenAIEmbeddings           = "openai_embeddings"
	protocolAnthropicMessages          = "anthropic_messages"
	protocolBailianMultimodalEmbedding = "bailian_multimodal_embedding"
)

type Config struct {
	MySQL        MySQLConfig    `yaml:"mysql"`
	DetailRetain retainDuration `yaml:"detail_retain"`
	Admin        AdminConfig    `yaml:"admin"`
	APIKeys      []APIKey       `yaml:"api_keys"`
	Providers    []Provider     `yaml:"providers"`
	Models       []ModelConfig  `yaml:"models"`
}

type AdminConfig struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type MySQLConfig struct {
	DSN string `yaml:"dsn"`
}

func (c Config) detailRetain() time.Duration {
	if !c.DetailRetain.set {
		return defaultDetailRetain
	}
	return c.DetailRetain.d
}

// retainDuration is a YAML duration. Omitted uses defaultDetailRetain; 0 keeps
// no blobs. Go durations (168h, 24h) and a day unit (7d) are accepted.
type retainDuration struct {
	set bool
	d   time.Duration
}

func (r *retainDuration) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("config: detail_retain must be a duration")
	}
	if value.Tag == "!!null" || strings.TrimSpace(value.Value) == "" {
		return nil
	}
	d, err := parseRetainDuration(value.Value)
	if err != nil {
		return fmt.Errorf("config: detail_retain: %w", err)
	}
	r.set = true
	r.d = d
	return nil
}

func parseRetainDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("invalid duration")
	}
	if s == "0" {
		return 0, nil
	}
	var d time.Duration
	if strings.HasSuffix(s, "d") {
		days, err := strconv.ParseFloat(s[:len(s)-1], 64)
		if err != nil || s[:len(s)-1] == "" {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		d = time.Duration(days * 24 * float64(time.Hour))
	} else {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		d = parsed
	}
	if d < 0 {
		return 0, fmt.Errorf("must be >= 0")
	}
	return d, nil
}

type APIKey struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type ModelConfig struct {
	Name      string        `yaml:"name"`
	Providers []ProviderRef `yaml:"providers"`
}

// ProviderRef associates a named vendor with a client-facing model.
type ProviderRef struct {
	Name      string   `yaml:"name"`
	Model     string   `yaml:"model"`
	Protocols []string `yaml:"protocols"`
}

type ProviderEndpoint struct {
	URL     string            `yaml:"url"`
	Model   string            `yaml:"model"`
	Headers map[string]string `yaml:"headers"`
}

type Provider struct {
	Name                       string            `yaml:"name"`
	Model                      string            `yaml:"model"`
	Headers                    map[string]string `yaml:"headers"`
	OpenAICompletions          *ProviderEndpoint `yaml:"openai_completions"`
	OpenAIResponses            *ProviderEndpoint `yaml:"openai_responses"`
	OpenAIEmbeddings           *ProviderEndpoint `yaml:"openai_embeddings"`
	AnthropicMessages          *ProviderEndpoint `yaml:"anthropic_messages"`
	BailianMultimodalEmbedding *ProviderEndpoint `yaml:"bailian_multimodal_embedding"`
}

func isKnownProtocol(protocol string) bool {
	switch protocol {
	case protocolOpenAICompletions, protocolOpenAIResponses, protocolOpenAIEmbeddings, protocolAnthropicMessages, protocolBailianMultimodalEmbedding:
		return true
	default:
		return false
	}
}

func (p Provider) endpoint(protocol string) *ProviderEndpoint {
	switch protocol {
	case protocolOpenAICompletions:
		return p.OpenAICompletions
	case protocolOpenAIResponses:
		return p.OpenAIResponses
	case protocolOpenAIEmbeddings:
		return p.OpenAIEmbeddings
	case protocolAnthropicMessages:
		return p.AnthropicMessages
	case protocolBailianMultimodalEmbedding:
		return p.BailianMultimodalEmbedding
	}
	return nil
}

func (p Provider) bind(ref ProviderRef) Provider {
	out := p
	if ref.Model != "" {
		out.Model = ref.Model
	}
	allowed := make(map[string]struct{}, len(ref.Protocols))
	for _, protocol := range ref.Protocols {
		allowed[protocol] = struct{}{}
	}
	if _, ok := allowed[protocolOpenAICompletions]; !ok {
		out.OpenAICompletions = nil
	}
	if _, ok := allowed[protocolOpenAIResponses]; !ok {
		out.OpenAIResponses = nil
	}
	if _, ok := allowed[protocolOpenAIEmbeddings]; !ok {
		out.OpenAIEmbeddings = nil
	}
	if _, ok := allowed[protocolAnthropicMessages]; !ok {
		out.AnthropicMessages = nil
	}
	if _, ok := allowed[protocolBailianMultimodalEmbedding]; !ok {
		out.BailianMultimodalEmbedding = nil
	}
	return out
}

func (p Provider) resolve(protocol string) (upstreamURL, model string, headers map[string]string, ok bool) {
	ep := p.endpoint(protocol)
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
	if len(c.Providers) == 0 {
		return fmt.Errorf("config: at least one provider is required")
	}
	seenProvider := make(map[string]struct{}, len(c.Providers))
	byName := make(map[string]*Provider, len(c.Providers))
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.Name == "" {
			return fmt.Errorf("config: providers[%d] name is required", i)
		}
		if _, dup := seenProvider[p.Name]; dup {
			return fmt.Errorf("config: duplicate provider name %q", p.Name)
		}
		seenProvider[p.Name] = struct{}{}
		if err := p.validate(); err != nil {
			return err
		}
		byName[p.Name] = p
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
		seenRef := make(map[string]struct{}, len(m.Providers))
		for j, ref := range m.Providers {
			if ref.Name == "" {
				return fmt.Errorf("config: model %q providers[%d] name is required", m.Name, j)
			}
			if _, dup := seenRef[ref.Name]; dup {
				return fmt.Errorf("config: model %q has duplicate provider name %q", m.Name, ref.Name)
			}
			seenRef[ref.Name] = struct{}{}
			p := byName[ref.Name]
			if p == nil {
				return fmt.Errorf("config: model %q references unknown provider %q", m.Name, ref.Name)
			}
			if len(ref.Protocols) == 0 {
				return fmt.Errorf("config: model %q provider %q protocols is required", m.Name, ref.Name)
			}
			seenProto := make(map[string]struct{}, len(ref.Protocols))
			for k, protocol := range ref.Protocols {
				if !isKnownProtocol(protocol) {
					return fmt.Errorf("config: model %q provider %q protocols[%d] %q is invalid", m.Name, ref.Name, k, protocol)
				}
				if _, dup := seenProto[protocol]; dup {
					return fmt.Errorf("config: model %q provider %q has duplicate protocol %q", m.Name, ref.Name, protocol)
				}
				seenProto[protocol] = struct{}{}
				if p.endpoint(protocol) == nil {
					return fmt.Errorf("config: model %q provider %q protocols lists %s, but provider has no %s block", m.Name, ref.Name, protocol, protocol)
				}
			}
		}
	}
	if strings.TrimSpace(c.MySQL.DSN) == "" {
		return fmt.Errorf("config: mysql.dsn is required")
	}
	if _, err := normalizeMySQLDSN(c.MySQL.DSN); err != nil {
		return fmt.Errorf("config: mysql.dsn is invalid: %w", err)
	}
	if c.DetailRetain.set && c.DetailRetain.d < 0 {
		return fmt.Errorf("config: detail_retain must be >= 0")
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

func (p *Provider) validate() error {
	if p.OpenAICompletions == nil && p.OpenAIResponses == nil && p.OpenAIEmbeddings == nil && p.AnthropicMessages == nil && p.BailianMultimodalEmbedding == nil {
		return fmt.Errorf("config: provider %q must set openai_completions, openai_responses, openai_embeddings, anthropic_messages, or bailian_multimodal_embedding", p.Name)
	}
	for _, item := range []struct {
		protocol string
		ep       *ProviderEndpoint
	}{
		{protocolOpenAICompletions, p.OpenAICompletions},
		{protocolOpenAIResponses, p.OpenAIResponses},
		{protocolOpenAIEmbeddings, p.OpenAIEmbeddings},
		{protocolAnthropicMessages, p.AnthropicMessages},
		{protocolBailianMultimodalEmbedding, p.BailianMultimodalEmbedding},
	} {
		if err := validateEndpointURL(p.Name, item.protocol, item.ep); err != nil {
			return err
		}
	}
	return nil
}

func validateEndpointURL(provider, protocol string, ep *ProviderEndpoint) error {
	if ep == nil {
		return nil
	}
	return validateHTTPURL(provider, protocol, ep.URL)
}

func validateHTTPURL(provider, protocol, raw string) error {
	what := "url"
	if protocol != "" {
		what = protocol + " url"
	}
	if raw == "" {
		return fmt.Errorf("config: provider %q %s is required", provider, what)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("config: provider %q %s is invalid: %w", provider, what, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("config: provider %q %s must be http or https with a host", provider, what)
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

func (c *Config) provider(name string) *Provider {
	if c == nil {
		return nil
	}
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i]
		}
	}
	return nil
}

func (c *Config) providers(model string) []Provider {
	m := c.model(model)
	if m == nil {
		return nil
	}
	out := make([]Provider, 0, len(m.Providers))
	for _, ref := range m.Providers {
		p := c.provider(ref.Name)
		if p == nil {
			continue
		}
		out = append(out, p.bind(ref))
	}
	return out
}

func (c *Config) providersFor(model, protocol string) []Provider {
	all := c.providers(model)
	out := make([]Provider, 0, len(all))
	for _, p := range all {
		if p.endpoint(protocol) != nil {
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
