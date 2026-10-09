package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"bob/internal/logging"
)

// Config is read from a YAML file; unknown keys are rejected. String values of providers.*.apiKey and proxies may reference
// environment variables as ${NAME}, so secrets can come from the environment.
type Config struct {
	Server ServerConfig `yaml:"server"`
	// AIGate enables /aigate when it lists models.
	AIGate AIGateConfig `yaml:"aigate"`
	// Search enables /search when at least one engine has credentials.
	Search SearchConfig `yaml:"search"`
	// K8s enables /k8s when prometheus is set.
	K8s K8sConfig `yaml:"k8s"`
	// Proxies maps a route name to an upstream base URL: /<name>/... is forwarded to <url>/...
	Proxies map[string]string `yaml:"proxies,omitempty"`
}

type ServerConfig struct {
	Port     int    `yaml:"port"`
	LogLevel string `yaml:"logLevel"`
}

type AIGateConfig struct {
	ValidateModelsOnStartup *bool                     `yaml:"validateModelsOnStartup,omitempty"`
	Models                  []string                  `yaml:"models,omitempty"`
	Providers               map[string]ProviderConfig `yaml:"providers,omitempty"`
	Fallback                FallbackConfig            `yaml:"fallback"`
}

// Enabled reports whether /aigate should be served.
func (c AIGateConfig) Enabled() bool {
	return len(c.Models) > 0
}

// K8sConfig selects what /k8s/metrics reports: nodes of one node pool and pods and one PVC of
// one namespace, read from a Prometheus-compatible query API.
type K8sConfig struct {
	Prometheus string `yaml:"prometheus"`
	Nodepool   string `yaml:"nodepool"`
	Namespace  string `yaml:"namespace"`
	PVC        string `yaml:"pvc"`
}

// Enabled reports whether /k8s should be served.
func (c K8sConfig) Enabled() bool {
	return c.Prometheus != ""
}

// SearchConfig holds search API credentials; an engine is used only when its credentials are set.
type SearchConfig struct {
	KakaoAPIKey       string `yaml:"kakaoAPIKey"`
	NaverClientID     string `yaml:"naverClientID"`
	NaverClientSecret string `yaml:"naverClientSecret"`
	GoogleAPIKey      string `yaml:"googleAPIKey"`
	GoogleCX          string `yaml:"googleCX"`
}

// Enabled reports whether /search should be served.
func (c SearchConfig) Enabled() bool {
	return c.KakaoAPIKey != "" || (c.NaverClientID != "" && c.NaverClientSecret != "") || (c.GoogleAPIKey != "" && c.GoogleCX != "")
}

type ProviderConfig struct {
	APIKey string `yaml:"apiKey"`
}

