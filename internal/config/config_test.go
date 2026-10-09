package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T, yaml string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(path)
}

func TestLoadConfig_ProxiesOnly(t *testing.T) {
	cfg, err := load(t, "proxies:\n  runbox: http://runbox.runbox\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AIGate.Enabled() || cfg.Server.Port != 8080 || cfg.Server.LogLevel != "info" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadConfig_AIGateDefaults(t *testing.T) {
	cfg, err := load(t, "aigate:\n  models: [gemini/x]\n  providers:\n    gemini: {apiKey: k}\n")
	if err != nil {
		t.Fatal(err)
	}
	if !*cfg.AIGate.ValidateModelsOnStartup || cfg.AIGate.Fallback.Rounds != 2 || cfg.AIGate.Fallback.PerAttemptTimeout == 0 {
		t.Fatalf("unexpected aigate defaults: %+v", cfg.AIGate)
	}
}

func TestLoadConfig_Rejects(t *testing.T) {
	cases := map[string]string{
		"empty":            "server: {port: 8080}\n",
		"reserved route":   "proxies:\n  aigate: http://x\n",
		"reserved search":  "proxies:\n  search: http://x\n",
		"route with slash": "proxies:\n  a/b: http://x\n",
		"bad upstream":     "proxies:\n  runbox: runbox.runbox\n",
		"no providers":     "aigate:\n  models: [gemini/x]\n",
	}
	for name, yaml := range cases {
		if _, err := load(t, yaml); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestLoadConfig_RejectsUnknownKeys(t *testing.T) {
	if _, err := load(t, "proxies:\n  search: http://search\naigate:\n  fallbak: {rounds: 3}\n"); err == nil {
		t.Fatal("expected error for misspelled key")
	}
}

func TestLoadConfig_ExpandsEnv(t *testing.T) {
	t.Setenv("BOB_TEST_KEY", "from-env")
	t.Setenv("BOB_TEST_HOST", "runbox.internal")
	cfg, err := load(t, "aigate:\n  models: [gemini/x]\n  providers:\n    gemini: {apiKey: \"${BOB_TEST_KEY}\"}\nproxies:\n  runbox: http://${BOB_TEST_HOST}\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AIGate.Providers["gemini"].APIKey != "from-env" || cfg.Proxies["runbox"] != "http://runbox.internal" {
		t.Fatalf("env not expanded: %+v %+v", cfg.AIGate.Providers, cfg.Proxies)
	}
}

func TestLoadConfig_UnsetEnvIsError(t *testing.T) {
	_, err := load(t, "aigate:\n  models: [gemini/x]\n  providers:\n    gemini: {apiKey: \"${BOB_TEST_UNSET_KEY}\"}\n")
	if err == nil || !strings.Contains(err.Error(), "BOB_TEST_UNSET_KEY") {
		t.Fatalf("expected unset variable error, got %v", err)
	}
}

func TestLoadConfig_SearchEnabledByCredentials(t *testing.T) {
	t.Setenv("BOB_TEST_KAKAO", "kakao-key")
	cfg, err := load(t, "search:\n  kakaoAPIKey: \"${BOB_TEST_KAKAO}\"\n  googleAPIKey: only-key-no-cx\n")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Search.Enabled() || cfg.Search.KakaoAPIKey != "kakao-key" {
		t.Fatalf("search not enabled: %+v", cfg.Search)
	}
	out, err := RedactedYAML(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "kakao-key") || strings.Contains(out, "only-key-no-cx") {
		t.Fatalf("search keys leaked: %s", out)
	}
}

func TestRedactedYAML_HidesKeys(t *testing.T) {
	cfg, err := load(t, "aigate:\n  models: [gemini/x]\n  providers:\n    gemini: {apiKey: secret-key}\n")
	if err != nil {
		t.Fatal(err)
	}
	out, err := RedactedYAML(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "secret-key") || cfg.AIGate.Providers["gemini"].APIKey != "secret-key" {
		t.Fatalf("redaction leaked or mutated config: %s", out)
	}
}
