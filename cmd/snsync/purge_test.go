package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dodorz/snsync/internal/store"
)

func TestPurgeTrashOnlyDeletesRecent(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, ".snsync", "state.json")
	cfgPath := filepath.Join(dir, "config.json")

	var mu sync.Mutex
	requested := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.NotFound(w, r)
			return
		}
		key := path.Base(r.URL.Path)
		mu.Lock()
		requested[key] = true
		mu.Unlock()
		if key == "gone" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	now := float64(time.Now().Unix())
	recent := now - 3600
	old := now - 72*3600

	st, err := store.LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	st.Notes = map[string]*store.NoteState{
		"recent": {ID: "recent", ServerKey: "recent", Filename: "recent.md", BaseHash: "h-live", Deleted: true, ModTime: recent},
		"gone":   {ID: "gone", ServerKey: "gone", Filename: "gone.md", BaseHash: "h-live", Deleted: true, ModTime: recent},
		"orphan": {ID: "orphan", ServerKey: "orphan", Filename: "orphan.md", BaseHash: "h-unique", Deleted: true, ModTime: recent},
		"old":    {ID: "old", ServerKey: "old", Filename: "old.md", BaseHash: "h-old", Deleted: true, ModTime: old},
		"live":   {ID: "live", ServerKey: "live", Filename: "live.md", BaseHash: "h-live"},
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	cfg, _ := json.Marshal(map[string]any{
		"email":     "test@example.com",
		"local_dir": dir,
		"token":     "tok",
		"api_host":  srv.URL,
	})
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		t.Fatal(err)
	}

	// A dry run must not send any DELETE.
	if err := runPurgeTrash(cfgPath, 48*time.Hour, false, 2, false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(requested) != 0 {
		t.Fatalf("dry run deleted %v", requested)
	}
	mu.Unlock()

	if err := runPurgeTrash(cfgPath, 48*time.Hour, true, 2, false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	for _, key := range []string{"recent", "gone"} {
		if !requested[key] {
			t.Errorf("expected %s to be purged", key)
		}
	}
	for _, key := range []string{"orphan", "old", "live"} {
		if requested[key] {
			t.Errorf("%s should not have been deleted", key)
		}
	}
	mu.Unlock()

	got, err := store.LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Notes["recent"]; ok {
		t.Error("recent entry was not removed from the state file")
	}
	if _, ok := got.Notes["gone"]; ok {
		t.Error("already-missing note was not removed from the state file")
	}
	if _, ok := got.Notes["orphan"]; !ok {
		t.Error("orphan entry should be kept by default")
	}
	if _, ok := got.Notes["old"]; !ok {
		t.Error("older trash entry was removed from the state file")
	}
	if _, ok := got.Notes["live"]; !ok {
		t.Error("live note was removed from the state file")
	}

	// The explicit override may remove content that exists nowhere else.
	if err := runPurgeTrash(cfgPath, 48*time.Hour, true, 2, true); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if !requested["orphan"] {
		t.Error("--purge-orphans did not delete the orphan")
	}
	mu.Unlock()
	got, err = store.LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Notes["orphan"]; ok {
		t.Error("orphan entry was not removed from the state file")
	}
}
