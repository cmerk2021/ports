// Package config loads the global ports configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/cmerk2021/ports/internal/model"
)

// Default locations and values.
const (
	DefaultConfigPath   = "/etc/ports/config.json"
	DefaultStackRoot    = "/opt/stacks"
	DefaultRegistryPath = "/var/lib/ports/registry.json"
	DefaultBlockSize    = 5
	DefaultRangeStart   = 12000
	DefaultRangeEnd     = 29999
	DefaultDocker       = "docker"
	DefaultProcNet      = "/proc/net"
	ManifestName        = ".ports"
)

// Environment variables that override configuration values.
const (
	EnvConfig    = "PORTS_CONFIG"
	EnvStackRoot = "PORTS_STACK_ROOT"
	EnvRegistry  = "PORTS_REGISTRY"
)

// Config is the global configuration. Its JSON form is the on-disk format of
// /etc/ports/config.json.
type Config struct {
	Version       int         `json:"version"`
	StackRoot     string      `json:"stack_root"`
	RegistryPath  string      `json:"registry_path"`
	BlockSize     int         `json:"block_size"`
	DynamicRange  model.Range `json:"dynamic_range"`
	Strategy      string      `json:"strategy"`
	ComposeFiles  []string    `json:"compose_files,omitempty"`
	DockerCommand string      `json:"docker_command"`
	ProcNetDir    string      `json:"proc_net_dir"`

	// Path is the file the configuration was loaded from ("" for defaults).
	Path string `json:"-"`
}

// StrategyFirstFit is the only supported allocation strategy.
const StrategyFirstFit = "first-fit"

// Default returns the built-in configuration.
func Default() *Config {
	return &Config{
		Version:       1,
		StackRoot:     DefaultStackRoot,
		RegistryPath:  DefaultRegistryPath,
		BlockSize:     DefaultBlockSize,
		DynamicRange:  model.Range{Start: DefaultRangeStart, End: DefaultRangeEnd},
		Strategy:      StrategyFirstFit,
		DockerCommand: DefaultDocker,
		ProcNetDir:    DefaultProcNet,
	}
}

// Overrides are values supplied on the command line; empty means unset.
type Overrides struct {
	ConfigPath   string
	StackRoot    string
	RegistryPath string
}

// Load resolves the configuration: defaults, then the config file, then
// environment variables, then command-line overrides. A missing config file is
// only acceptable when the default path is used implicitly.
func Load(o Overrides) (*Config, error) {
	path := o.ConfigPath
	explicit := path != ""
	if !explicit {
		if env := os.Getenv(EnvConfig); env != "" {
			path, explicit = env, true
		} else {
			path = DefaultConfigPath
		}
	}
	cfg := Default()
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := decodeStrict(data, cfg); err != nil {
			return nil, fmt.Errorf("config %s is malformed: %w (fix or remove the file; it was not modified)", path, err)
		}
		cfg.Path = path
	case errors.Is(err, fs.ErrNotExist) && !explicit:
		// Built-in defaults.
	default:
		return nil, fmt.Errorf("cannot read config %s: %w", path, err)
	}
	if v := os.Getenv(EnvStackRoot); v != "" {
		cfg.StackRoot = v
	}
	if v := os.Getenv(EnvRegistry); v != "" {
		cfg.RegistryPath = v
	}
	if o.StackRoot != "" {
		cfg.StackRoot = o.StackRoot
	}
	if o.RegistryPath != "" {
		cfg.RegistryPath = o.RegistryPath
	}
	if err := cfg.Validate(); err != nil {
		where := "configuration"
		if cfg.Path != "" {
			where = "config " + cfg.Path
		}
		return nil, fmt.Errorf("invalid %s: %w", where, err)
	}
	cfg.StackRoot = filepath.Clean(cfg.StackRoot)
	cfg.RegistryPath = filepath.Clean(cfg.RegistryPath)
	return cfg, nil
}

func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after JSON object")
	}
	return nil
}

// Validate checks configuration values.
func (c *Config) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("unsupported config version %d (expected 1)", c.Version)
	}
	if !filepath.IsAbs(c.StackRoot) {
		return fmt.Errorf("stack_root %q must be an absolute path", c.StackRoot)
	}
	if !filepath.IsAbs(c.RegistryPath) {
		return fmt.Errorf("registry_path %q must be an absolute path", c.RegistryPath)
	}
	if err := c.DynamicRange.Validate(); err != nil {
		return fmt.Errorf("dynamic_range: %w", err)
	}
	if c.BlockSize < 1 || c.BlockSize > c.DynamicRange.Size() {
		return fmt.Errorf("block_size %d must be between 1 and the size of the dynamic range (%d)", c.BlockSize, c.DynamicRange.Size())
	}
	if c.Strategy != "" && c.Strategy != StrategyFirstFit {
		return fmt.Errorf("unsupported strategy %q (only %q is supported)", c.Strategy, StrategyFirstFit)
	}
	if c.DockerCommand == "" {
		return errors.New("docker_command must not be empty")
	}
	if c.ProcNetDir == "" {
		return errors.New("proc_net_dir must not be empty")
	}
	return nil
}
