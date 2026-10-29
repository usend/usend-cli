package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	usend "usend.et/sdk/go"
)

const defaultProfile = "default"

// Config is the saved file: os.UserConfigDir()/usend/config.json, mode 0600.
type Config struct {
	CurrentProfile string              `json:"current_profile,omitempty"`
	Profiles       map[string]*Profile `json:"profiles"`
}

// Profile is one saved API key.
type Profile struct {
	APIKey     string `json:"api_key"`
	BaseURL    string `json:"base_url,omitempty"`
	Permission string `json:"permission,omitempty"`
}

func (a *app) configPath() (string, error) {
	dir, err := a.env.ConfigDir()
	if err != nil {
		return "", fmt.Errorf("find the config directory: %w", err)
	}
	return filepath.Join(dir, "usend", "config.json"), nil
}

// loadConfig reads the config file; a missing file is an empty config.
func (a *app) loadConfig() (*Config, string, error) {
	path, err := a.configPath()
	if err != nil {
		return nil, "", err
	}
	cfg := &Config{Profiles: map[string]*Profile{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, path, nil
	}
	if err != nil {
		return nil, path, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, path, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]*Profile{}
	}
	return cfg, path, nil
}

// saveConfig writes the config atomically with mode 0600 (directory 0700).
func saveConfig(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return os.Chmod(path, 0o600)
}

// credentials is the key and API the invocation uses, and where they came from.
type credentials struct {
	APIKey     string
	KeySource  string // "USEND_API_KEY" or "profile <name>" or ""
	Profile    string // the profile consulted
	BaseURL    string
	URLSource  string
	ConfigPath string
	Permission string
}

// credentials resolves the API key and base URL.
//
// Key: an explicit --profile wins, then USEND_API_KEY, then the current profile.
// Base URL: --base-url, then USEND_BASE_URL, then the profile's, then the default.
func (a *app) credentials() (*credentials, error) {
	if a.creds != nil {
		return a.creds, nil
	}
	cfg, path, err := a.loadConfig()
	if err != nil {
		return nil, err
	}
	c := &credentials{ConfigPath: path}
	name := a.profile
	if name == "" {
		name = cfg.CurrentProfile
	}
	if name == "" {
		name = defaultProfile
	}
	c.Profile = name
	p := cfg.Profiles[name]

	envKey := strings.TrimSpace(a.env.Getenv("USEND_API_KEY"))
	switch {
	case a.profile != "":
		if p == nil || p.APIKey == "" {
			return nil, fmt.Errorf("no saved profile %q: run `usend login --profile %s`", a.profile, a.profile)
		}
		c.APIKey, c.KeySource, c.Permission = p.APIKey, "profile "+name, p.Permission
	case envKey != "":
		c.APIKey, c.KeySource = envKey, "USEND_API_KEY"
	case p != nil && p.APIKey != "":
		c.APIKey, c.KeySource, c.Permission = p.APIKey, "profile "+name, p.Permission
	}

	envURL := strings.TrimSpace(a.env.Getenv("USEND_BASE_URL"))
	switch {
	case a.baseURL != "":
		c.BaseURL, c.URLSource = a.baseURL, "--base-url"
	case envURL != "":
		c.BaseURL, c.URLSource = envURL, "USEND_BASE_URL"
	case p != nil && p.BaseURL != "" && c.KeySource != "USEND_API_KEY":
		c.BaseURL, c.URLSource = p.BaseURL, "profile "+name
	default:
		c.BaseURL, c.URLSource = usend.DefaultBaseURL, "default"
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	a.creds = c
	return c, nil
}

// maskKey shows enough of a key to recognise it and never the secret part.
func maskKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 12 {
		if len(k) > 3 {
			return k[:3] + "****"
		}
		return "****"
	}
	return k[:6] + "..." + k[len(k)-4:]
}
