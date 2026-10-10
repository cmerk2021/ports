package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func clearEnv(t *testing.T) {
	t.Setenv(EnvConfig, "")
	t.Setenv(EnvStackRoot, "")
	t.Setenv(EnvRegistry, "")
}

func TestLoadPrecedence(t *testing.T) {
	clearEnv(t)
	p := write(t, `{"version":1,"stack_root":"/srv/stacks","block_size":10,"dynamic_range":{"start":20000,"end":20999}}`)
	cfg, err := Load(Overrides{ConfigPath: p})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StackRoot != "/srv/stacks" || cfg.BlockSize != 10 || cfg.DynamicRange.Start != 20000 || cfg.RegistryPath != DefaultRegistryPath {
		t.Fatalf("%+v", cfg)
	}
	t.Setenv(EnvStackRoot, "/env/stacks")
	cfg, _ = Load(Overrides{ConfigPath: p})
	if cfg.StackRoot != "/env/stacks" {
		t.Fatalf("env override: %s", cfg.StackRoot)
	}
	cfg, _ = Load(Overrides{ConfigPath: p, StackRoot: "/flag/stacks/"})
	if cfg.StackRoot != "/flag/stacks" {
		t.Fatalf("flag override: %s", cfg.StackRoot)
	}
}

func TestLoadErrors(t *testing.T) {
	clearEnv(t)
	cases := map[string]string{
		"malformed":      `{"version":1,`,
		"unknown field":  `{"version":1,"blocksize":5}`,
		"bad version":    `{"version":2}`,
		"relative root":  `{"version":1,"stack_root":"stacks"}`,
		"bad range":      `{"version":1,"dynamic_range":{"start":30000,"end":20000}}`,
		"range overflow": `{"version":1,"dynamic_range":{"start":60000,"end":70000}}`,
		"block too big":  `{"version":1,"block_size":11,"dynamic_range":{"start":20000,"end":20009}}`,
		"strategy":       `{"version":1,"strategy":"random"}`,
	}
	for name, body := range cases {
		p := write(t, body)
		if _, err := Load(Overrides{ConfigPath: p}); err == nil {
			t.Errorf("%s: expected error", name)
		}
		if b, _ := os.ReadFile(p); string(b) != body {
			t.Errorf("%s: config modified", name)
		}
	}
	_, err := Load(Overrides{ConfigPath: filepath.Join(t.TempDir(), "missing.json")})
	if err == nil || !strings.Contains(err.Error(), "cannot read config") {
		t.Fatalf("explicit missing config: %v", err)
	}
}
