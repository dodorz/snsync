// Package config loads and persists snsync configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Public, community-maintained Simperium credentials for the Simplenote app.
// These are the ones shipped by sncli and are the only credentials that were
// explicitly shared for third-party use. Please be polite: cache the token,
// sync incrementally and do not poll aggressively.
const (
	DefaultAppID  = "chalk-bump-f49"
	DefaultAppKey = "26864ab5d6fd4a37b80343439f107350"
)

// DefaultWatchInterval is the poll interval used by `watch` when neither the
// --interval flag nor the watch_interval config key is set.
const DefaultWatchInterval = 60 * time.Second

// Interval is a duration that accepts either a Go duration string ("90s",
// "5m") or a plain number of seconds in JSON. It marshals back as a string.
type Interval time.Duration

// UnmarshalJSON accepts both "90s" and 90 (seconds).
func (iv *Interval) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*iv = 0
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		d, err := time.ParseDuration(str)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", str, err)
		}
		*iv = Interval(d)
		return nil
	}
	var seconds float64
	if err := json.Unmarshal(b, &seconds); err != nil {
		return fmt.Errorf("duration must be a string like \"90s\" or a number of seconds: %w", err)
	}
	*iv = Interval(time.Duration(seconds * float64(time.Second)))
	return nil
}

// MarshalJSON writes the interval back as a Go duration string.
func (iv Interval) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(iv).String())
}

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

	// WatchInterval is the default `watch` poll interval. It accepts a Go
	// duration string such as "90s" or "5m", or a number of seconds.
	WatchInterval Interval `json:"watch_interval,omitempty"`

	path    string
	profile string // non-empty when the config came from a profiles section
}

// Profile is one named configuration section in a config file.
type Profile struct {
	Name   string
	Config *Config
}

// LoadProfiles reads every profile from a config file and applies defaults and
// environment overrides to each.
//
// A file may either contain a top-level "profiles" object (the new format) or
// the legacy single-profile fields at the top level. A legacy file yields one
// profile named "default".
func LoadProfiles(path string) ([]*Profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if raw, ok := top["profiles"]; ok {
		var sections map[string]*Config
		if err := json.Unmarshal(raw, &sections); err != nil {
			return nil, fmt.Errorf("parse config %s profiles: %w", path, err)
		}
		if len(sections) == 0 {
			return nil, fmt.Errorf("config %s: no profiles defined", path)
		}
		names := make([]string, 0, len(sections))
		for name := range sections {
			names = append(names, name)
		}
		sort.Strings(names)
		out := make([]*Profile, 0, len(names))
		for _, name := range names {
			c := sections[name]
			if c == nil {
				c = &Config{}
			}
			c.path = path
			c.profile = name
			c.applyDefaults()
			c.applyEnv()
			out = append(out, &Profile{Name: name, Config: c})
		}
		return out, nil
	}

	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	c.path = path
	c.applyDefaults()
	c.applyEnv()
	return []*Profile{{Name: "default", Config: &c}}, nil
}

// LoadProfile loads one named profile.
func LoadProfile(path, name string) (*Profile, error) {
	profiles, err := LoadProfiles(path)
	if err != nil {
		return nil, err
	}
	for _, p := range profiles {
		if p.Name == name {
			return p, nil
		}
	}
	return nil, fmt.Errorf("config %s: no profile %q", path, name)
}

// Load reads a config file as a single profile. It errors when the file defines
// more than one profile; use LoadProfiles or LoadProfile for those.
func Load(path string) (*Config, error) {
	profiles, err := LoadProfiles(path)
	if err != nil {
		return nil, err
	}
	if len(profiles) == 1 {
		return profiles[0].Config, nil
	}
	for _, p := range profiles {
		if p.Name == "default" {
			return p.Config, nil
		}
	}
	return nil, fmt.Errorf("config %s defines %d profiles; choose one with --profile", path, len(profiles))
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
		c.TrashDir = filepath.Join(c.LocalDir, ".trash")
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

// WatchEvery returns the configured `watch` poll interval, falling back to
// DefaultWatchInterval when watch_interval is unset.
func (c *Config) WatchEvery() (time.Duration, error) {
	d := time.Duration(c.WatchInterval)
	if d == 0 {
		return DefaultWatchInterval, nil
	}
	if d < 0 {
		return 0, fmt.Errorf("config: watch_interval must be positive, got %s", d)
	}
	return d, nil
}

// Validate checks the fields required to run.
func (c *Config) Validate() error {
	if c.Email == "" {
		return fmt.Errorf("config: email is required")
	}
	if c.LocalDir == "" {
		return fmt.Errorf("config: local_dir is required")
	}
	if _, err := c.WatchEvery(); err != nil {
		return err
	}
	return nil
}

// Path returns the file the config was loaded from.
func (c *Config) Path() string { return c.path }

// Save writes the config back atomically, preserving the cached token. In a
// multi-profile file only this section is replaced; the others are kept.
func (c *Config) Save() error {
	if c.path == "" {
		return fmt.Errorf("config: no path to save to")
	}
	if c.profile == "" {
		b, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			return err
		}
		return writeAtomic(c.path, append(b, '\n'))
	}

	b, err := os.ReadFile(c.path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return fmt.Errorf("parse config %s: %w", c.path, err)
	}
	sections := map[string]json.RawMessage{}
	if raw, ok := top["profiles"]; ok {
		if err := json.Unmarshal(raw, &sections); err != nil {
			return fmt.Errorf("parse config %s profiles: %w", c.path, err)
		}
	}
	body, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	sections[c.profile] = body
	pb, err := json.MarshalIndent(sections, "", "  ")
	if err != nil {
		return err
	}
	top["profiles"] = pb
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(c.path, append(out, '\n'))
}

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
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
