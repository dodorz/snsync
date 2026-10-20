package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/dodorz/snsync/internal/lock"
)

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    options
		wantErr bool
	}{
		{
			name: "sync with flags in any order",
			args: []string{"sync", "--dry-run", "-c", "/tmp/x.json", "--full"},
			want: options{command: "sync", configPath: "/tmp/x.json", dryRun: true, full: true},
		},
		{
			name: "flags before command",
			args: []string{"-c=/a", "watch", "--interval", "5m"},
			want: options{command: "watch", configPath: "/a", interval: 5 * time.Minute, intervalSet: true},
		},
		{
			name: "version flag",
			args: []string{"--version"},
			want: options{command: "version", configPath: defaultConfigPath()},
		},
		{
			name: "purge trash",
			args: []string{"purge-trash", "--since", "48h"},
			want: options{command: "purge-trash", configPath: defaultConfigPath(), since: 48 * time.Hour, sinceSet: true, jobs: 4},
		},
		{
			name:    "purge trash without window",
			args:    []string{"purge-trash"},
			wantErr: true,
		},
		{
			name:    "purge trash with too many jobs",
			args:    []string{"purge-trash", "--since", "48h", "--jobs", "99"},
			wantErr: true,
		},
		{
			name: "login with token",
			args: []string{"login", "--token", "abc"},
			want: options{command: "login", configPath: defaultConfigPath(), token: "abc"},
		},
		{
			name:    "bad interval",
			args:    []string{"watch", "--interval", "nope"},
			wantErr: true,
		},
		{
			name:    "non-positive interval",
			args:    []string{"watch", "--interval=0s"},
			wantErr: true,
		},
		{
			name:    "unknown flag",
			args:    []string{"sync", "--bogus"},
			wantErr: true,
		},
		{
			name:    "two commands",
			args:    []string{"sync", "watch"},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if *got != tc.want {
				t.Fatalf("got %+v want %+v", *got, tc.want)
			}
		})
	}
}

func TestRunSyncSkipsWhenLocked(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	body := `{"email":"test@example.com","local_dir":` + strconv.Quote(dir) + `}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// Hold the same lock a running sync would hold.
	lk, err := lock.Acquire(filepath.Join(dir, ".snsync", "state.json.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()

	if err := runSync(cfgPath, false, false, false); !errors.Is(err, lock.ErrHeld) {
		t.Fatalf("runSync: got %v, want ErrHeld", err)
	}
}
