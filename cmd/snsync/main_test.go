package main

import (
	"testing"
	"time"
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
			want: options{command: "sync", configPath: "/tmp/x.json", dryRun: true, full: true, interval: 60 * time.Second},
		},
		{
			name: "flags before command",
			args: []string{"-c=/a", "watch", "--interval", "5m"},
			want: options{command: "watch", configPath: "/a", interval: 5 * time.Minute},
		},
		{
			name: "version flag",
			args: []string{"--version"},
			want: options{command: "version", configPath: defaultConfigPath(), interval: 60 * time.Second},
		},
		{
			name: "login with token",
			args: []string{"login", "--token", "abc"},
			want: options{command: "login", configPath: defaultConfigPath(), token: "abc", interval: 60 * time.Second},
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
