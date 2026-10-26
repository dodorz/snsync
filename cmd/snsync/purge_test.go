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
	mu         sync.Mutex
	notes      map[string]simperium.Note
	requested  map[string]bool
	indexCalls int
}

func (f *purgeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if strings.HasSuffix(r.URL.Path, "/note/index") {
		f.indexCalls++
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

func purgeHarness(t *testing.T, state map[string]*store.NoteState, notes map[string]simperium.Note) (string, *purgeServer, *store.State) {
	t.Helper()
	dir := t.TempDir()
	statePath := filepath.Join(dir, ".snsync", "state.json")
	cfgPath := filepath.Join(dir, "config.json")

	st, err := store.LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	st.Notes = state
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	api := &purgeServer{notes: notes, requested: map[string]bool{}}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	cfg, _ := json.Marshal(map[string]any{
		"email":     "test@example.com",
		"local_dir": dir,
		"token":     "tok",
		"api_host":  srv.URL,
	})
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, api, st
}

func TestPurgeTrashUsesStateTombstones(t *testing.T) {
	now := time.Now()
	state := map[string]*store.NoteState{
		"recent": {ID: "recent", ServerKey: "recent", BaseHash: "h-live", Deleted: true, ModTime: float64(now.Add(-time.Hour).Unix())},
		"gone":   {ID: "gone", ServerKey: "gone", BaseHash: "h-live", Deleted: true, ModTime: float64(now.Add(-time.Hour).Unix())},
		"orphan": {ID: "orphan", ServerKey: "orphan", BaseHash: "h-unique", Deleted: true, ModTime: float64(now.Add(-time.Hour).Unix())},
		"old":    {ID: "old", ServerKey: "old", BaseHash: "h-live", Deleted: true, ModTime: float64(now.Add(-72 * time.Hour).Unix())},
		"live":   {ID: "live", ServerKey: "live", Filename: "live.md", BaseHash: "h-live"},
	}
	cfgPath, api, _ := purgeHarness(t, state, nil)

	// Dry run first: no DELETE.
	if err := runPurgeTrashProfile(mustProfile(t, cfgPath), 48*time.Hour, false, 2, false, false); err != nil {
		t.Fatal(err)
	}
	if len(api.requested) != 0 {
		t.Fatalf("dry run deleted %v", api.requested)
	}
	if api.indexCalls != 0 {
		t.Fatalf("state tombstones should avoid the index, got %d calls", api.indexCalls)
	}

	if err := runPurgeTrashProfile(mustProfile(t, cfgPath), 48*time.Hour, true, 2, false, false); err != nil {
		t.Fatal(err)
	}
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
	got, err := store.LoadState(filepath.Join(filepath.Dir(cfgPath), ".snsync", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"recent", "gone"} {
		if _, ok := got.Notes[key]; ok {
			t.Errorf("%s entry was not removed from the state file", key)
		}
	}
	for _, key := range []string{"orphan", "old", "live"} {
		if _, ok := got.Notes[key]; !ok {
			t.Errorf("%s entry should have been kept", key)
		}
	}
}

func TestPurgeTrashFallsBackToRemoteIndex(t *testing.T) {
	now := time.Now()
	state := map[string]*store.NoteState{
		"live": {ID: "live", ServerKey: "live", Filename: "live.md", BaseHash: "h-live"},
	}
	notes := map[string]simperium.Note{
		"live":   {Content: "live content", ModificationDate: float64(now.Unix())},
		"recent": {Content: "live content", Deleted: true, ModificationDate: float64(now.Add(-time.Hour).Unix())},
		"gone":   {Content: "live content", Deleted: true, ModificationDate: float64(now.Add(-time.Hour).Unix())},
		"orphan": {Content: "unique content", Deleted: true, ModificationDate: float64(now.Add(-time.Hour).Unix())},
		"old":    {Content: "live content", Deleted: true, ModificationDate: float64(now.Add(-72 * time.Hour).Unix())},
	}
	cfgPath, api, _ := purgeHarness(t, state, notes)

	// No tombstones -> the index is consulted, and the orphan is kept.
	if err := runPurgeTrashProfile(mustProfile(t, cfgPath), 48*time.Hour, true, 2, false, false); err != nil {
		t.Fatal(err)
	}
	if api.indexCalls == 0 {
		t.Fatal("expected a remote index scan")
	}
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

	// The explicit override also deletes the orphan.
	if err := runPurgeTrashProfile(mustProfile(t, cfgPath), 48*time.Hour, true, 2, true, true); err != nil {
		t.Fatal(err)
	}
	if !api.requested["orphan"] {
		t.Error("--purge-orphans did not delete the orphan")
	}
}
