package config

import (
	"os"
	"path/filepath"
	"testing"
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

func TestApplyDefaultsFillsPaths(t *testing.T) {
	c := &Config{LocalDir: "/tmp/notes"}
	c.applyDefaults()
	if c.StateFile != filepath.Join("/tmp/notes", ".snsync", "state.json") {
		t.Errorf("StateFile = %q", c.StateFile)
	}
	if c.TrashDir != filepath.Join("/tmp/notes", ".snsync", "trash") {
		t.Errorf("TrashDir = %q", c.TrashDir)
	}
	if c.AppID != DefaultAppID || c.AppKey != DefaultAppKey {
		t.Errorf("app credentials not defaulted: %q %q", c.AppID, c.AppKey)
	}
	if c.FullSyncEvery != 10 {
		t.Errorf("FullSyncEvery = %d, want 10", c.FullSyncEvery)
	}
}
