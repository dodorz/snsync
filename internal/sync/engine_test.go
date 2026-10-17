package sync

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dodorz/snsync/internal/config"
	"github.com/dodorz/snsync/internal/simperium"
	"github.com/dodorz/snsync/internal/store"
)

// fakeServer is a tiny in-memory stand-in for the Simperium API.
type fakeServer struct {
	mu     sync.Mutex
	notes  map[string]simperium.Note
	vers   map[string]int
	cv     int
	pushes int
}

func newFakeServer() *fakeServer {
	return &fakeServer{notes: map[string]simperium.Note{}, vers: map[string]int{}}
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path

	switch {
	case strings.HasSuffix(path, "/authorize/"):
		writeJSON(w, map[string]string{"access_token": "tok"})
	case strings.HasSuffix(path, "/note/index"):
		type item struct {
			ID string          `json:"id"`
			V  int             `json:"v"`
			D  json.RawMessage `json:"d"`
		}
		resp := struct {
			Current string `json:"current"`
			Count   int    `json:"count"`
			Index   []item `json:"index"`
		}{Current: "cv-test", Count: len(f.notes)}
		for id, n := range f.notes {
			d, _ := json.Marshal(n)
			resp.Index = append(resp.Index, item{ID: id, V: f.vers[id], D: d})
		}
		writeJSON(w, resp)
	case strings.Contains(path, "/note/i/"):
		id := path[strings.LastIndex(path, "/i/")+3:]
		switch r.Method {
		case http.MethodPost:
			var n simperium.Note
			_ = json.NewDecoder(r.Body).Decode(&n)
			if n.CreationDate == 0 {
				n.CreationDate = 1
			}
			n.ModificationDate = 2
			f.notes[id] = n
			f.vers[id]++
			f.cv++
			f.pushes++
			writeJSON(w, n)
		case http.MethodDelete:
			delete(f.notes, id)
			delete(f.vers, id)
			w.WriteHeader(http.StatusOK)
		}
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type harness struct {
	eng     *Engine
	cfg     *config.Config
	st      *store.State
	srv     *fakeServer
	dir     string
	srvHTTP *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	server := newFakeServer()
	srv := httptest.NewServer(server)
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		Email:     "test@example.com",
		LocalDir:  dir,
		StateFile: filepath.Join(dir, ".snsync", "state.json"),
		TrashDir:  filepath.Join(dir, ".snsync", "trash"),
	}
	st, err := store.LoadState(cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	sn := simperium.New("chalk-bump-f49", "key", "tok")
	sn.AuthHost = srv.URL
	sn.APIHost = srv.URL

	eng := New(cfg, sn, st)
	eng.Log = t.Logf
	return &harness{
		eng: eng, cfg: cfg, st: st, srv: server, dir: dir, srvHTTP: srv,
	}
}

func (h *harness) run(t *testing.T) {
	t.Helper()
	if err := h.eng.Run(false); err != nil {
		t.Fatalf("sync: %v", err)
	}
}

func (h *harness) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func (h *harness) noteByContent(sub string) (string, simperium.Note) {
	for id, n := range h.srv.notes {
		if strings.Contains(n.Content, sub) {
			return id, n
		}
	}
	return "", simperium.Note{}
}

func (h *harness) stateByServerKey(key string) *store.NoteState {
	for _, s := range h.st.Notes {
		if s.ServerKey == key {
			return s
		}
	}
	return nil
}

func TestSecondSyncIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.write(t, "hello.md", "hello world")
	h.run(t)
	if h.srv.pushes != 1 {
		t.Fatalf("expected 1 push, got %d", h.srv.pushes)
	}
	h.run(t)
	if h.srv.pushes != 1 {
		t.Fatalf("second sync should not push, got %d pushes", h.srv.pushes)
	}
	if len(h.srv.notes) != 1 {
		t.Fatalf("expected 1 server note, got %d", len(h.srv.notes))
	}
}

func TestManualRenameDoesNotDuplicate(t *testing.T) {
	h := newHarness(t)
	h.write(t, "orig.md", "rename me")
	h.run(t)
	id, _ := h.noteByContent("rename me")
	old := h.stateByServerKey(id).Filename

	if err := os.Rename(filepath.Join(h.dir, old), filepath.Join(h.dir, "custom.md")); err != nil {
		t.Fatal(err)
	}
	h.run(t)

	if len(h.srv.notes) != 1 {
		t.Fatalf("rename created a duplicate note: %d notes", len(h.srv.notes))
	}
	if _, n := h.noteByContent("rename me"); n.Content == "" {
		t.Fatal("content lost across rename")
	}
}

func TestPushNewLocalFile(t *testing.T) {
	h := newHarness(t)
	h.write(t, "hello.md", "hello world")
	h.run(t)

	id, n := h.noteByContent("hello world")
	if id == "" {
		t.Fatal("note was not pushed to the server")
	}
	if n.Deleted {
		t.Fatal("pushed note should not be deleted")
	}
	if _, err := os.Stat(filepath.Join(h.dir, "hello world.md")); err != nil {
		t.Fatalf("file was not renamed to its title: %v", err)
	}
	st := h.st.Notes
	if len(st) != 1 {
		t.Fatalf("expected 1 state entry, got %d", len(st))
	}
}

func TestPullNewRemoteNote(t *testing.T) {
	h := newHarness(t)
	h.srv.notes["abc"] = simperium.Note{
		Content: "from the server", Tags: []string{"work"},
		SystemTags: []string{"pinned"}, ModificationDate: 5,
	}
	h.srv.vers["abc"] = 1
	h.run(t)

	got := h.read(t, "from the server.md")
	if got != "from the server" {
		t.Fatalf("unexpected file content: %q", got)
	}
	st := h.st.Notes["abc"]
	if st == nil || st.ServerKey != "abc" {
		t.Fatalf("state not recorded for pulled note: %+v", st)
	}
	if len(st.Tags) != 1 || st.Tags[0] != "work" {
		t.Fatalf("tags not preserved: %+v", st.Tags)
	}
}

func TestLocalDeletePropagates(t *testing.T) {
	h := newHarness(t)
	h.write(t, "note.md", "delete me")
	h.run(t)
	id, _ := h.noteByContent("delete me")
	st := h.stateByServerKey(id)
	if st == nil {
		t.Fatal("state entry not found for pushed note")
	}
	fname := st.Filename

	if err := os.Remove(filepath.Join(h.dir, fname)); err != nil {
		t.Fatal(err)
	}
	h.run(t)

	n, ok := h.srv.notes[id]
	if !ok {
		t.Fatal("note disappeared from server entirely")
	}
	if !n.Deleted {
		t.Fatal("local deletion was not propagated as deleted=true")
	}
	if !h.stateByServerKey(id).Deleted {
		t.Fatal("state did not record the deletion")
	}
}

func TestRemoteDeleteTrashesLocal(t *testing.T) {
	h := newHarness(t)
	h.srv.notes["xyz"] = simperium.Note{Content: "to be trashed", ModificationDate: 5}
	h.srv.vers["xyz"] = 1
	h.run(t)
	if _, err := os.Stat(filepath.Join(h.dir, "to be trashed.md")); err != nil {
		t.Fatalf("note was not pulled: %v", err)
	}

	h.srv.notes["xyz"] = simperium.Note{Content: "to be trashed", Deleted: true, ModificationDate: 6}
	h.srv.vers["xyz"] = 2
	h.run(t)

	if _, err := os.Stat(filepath.Join(h.dir, "to be trashed.md")); !os.IsNotExist(err) {
		t.Fatal("local file should have been moved out of the notes directory")
	}
	entries, _ := os.ReadDir(h.cfg.TrashDir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 file in trash, got %d", len(entries))
	}
}

func TestConflictCreatesCopy(t *testing.T) {
	h := newHarness(t)
	h.write(t, "shared.md", "base")
	h.run(t)
	id, _ := h.noteByContent("base")
	st := h.stateByServerKey(id)
	if st == nil {
		t.Fatal("state entry not found for pushed note")
	}
	fname := st.Filename

	// Both sides change the same note.
	h.write(t, fname, "local edit")
	h.srv.notes[id] = simperium.Note{Content: "remote edit", ModificationDate: 9}
	h.srv.vers[id]++

	h.run(t)

	// The canonical note keeps the remote version.
	if got := h.read(t, "remote edit.md"); got != "remote edit" {
		t.Fatalf("canonical note should hold the remote edit, got %q", got)
	}
	// The local version survives as a separate note pushed to the server.
	if _, n := h.noteByContent("local edit"); n.Content == "" {
		t.Fatal("local edit was lost on conflict")
	}
	entries, _ := os.ReadDir(h.dir)
	var names []string
	for _, e := range entries {
		if strings.Contains(e.Name(), "conflict") {
			names = append(names, e.Name())
		}
	}
	if len(names) != 1 {
		t.Fatalf("expected one conflict copy, got %v", names)
	}
}
