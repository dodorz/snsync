// Package config loads and persists snsync configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Public, community-maintained Simperium credentials for the Simplenote app.
// These are the ones shipped by sncli and are the only credentials that were
// explicitly shared for third-party use. Please be polite: cache the token,
// sync incrementally and do not poll aggressively.
const (
	DefaultAppID  = "chalk-bump-f49"
	DefaultAppKey = "26864ab5d6fd4a37b80343439f107350"
)

// Config is the on-disk configuration file (JSON).
type Config struct {
	Email    string `json:"email"`
	Password string `json:"password,omitempty"`
	AppID    string `json:"app_id,omitempty"`
	AppKey   string `json:"app_key,omitempty"`
	// Token is cached after a successful login and persisted back to disk.
	Token string `json:"token,omitempty"`

	LocalDir  string `json:"local_dir"`
	StateFile string `json:"state_file,omitempty"`
	TrashDir  string `json:"trash_dir,omitempty"`

	// Optional host overrides, useful when a network blocks the defaults.
	AuthHost string `json:"auth_host,omitempty"`
	APIHost  string `json:"api_host,omitempty"`

	// FullSyncEvery forces a full reconciliation after this many incremental
	// syncs (0 disables periodic full syncs). Defaults to 10.
	FullSyncEvery int `json:"full_sync_every,omitempty"`

	path string
}

// Load reads a config file, applies defaults and environment overrides.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	c.path = path
	c.applyDefaults()
	c.applyEnv()
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.AppID == "" {
		c.AppID = DefaultAppID
	}
	if c.AppKey == "" {
		c.AppKey = DefaultAppKey
	}
	if c.LocalDir != "" {
		c.LocalDir = expandHome(c.LocalDir)
	}
	if c.LocalDir != "" && c.StateFile == "" {
		c.StateFile = filepath.Join(c.LocalDir, ".snsync", "state.json")
	}
	if c.LocalDir != "" && c.TrashDir == "" {
		c.TrashDir = filepath.Join(c.LocalDir, ".snsync", "trash")
	}
	if c.FullSyncEvery == 0 {
		c.FullSyncEvery = 10
	}
	if c.StateFile != "" {
		c.StateFile = expandHome(c.StateFile)
	}
	if c.TrashDir != "" {
		c.TrashDir = expandHome(c.TrashDir)
	}
}

func (c *Config) applyEnv() {
	if v := os.Getenv("SN_PASSWORD"); v != "" {
		c.Password = v
	}
	if v := os.Getenv("SN_TOKEN"); v != "" {
		c.Token = v
	}
	if v := os.Getenv("SN_EMAIL"); v != "" {
		c.Email = v
	}
	if v := os.Getenv("SN_AUTH_HOST"); v != "" {
		c.AuthHost = v
	}
	if v := os.Getenv("SN_API_HOST"); v != "" {
		c.APIHost = v
	}
}

// Validate checks the fields required to run.
func (c *Config) Validate() error {
	if c.Email == "" {
		return fmt.Errorf("config: email is required")
	}
	if c.LocalDir == "" {
		return fmt.Errorf("config: local_dir is required")
	}
	return nil
}

// Path returns the file the config was loaded from.
func (c *Config) Path() string { return c.path }

// Save writes the config back atomically, preserving the cached token.
func (c *Config) Save() error {
	if c.path == "" {
		return fmt.Errorf("config: no path to save to")
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// expandHome expands a leading "~" path component to the current user's home
// directory.
//
// A "~" only refers to the home directory when it is the first component of the
// path. This matters on Windows, where "~" is a legal directory name: a path
// such as `C:/~/Temp` (or `C:\~\Temp`) must be left untouched because its "~"
// is an ordinary directory, not a shortcut to %USERPROFILE%.
func expandHome(p string) string {
	// A drive/volume prefix means the "~" can never be the leading component,
	// so paths like `C:/~/Temp` are ordinary directories and stay as they are.
	if filepath.VolumeName(p) != "" {
		return p
	}
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	// Accept both separators so Windows users can write `~/Notes` or `~\Notes`.
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}