type FallbackConfig struct {
	Rounds            int           `yaml:"rounds"`
	PerAttemptTimeout time.Duration `yaml:"perAttemptTimeout"`
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${NAME} with the environment value and fails on unset variables.
func expandEnv(field, value string) (string, error) {
	var missing []string
	out := envRef.ReplaceAllStringFunc(value, func(ref string) string {
		name := envRef.FindStringSubmatch(ref)[1]
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("config %s: environment variable %s is not set", field, strings.Join(missing, ", "))
	}
	return out, nil
}

// routeName is a single path segment. Reserved names are served by bob itself.
var routeName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

var reservedRoutes = []string{"aigate", "healthz", "k8s", "search"}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %q: %w", path, err)
	}

	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config file %q: %w", path, err)
	}

	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	if cfg.Server.Port < 1 || cfg.Server.Port > 65535 {
		return nil, fmt.Errorf("config server.port must be between 1 and 65535")
	}
	if cfg.Server.LogLevel == "" {
		cfg.Server.LogLevel = "info"
	}
	switch normalized := logging.Normalize(cfg.Server.LogLevel); normalized {
	case "debug", "info", "error":
		cfg.Server.LogLevel = normalized
	default:
		return nil, fmt.Errorf("config server.logLevel must be one of: debug, info, error")
	}

	if err := normalizeAIGate(&cfg.AIGate); err != nil {
		return nil, err
	}

	for field, value := range map[string]*string{
		"search.kakaoAPIKey":       &cfg.Search.KakaoAPIKey,
		"search.naverClientID":     &cfg.Search.NaverClientID,
		"search.naverClientSecret": &cfg.Search.NaverClientSecret,
		"search.googleAPIKey":      &cfg.Search.GoogleAPIKey,
		"search.googleCX":          &cfg.Search.GoogleCX,
	} {
		expanded, err := expandEnv(field, *value)
		if err != nil {
			return nil, err
		}
		*value = expanded
	}

	if cfg.K8s.Enabled() {
		u, err := url.Parse(cfg.K8s.Prometheus)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("config k8s.prometheus must be an http(s) URL, got %q", cfg.K8s.Prometheus)
		}
		if cfg.K8s.Nodepool == "" || cfg.K8s.Namespace == "" || cfg.K8s.PVC == "" {
			return nil, fmt.Errorf("config k8s requires nodepool, namespace and pvc")
		}
	}

	for name, target := range cfg.Proxies {
		target, err := expandEnv("proxies."+name, target)
		if err != nil {
			return nil, err
		}
		cfg.Proxies[name] = target
		if !routeName.MatchString(name) || slices.Contains(reservedRoutes, name) {
			return nil, fmt.Errorf("config proxies: invalid route name %q", name)
		}
		u, err := url.Parse(target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("config proxies.%s: upstream must be an http(s) URL, got %q", name, target)
		}
	}

	if !cfg.AIGate.Enabled() && !cfg.Search.Enabled() && !cfg.K8s.Enabled() && len(cfg.Proxies) == 0 {
		return nil, fmt.Errorf("config must enable aigate, search or k8s, or define at least one proxy")
	}
	return &cfg, nil
}

func normalizeAIGate(c *AIGateConfig) error {
	if !c.Enabled() {
		return nil
	}
	if len(c.Providers) == 0 {
		return fmt.Errorf("config aigate must define at least one provider")
	}
	if c.ValidateModelsOnStartup == nil {
		defaultValue := true
		c.ValidateModelsOnStartup = &defaultValue
	}
	if c.Fallback.Rounds == 0 {
		c.Fallback.Rounds = 2
	}
	if c.Fallback.Rounds < 1 {
		return fmt.Errorf("config aigate.fallback.rounds must be >= 1")
	}
	if c.Fallback.PerAttemptTimeout == 0 {
		c.Fallback.PerAttemptTimeout = 30 * time.Second
	}
	if c.Fallback.PerAttemptTimeout < 0 {
		return fmt.Errorf("config aigate.fallback.perAttemptTimeout must be >= 0")
	}
	for providerName, provider := range c.Providers {
		if providerName == "" {
			return fmt.Errorf("provider names cannot be empty")
		}
		key, err := expandEnv("aigate.providers."+providerName+".apiKey", provider.APIKey)
		if err != nil {
			return err
		}
		provider.APIKey = key
		c.Providers[providerName] = provider
	}
	return nil
}

// RedactedYAML renders the config for the startup log with API keys hidden.
func RedactedYAML(cfg *Config) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("config is nil")
	}

	redacted := *cfg
	redacted.AIGate.Providers = make(map[string]ProviderConfig, len(cfg.AIGate.Providers))
	for name, provider := range cfg.AIGate.Providers {
		copyProvider := provider
		if strings.TrimSpace(copyProvider.APIKey) != "" {
			copyProvider.APIKey = "[redacted]"
		}
		redacted.AIGate.Providers[name] = copyProvider
	}
	for _, secret := range []*string{&redacted.Search.KakaoAPIKey, &redacted.Search.NaverClientSecret, &redacted.Search.GoogleAPIKey} {
		if strings.TrimSpace(*secret) != "" {
			*secret = "[redacted]"
		}
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	err := enc.Encode(&redacted)
	_ = enc.Close()
	if err != nil {
		return "", fmt.Errorf("marshal redacted config: %w", err)
	}
	return buf.String(), nil
}
