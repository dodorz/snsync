package sync

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/dodorz/snsync/internal/config"
	"github.com/dodorz/snsync/internal/simperium"
	"github.com/dodorz/snsync/internal/store"
)

// fakeServer is a tiny in-memory stand-in for the Simperium API.
type fakeServer struct {
	mu       sync.Mutex
	notes    map[string]simperium.Note
	vers     map[string]int
	changeCV map[string]int
	cv       int
	pushes   int
}

func newFakeServer() *fakeServer {
	return &fakeServer{
		notes:    map[string]simperium.Note{},
		vers:     map[string]int{},
		changeCV: map[string]int{},
	}
}

// put stores a note and bumps the change cursor, as a real server change would.
func (f *fakeServer) put(id string, n simperium.Note) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cv++
	f.notes[id] = n
	f.vers[id]++
	f.changeCV[id] = f.cv
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path

	switch {
	case strings.HasSuffix(path, "/authorize/"):
		writeJSON(w, map[string]string{"access_token": "tok"})
	case strings.HasSuffix(path, "/note/index"):
		sinceRaw := r.URL.Query().Get("since")
		since := 0
		if sinceRaw != "" {
			since, _ = strconv.Atoi(sinceRaw)
		}
		type item struct {
			ID string          `json:"id"`
			V  int             `json:"v"`
			D  json.RawMessage `json:"d"`
		}
		resp := struct {
			Current string `json:"current"`
			Count   int    `json:"count"`
			Index   []item `json:"index"`
		}{Current: strconv.Itoa(f.cv)}
		for id, n := range f.notes {
			if sinceRaw != "" && f.changeCV[id] <= since {
				continue
			}
			d, _ := json.Marshal(n)
			resp.Index = append(resp.Index, item{ID: id, V: f.vers[id], D: d})
		}
		resp.Count = len(resp.Index)
		writeJSON(w, resp)
	case strings.Contains(path, "/note/i/"):
		id := path[strings.LastIndex(path, "/i/")+3:]
		switch r.Method {
		case http.MethodGet:
			n, ok := f.notes[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, n)
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
			f.changeCV[id] = f.cv
			f.pushes++
			writeJSON(w, n)
		case http.MethodDelete:
			delete(f.notes, id)
			delete(f.vers, id)
			delete(f.changeCV, id)
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
		TrashDir:  filepath.Join(dir, ".trash"),
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

func (h *harness) runFull(t *testing.T) {
	t.Helper()
	h.eng.Full = true
	defer func() { h.eng.Full = false }()
	if err := h.eng.Run(false); err != nil {
		t.Fatalf("full sync: %v", err)
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
	h.srv.put("abc", simperium.Note{
		Content: "from the server", Tags: []string{"work"},
		SystemTags: []string{"pinned"},
	})
	h.run(t)

	got := h.read(t, "from the server.md")
	want := "---\r\ntags:\r\n  - work\r\n---\r\nfrom the server"
	if got != want {
		t.Fatalf("unexpected file content: %q, want %q", got, want)
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
	h.srv.put("xyz", simperium.Note{Content: "to be trashed"})
	h.run(t)
	if _, err := os.Stat(filepath.Join(h.dir, "to be trashed.md")); err != nil {
		t.Fatalf("note was not pulled: %v", err)
	}

	h.srv.put("xyz", simperium.Note{Content: "to be trashed", Deleted: true})
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
	h.srv.put(id, simperium.Note{Content: "remote edit"})
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

func TestIncrementalPullDoesNotDeleteUntouchedNotes(t *testing.T) {
	h := newHarness(t)
	h.srv.put("a", simperium.Note{Content: "alpha"})
	h.run(t)
	if _, err := os.Stat(filepath.Join(h.dir, "alpha.md")); err != nil {
		t.Fatalf("alpha not pulled: %v", err)
	}
	if h.st.IncrementalSinceFull != 0 {
		t.Fatalf("a full sync should not bump the incremental counter, got %d", h.st.IncrementalSinceFull)
	}

	h.srv.put("b", simperium.Note{Content: "beta"})
	h.run(t)

	if _, err := os.Stat(filepath.Join(h.dir, "beta.md")); err != nil {
		t.Fatalf("beta not pulled incrementally: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "alpha.md")); err != nil {
		t.Fatalf("untouched note was deleted during incremental sync: %v", err)
	}
	if h.st.IncrementalSinceFull != 1 {
		t.Fatalf("expected 1 incremental sync, got %d", h.st.IncrementalSinceFull)
	}
}

func TestIncrementalLocalDeleteKeepsRemoteContent(t *testing.T) {
	h := newHarness(t)
	h.srv.put("a", simperium.Note{Content: "keep my body"})
	h.run(t)
	st := h.stateByServerKey("a")
	if st == nil {
		t.Fatal("pull failed")
	}
	if err := os.Remove(filepath.Join(h.dir, st.Filename)); err != nil {
		t.Fatal(err)
	}
	h.run(t)

	n := h.srv.notes["a"]
	if !n.Deleted {
		t.Fatal("local deletion not propagated incrementally")
	}
	if n.Content != "keep my body" {
		t.Fatalf("trashing wiped remote content: %q", n.Content)
	}
}

func TestPurgedNoteOnlyDetectedOnFullSync(t *testing.T) {
	h := newHarness(t)
	h.srv.put("a", simperium.Note{Content: "purge me"})
	h.run(t)
	st := h.stateByServerKey("a")
	if st == nil {
		t.Fatal("pull failed")
	}
	fname := st.Filename

	// Simulate a hard delete: the note vanishes without a change entry.
	h.srv.mu.Lock()
	delete(h.srv.notes, "a")
	h.srv.mu.Unlock()

	h.run(t) // incremental: absence is indistinguishable from "unchanged"
	if _, err := os.Stat(filepath.Join(h.dir, fname)); err != nil {
		t.Fatalf("incremental sync should keep the local file: %v", err)
	}

	h.runFull(t) // full reconciliation notices the purge
	if _, err := os.Stat(filepath.Join(h.dir, fname)); !os.IsNotExist(err) {
		t.Fatal("full sync should have trashed the local file")
	}
	if st := h.stateByServerKey("a"); st != nil {
		t.Fatalf("purged note should be dropped from state, got %+v", st)
	}
}

func TestPeriodicFullSync(t *testing.T) {
	h := newHarness(t)
	h.cfg.FullSyncEvery = 2
	h.srv.put("a", simperium.Note{Content: "alpha"})
	h.run(t) // full, counter stays 0
	h.run(t) // incremental, counter 1
	h.run(t) // incremental, counter 2
	if h.st.IncrementalSinceFull != 2 {
		t.Fatalf("counter should be 2, got %d", h.st.IncrementalSinceFull)
	}
	h.run(t) // counter hit the limit: full sync, reset to 0
	if h.st.IncrementalSinceFull != 0 {
		t.Fatalf("periodic full sync did not reset the counter: %d", h.st.IncrementalSinceFull)
	}
}

func TestFrontMatterIsLocalOnly(t *testing.T) {
	h := newHarness(t)
	h.write(t, "note.md", "---\ntitle: local\n---\nbody text")
	h.run(t)

	id, n := h.noteByContent("body text")
	if id == "" {
		t.Fatal("note body was not pushed")
	}
	if strings.Contains(n.Content, "title: local") {
		t.Fatalf("front matter leaked to the server: %q", n.Content)
	}
	if n.Content != "body text" {
		t.Fatalf("server content = %q, want the body only", n.Content)
	}

	// The file is renamed after its (body) title, keeps the front matter and
	// uses CRLF line endings.
	got := h.read(t, "body text.md")
	if !strings.Contains(got, "title: local") {
		t.Fatalf("front matter was dropped locally: %q", got)
	}
	if !strings.HasPrefix(got, "---\r\n") || !strings.Contains(got, "\r\nbody text") {
		t.Fatalf("file is not CRLF: %q", got)
	}
	if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Fatalf("file contains a bare LF: %q", got)
	}

	// Editing only the front matter does not change the note, so nothing is
	// pushed and the second sync is otherwise a no-op.
	h.write(t, "body text.md", "---\ntitle: edited\n---\nbody text")
	h.run(t)
	if h.srv.pushes != 1 {
		t.Fatalf("front matter edit triggered a push: %d pushes", h.srv.pushes)
	}
	if got := h.read(t, "body text.md"); !strings.Contains(got, "title: edited") {
		t.Fatalf("edited front matter was not kept: %q", got)
	}
}

func TestPullPreservesFrontMatter(t *testing.T) {
	h := newHarness(t)
	h.srv.put("a", simperium.Note{Content: "old body"})
	h.run(t)

	st := h.stateByServerKey("a")
	if st == nil {
		t.Fatal("pull failed")
	}
	// Attach front matter locally, then let the server change the body.
	h.write(t, st.Filename, "---\nsite: docs\n---\nold body")
	h.srv.put("a", simperium.Note{Content: "new body"})
	h.run(t)

	got := h.read(t, "new body.md")
	if !strings.Contains(got, "site: docs") {
		t.Fatalf("front matter lost on pull: %q", got)
	}
	if !strings.Contains(got, "\r\nnew body") {
		t.Fatalf("pulled file is not CRLF: %q", got)
	}
	if _, n := h.noteByContent("new body"); strings.Contains(n.Content, "site: docs") {
		t.Fatalf("front matter leaked to the server: %q", n.Content)
	}
}

func TestTagsLiveInFrontMatter(t *testing.T) {
	h := newHarness(t)
	h.srv.put("a", simperium.Note{Content: "tagged note", Tags: []string{"work"}})
	h.run(t)

	// Server tags are written into the file's front matter.
	got := h.read(t, "tagged note.md")
	if !strings.Contains(got, "tags:\r\n  - work") {
		t.Fatalf("tags not written to front matter: %q", got)
	}

	// Editing the tags in the file pushes the new set to the server.
	h.write(t, "tagged note.md", "---\ntags: [home, work]\n---\ntagged note")
	h.run(t)
	if _, n := h.noteByContent("tagged note"); !equalStrings(n.Tags, []string{"home", "work"}) {
		t.Fatalf("local tag edit not pushed: %v", n.Tags)
	}

	// A remote tag change is written back into the file.
	h.srv.put("a", simperium.Note{Content: "tagged note", Tags: []string{"remote"}})
	h.run(t)
	if got := h.read(t, "tagged note.md"); !strings.Contains(got, "tags:\r\n  - remote") {
		t.Fatalf("remote tags not written to front matter: %q", got)
	}
}
