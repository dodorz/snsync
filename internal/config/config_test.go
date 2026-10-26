package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory available")
	}
	cases := []struct {
		in   string
		want string
	}{
		{"~", home},
		{"~/Notes", filepath.Join(home, "Notes")},
		{`~\Notes`, filepath.Join(home, "Notes")},
		{"~/a/b", filepath.Join(home, "a", "b")},
		{"~/", home},
		{"relative/path", "relative/path"},
		{"", ""},
		// "~" is a legal directory name on Windows, so only a leading "~" is
		// a home reference. A drive-letter path, or a "~" further along, must
		// be left untouched.
		{"C:/Users/someone/Notes", "C:/Users/someone/Notes"},
		{"C:/~/Temp", "C:/~/Temp"},
		{`C:\~\Temp`, `C:\~\Temp`},
		{"~foo/bar", "~foo/bar"},
	}
	for _, tc := range cases {
		if got := expandHome(tc.in); got != tc.want {
			t.Errorf("expandHome(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWatchEvery(t *testing.T) {
	c := &Config{}
	if got, err := c.WatchEvery(); err != nil || got != DefaultWatchInterval {
		t.Errorf("default WatchEvery() = %v, %v; want %v, nil", got, err, DefaultWatchInterval)
	}
	c.WatchInterval = Interval(90 * time.Second)
	if got, err := c.WatchEvery(); err != nil || got != 90*time.Second {
		t.Errorf("WatchEvery(90s) = %v, %v; want 90s, nil", got, err)
	}
	c.WatchInterval = Interval(-5 * time.Second)
	if _, err := c.WatchEvery(); err == nil {
		t.Errorf("WatchEvery(-5s) = nil error, want failure")
	}
}

func TestWatchIntervalJSON(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"watch_interval":"90s"}`), &c); err != nil {
		t.Fatalf("string duration: %v", err)
	}
	if got, _ := c.WatchEvery(); got != 90*time.Second {
		t.Errorf("string watch_interval = %v, want 90s", got)
	}
	if err := json.Unmarshal([]byte(`{"watch_interval":300}`), &c); err != nil {
		t.Fatalf("numeric duration: %v", err)
	}
	if got, _ := c.WatchEvery(); got != 300*time.Second {
		t.Errorf("numeric watch_interval = %v, want 300s", got)
	}
	if err := json.Unmarshal([]byte(`{"watch_interval":"nope"}`), &c); err == nil {
		t.Errorf("bad duration = nil error, want failure")
	}
}

func TestApplyDefaultsFillsPaths(t *testing.T) {
	c := &Config{LocalDir: "/tmp/notes"}
	c.applyDefaults()
	if c.StateFile != filepath.Join("/tmp/notes", ".snsync", "state.json") {
		t.Errorf("StateFile = %q", c.StateFile)
	}
	if c.TrashDir != filepath.Join("/tmp/notes", ".trash") {
		t.Errorf("TrashDir = %q", c.TrashDir)
	}
	if c.AppID != DefaultAppID || c.AppKey != DefaultAppKey {
		t.Errorf("app credentials not defaulted: %q %q", c.AppID, c.AppKey)
	}
	if c.FullSyncEvery != 10 {
		t.Errorf("FullSyncEvery = %d, want 10", c.FullSyncEvery)
	}
}

func TestLoadProfilesLegacySingle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"email":"a@b","local_dir":"/tmp/a"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ps, err := LoadProfiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].Name != "default" {
		t.Fatalf("profiles = %+v", ps)
	}
	if ps[0].Config.Email != "a@b" || ps[0].Config.LocalDir != "/tmp/a" {
		t.Fatalf("config = %+v", ps[0].Config)
	}
}

func TestLoadProfilesSections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"profiles":{"work":{"email":"w@x","local_dir":"/tmp/w"},"home":{"email":"h@x","local_dir":"/tmp/h"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ps, err := LoadProfiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].Name != "home" || ps[1].Name != "work" {
		t.Fatalf("profiles = %+v", ps)
	}
	if ps[0].Config.Email != "h@x" || ps[1].Config.Email != "w@x" {
		t.Fatalf("configs = %+v / %+v", ps[0].Config, ps[1].Config)
	}
	if _, err := LoadProfile(path, "nope"); err == nil {
		t.Error("LoadProfile(nope) should fail")
	}
}

func TestSaveProfileKeepsOtherSections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"profiles":{"work":{"email":"w@x","local_dir":"/tmp/w"},"home":{"email":"h@x","local_dir":"/tmp/h"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	home, err := LoadProfile(path, "home")
	if err != nil {
		t.Fatal(err)
	}
	home.Config.Token = "tok"
	if err := home.Config.Save(); err != nil {
		t.Fatal(err)
	}
	ps, err := LoadProfiles(path)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*Config{}
	for _, p := range ps {
		byName[p.Name] = p.Config
	}
	if byName["home"].Token != "tok" {
		t.Errorf("home token not saved: %+v", byName["home"])
	}
	if byName["work"].Email != "w@x" || byName["work"].LocalDir != "/tmp/w" {
		t.Errorf("work section changed: %+v", byName["work"])
	}
}
