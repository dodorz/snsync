package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodorz/snsync/internal/simperium"
	"github.com/dodorz/snsync/internal/store"
)

// purgeServer is the two Simperium endpoints purge-trash uses: the full index
// and a hard DELETE.
type purgeServer struct {
	mu        sync.Mutex
	notes     map[string]simperium.Note
	requested map[string]bool
}

func (f *purgeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if strings.HasSuffix(r.URL.Path, "/note/index") {
		type item struct {
			ID string          `json:"id"`
			V  int             `json:"v"`
			D  json.RawMessage `json:"d"`
		}
		resp := struct {
			Current string `json:"current"`
			Count   int    `json:"count"`
			Index   []item `json:"index"`
		}{Current: "cursor-1"}
		for id, n := range f.notes {
			d, _ := json.Marshal(n)
			resp.Index = append(resp.Index, item{ID: id, V: 1, D: d})
		}
		resp.Count = len(resp.Index)
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/note/i/") {
		key := path.Base(r.URL.Path)
		f.requested[key] = true
		if key == "gone" {
			delete(f.notes, key) // already hard-deleted by someone else
			http.NotFound(w, r)
			return
		}
		delete(f.notes, key)
		w.WriteHeader(http.StatusOK)
		return
	}

	http.NotFound(w, r)
}

func TestPurgeTrashOnlyDeletesRecent(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, ".snsync", "state.json")
	cfgPath := filepath.Join(dir, "config.json")

	now := time.Now()
	api := &purgeServer{
		notes: map[string]simperium.Note{
			"live":   {Content: "live content", ModificationDate: float64(now.Unix())},
			"recent": {Content: "live content", Deleted: true, ModificationDate: float64(now.Add(-time.Hour).Unix())},
			"gone":   {Content: "live content", Deleted: true, ModificationDate: float64(now.Add(-time.Hour).Unix())},
			"orphan": {Content: "unique content", Deleted: true, ModificationDate: float64(now.Add(-time.Hour).Unix())},
			"old":    {Content: "live content", Deleted: true, ModificationDate: float64(now.Add(-72 * time.Hour).Unix())},
		},
		requested: map[string]bool{},
	}
	srv := httptest.NewServer(api)
	defer srv.Close()

	st, err := store.LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	st.Notes = map[string]*store.NoteState{
		"live":   {ID: "live", ServerKey: "live", Filename: "live.md", BaseHash: "h-live"},
		"recent": {ID: "recent", ServerKey: "recent", BaseHash: "h-live", Deleted: true},
		"gone":   {ID: "gone", ServerKey: "gone", BaseHash: "h-live", Deleted: true},
		"orphan": {ID: "orphan", ServerKey: "orphan", BaseHash: "h-unique", Deleted: true},
		"old":    {ID: "old", ServerKey: "old", BaseHash: "h-live", Deleted: true},
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
	api.mu.Lock()
	if len(api.requested) != 0 {
		t.Fatalf("dry run deleted %v", api.requested)
	}
	api.mu.Unlock()

	if err := runPurgeTrash(cfgPath, 48*time.Hour, true, 2, false); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	for _, key := range []string{"recent", "gone"} {
		if !api.requested[key] {
			t.Errorf("expected %s to be purged", key)
		}
	}
	for _, key := range []string{"orphan", "old", "live"} {
		if api.requested[key] {
			t.Errorf("%s should not have been deleted", key)
		}
	}
	api.mu.Unlock()

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
	api.mu.Lock()
	if !api.requested["orphan"] {
		t.Error("--purge-orphans did not delete the orphan")
	}
	api.mu.Unlock()
	got, err = store.LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Notes["orphan"]; ok {
		t.Error("orphan entry was not removed from the state file")
	}
}
