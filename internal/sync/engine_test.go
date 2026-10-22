package sync

import (
	"encoding/json"
	"fmt"
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

// body returns the note body of a local file, checking on the way that the file
// records the note id it is synced under.
func (h *harness) body(t *testing.T, name string) string {
	t.Helper()
	front, body := store.SplitFrontMatter(store.Normalize(h.read(t, name)))
	if front != "" && store.FrontMatterID(front) == "" {
		t.Fatalf("%s: front matter without a note id: %q", name, front)
	}
	return body
}

// noteID returns the note id recorded in a local file's front matter.
func (h *harness) noteID(t *testing.T, name string) string {
	t.Helper()
	front, _ := store.SplitFrontMatter(store.Normalize(h.read(t, name)))
	return store.FrontMatterID(front)
}

// withBody replaces the body of a local file, keeping its front matter.
func (h *harness) keepFrontMatter(t *testing.T, name, body string) string {
	t.Helper()
	front, _ := store.SplitFrontMatter(store.Normalize(h.read(t, name)))
	return front + body
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

// Renaming a file and editing it in the same step must still update the same
// note: the front matter's note id identifies it even though the body changed.
func TestRenameWithEditKeepsTheNote(t *testing.T) {
	h := newHarness(t)
	h.srv.put("a", simperium.Note{Content: "alpha"})
	h.srv.put("b", simperium.Note{Content: "beta"})
	h.run(t)

	st := h.stateByServerKey("a")
	if st == nil {
		t.Fatal("alpha not pulled")
	}
	edited := h.keepFrontMatter(t, st.Filename, "alpha edited")
	if err := os.Remove(filepath.Join(h.dir, st.Filename)); err != nil {
		t.Fatal(err)
	}
	h.write(t, "renamed.md", edited)
	h.run(t)

	if len(h.srv.notes) != 2 {
		t.Fatalf("rename with edit changed the note count: %d", len(h.srv.notes))
	}
	if n := h.srv.notes["a"]; n.Deleted || n.Content != "alpha edited" {
		t.Fatalf("note a was not updated in place: %+v", n)
	}
	if n := h.srv.notes["b"]; n.Deleted || n.Content != "beta" {
		t.Fatalf("note b was disturbed: %+v", n)
	}
	if st := h.stateByServerKey("a"); st == nil || st.Filename != "alpha edited.md" {
		t.Fatalf("state not updated: %+v", st)
	}
}

// Files written by older versions (and TOML front matter) carry no note id, so
// a rename that also edits the body is recognised by the one-to-one pairing:
// exactly one tracked note lost its file and exactly one file appeared.
func TestRenameWithEditWithoutNoteID(t *testing.T) {
	h := newHarness(t)
	h.srv.put("a", simperium.Note{Content: "first version"})
	h.run(t)

	st := h.stateByServerKey("a")
	if err := os.Remove(filepath.Join(h.dir, st.Filename)); err != nil {
		t.Fatal(err)
	}
	h.write(t, "totally different.md", "second version")
	h.run(t)

	if len(h.srv.notes) != 1 {
		t.Fatalf("expected the note to be adopted, got %d notes", len(h.srv.notes))
	}
	if n := h.srv.notes["a"]; n.Deleted || n.Content != "second version" {
		t.Fatalf("note not updated in place: %+v", n)
	}
}

// The pairing is a guess, so the body it overwrites must stay recoverable.
func TestInferredRenameKeepsOverwrittenBody(t *testing.T) {
	h := newHarness(t)
	h.write(t, "alpha.md", "alpha body")
	h.run(t)

	id, _ := h.noteByContent("alpha body")
	st := h.stateByServerKey(id)
	if err := os.Remove(filepath.Join(h.dir, st.Filename)); err != nil {
		t.Fatal(err)
	}
	h.write(t, "unrelated.md", "unrelated body")
	h.run(t)

	if len(h.srv.notes) != 1 {
		t.Fatalf("expected 1 note, got %d", len(h.srv.notes))
	}
	if n := h.srv.notes[id]; n.Content != "unrelated body" {
		t.Fatalf("note was not updated: %+v", n)
	}
	trashed, err := os.ReadDir(h.cfg.TrashDir)
	if err != nil {
		t.Fatalf("read trash: %v", err)
	}
	if len(trashed) != 1 {
		t.Fatalf("expected the overwritten body in the trash, got %v", trashed)
	}
	if got := h.read(t, filepath.Join(".trash", trashed[0].Name())); !strings.Contains(got, "alpha body") {
		t.Fatalf("trash copy does not hold the old body: %q", got)
	}
}

// With more than one note in flight there is nothing to pair, so no guess is
// made: the missing notes are trashed and the new file becomes a new note.
func TestAmbiguousRenameIsNotGuessed(t *testing.T) {
	h := newHarness(t)
	h.srv.put("a", simperium.Note{Content: "alpha"})
	h.srv.put("b", simperium.Note{Content: "beta"})
	h.run(t)

	for _, key := range []string{"a", "b"} {
		st := h.stateByServerKey(key)
		if err := os.Remove(filepath.Join(h.dir, st.Filename)); err != nil {
			t.Fatal(err)
		}
	}
	h.write(t, "gamma.md", "gamma")
	h.run(t)

	if n := h.srv.notes["a"]; !n.Deleted {
		t.Fatalf("note a should have been trashed: %+v", n)
	}
	if n := h.srv.notes["b"]; !n.Deleted {
		t.Fatalf("note b should have been trashed: %+v", n)
	}
	if _, n := h.noteByContent("gamma"); n.Content == "" || n.Deleted {
		t.Fatalf("the new file was not pushed: %+v", n)
	}
}

// Copying a file copies its note id too. The copy must not hijack the original
// note: it gets its own key, and the stale id is rewritten.
func TestCopyOfNoteGetsItsOwnID(t *testing.T) {
	h := newHarness(t)
	h.write(t, "note.md", "shared body")
	h.run(t)

	id, _ := h.noteByContent("shared body")
	st := h.stateByServerKey(id)
	h.write(t, "copy.md", h.read(t, st.Filename))
	h.run(t)

	if len(h.srv.notes) != 2 {
		t.Fatalf("copy should become its own note, got %d notes", len(h.srv.notes))
	}
	for k, n := range h.srv.notes {
		if n.Deleted {
			t.Fatalf("note %s was deleted", k)
		}
	}
	origID := h.noteID(t, "shared body.md")
	copyID := h.noteID(t, "shared body (2).md")
	if origID == "" || copyID == "" || origID == copyID {
		t.Fatalf("ids are not unique: original %q, copy %q", origID, copyID)
	}
}

// The id in the file is bookkeeping, not authority: it is rewritten from the
// state on every pass, so a hand-edited one is repaired instead of trusted.
func TestNoteIDIsRewrittenFromState(t *testing.T) {
	h := newHarness(t)
	h.write(t, "x.md", "hello")
	h.run(t)

	id, _ := h.noteByContent("hello")
	st := h.stateByServerKey(id)
	if got := h.noteID(t, st.Filename); got != id {
		t.Fatalf("file records id %q, want %q", got, id)
	}

	h.write(t, st.Filename, "---\nsimplenote_id: bogus\n---\nhello")
	h.run(t)

	if got := h.noteID(t, st.Filename); got != id {
		t.Fatalf("id not repaired: %q, want %q", got, id)
	}
	if len(h.srv.notes) != 1 || h.srv.pushes != 1 {
		t.Fatalf("repairing the id caused a push: %d pushes, %d notes", h.srv.pushes, len(h.srv.notes))
	}
}

// After a lost state file the note id alone is enough to re-adopt a note whose
// body has changed in the meantime.
func TestLostStateAdoptsByNoteIDAfterEdit(t *testing.T) {
	h := newHarness(t)
	h.write(t, "hello.md", "hello world")
	h.run(t)

	id, _ := h.noteByContent("hello world")
	st := h.stateByServerKey(id)
	edited := h.keepFrontMatter(t, st.Filename, "hello edited")

	if err := os.Remove(h.cfg.StateFile); err != nil {
		t.Fatal(err)
	}
	fresh, err := store.LoadState(h.cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	h.st = fresh
	h.eng = New(h.cfg, h.eng.sn, fresh)
	h.eng.Log = t.Logf
	h.write(t, st.Filename, edited)
	h.runFull(t)

	if len(h.srv.notes) != 1 {
		t.Fatalf("lost state duplicated the note: %d notes", len(h.srv.notes))
	}
	if n := h.srv.notes[id]; n.Deleted || n.Content != "hello edited" {
		t.Fatalf("note not updated in place: %+v", n)
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
	want := "---\r\nsimplenote_id: abc\r\ntags:\r\n  - work\r\n---\r\nfrom the server"
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

func TestMassLocalDeleteIsRefused(t *testing.T) {
	h := newHarness(t)
	const n = massDeleteMin + 5
	for i := 0; i < n; i++ {
		h.write(t, fmt.Sprintf("note-%02d.md", i), fmt.Sprintf("content %d", i))
	}
	h.run(t)
	if len(h.srv.notes) != n {
		t.Fatalf("expected %d notes, got %d", n, len(h.srv.notes))
	}

	entries, err := os.ReadDir(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			if err := os.Remove(filepath.Join(h.dir, e.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}

	// The guard must refuse before anything is trashed.
	err = h.eng.Run(false)
	if err == nil {
		t.Fatal("expected the mass deletion guard to abort the sync")
	}
	if !strings.Contains(err.Error(), "allow-mass-delete") {
		t.Fatalf("error should name the override flag: %v", err)
	}
	for id, note := range h.srv.notes {
		if note.Deleted {
			t.Fatalf("note %s was trashed despite the guard", id)
		}
	}

	// The override lets an intentional cleanup proceed.
	h.eng.AllowMassDelete = true
	h.run(t)
	deleted := 0
	for _, note := range h.srv.notes {
		if note.Deleted {
			deleted++
		}
	}
	if deleted != n {
		t.Fatalf("expected %d trashed notes, got %d", n, deleted)
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
	if st := h.stateByServerKey(id); st != nil {
		t.Fatalf("trashed note should not stay in the state file: %+v", st)
	}
}

func TestFullSyncDoesNotAccumulateTrash(t *testing.T) {
	h := newHarness(t)
	// A note that only exists in the remote trash, plus a live one so the
	// state file is non-empty.
	h.srv.put("live", simperium.Note{Content: "live note"})
	h.srv.put("trash", simperium.Note{Content: "only in trash", Deleted: true})

	h.runFull(t)

	if st := h.stateByServerKey("trash"); st != nil {
		t.Fatalf("trashed note was added to the state file: %+v", st)
	}
	if len(h.st.Notes) != 1 {
		t.Fatalf("expected only the live note in state, got %d entries", len(h.st.Notes))
	}
	if st := h.stateByServerKey("live"); st == nil || st.Deleted {
		t.Fatalf("live note missing from state: %+v", st)
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
	if got := h.body(t, "remote edit.md"); got != "remote edit" {
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
	h.write(t, "note.md", "---\nsource: local\n---\nbody text")
	h.run(t)

	id, n := h.noteByContent("body text")
	if id == "" {
		t.Fatal("note body was not pushed")
	}
	if strings.Contains(n.Content, "source: local") {
		t.Fatalf("front matter leaked to the server: %q", n.Content)
	}
	if n.Content != "body text" {
		t.Fatalf("server content = %q, want the body only", n.Content)
	}

	// The file is renamed after its (body) title, keeps the front matter and
	// uses CRLF line endings.
	got := h.read(t, "body text.md")
	if !strings.Contains(got, "source: local") {
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
	h.write(t, "body text.md", "---\nsource: edited\n---\nbody text")
	h.run(t)
	if h.srv.pushes != 1 {
		t.Fatalf("front matter edit triggered a push: %d pushes", h.srv.pushes)
	}
	if got := h.read(t, "body text.md"); !strings.Contains(got, "source: edited") {
		t.Fatalf("edited front matter was not kept: %q", got)
	}
}

func TestFrontMatterTitleControlsFilename(t *testing.T) {
	h := newHarness(t)
	h.write(t, "note.md", "---\ntitle: My Custom Name\n---\nbody first line")
	h.run(t)

	// The file is named from the front matter title, not the body.
	if _, err := os.Stat(filepath.Join(h.dir, "My Custom Name.md")); err != nil {
		t.Fatalf("file not named from front matter title: %v", err)
	}
	// The title is local-only and must not leak into the note body.
	if _, n := h.noteByContent("body first line"); strings.Contains(n.Content, "My Custom Name") {
		t.Fatalf("front matter title leaked to the server: %q", n.Content)
	}

	// Changing the body's first line keeps the front matter title as the name.
	h.write(t, "My Custom Name.md", "---\ntitle: My Custom Name\n---\nnew body first line")
	h.run(t)
	if _, err := os.Stat(filepath.Join(h.dir, "My Custom Name.md")); err != nil {
		t.Fatalf("file was renamed away from the front matter title: %v", err)
	}
	if _, n := h.noteByContent("new body first line"); n.Content == "" {
		t.Fatal("edited body was not pushed")
	}
	if _, err := os.Stat(filepath.Join(h.dir, "new body first line.md")); !os.IsNotExist(err) {
		t.Fatalf("file was also named from the body first line")
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

// A crash or an interrupted run can lose the state file after notes were
// already created on the server. The next run must adopt the existing pair
// instead of pushing a duplicate note for the local file.
func TestLostStateAdoptsExistingNote(t *testing.T) {
	h := newHarness(t)
	h.write(t, "hello.md", "hello world")
	h.run(t)
	if h.srv.pushes != 1 || len(h.srv.notes) != 1 {
		t.Fatalf("setup: pushes=%d notes=%d", h.srv.pushes, len(h.srv.notes))
	}

	// Forget everything the state file knew, as if the process had been killed
	// before it could save.
	if err := os.Remove(h.cfg.StateFile); err != nil {
		t.Fatal(err)
	}
	st, err := store.LoadState(h.cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	h.st = st
	h.eng = New(h.cfg, h.eng.sn, st)
	h.eng.Log = t.Logf

	h.runFull(t)

	if h.srv.pushes != 1 {
		t.Fatalf("lost state caused a duplicate push: %d pushes", h.srv.pushes)
	}
	if len(h.srv.notes) != 1 {
		t.Fatalf("lost state duplicated the server note: %d notes", len(h.srv.notes))
	}
	files, err := store.ListMarkdown(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "hello world.md" {
		t.Fatalf("unexpected local files after recovery: %v", files)
	}
}

// Existing duplicates (for example from older snsync versions) must be
// absorbed, not multiplied. Every extra run used to create one remote note per
// local file and one local file per remote note, growing exponentially.
func TestDuplicatesFromLostStateDoNotMultiply(t *testing.T) {
	h := newHarness(t)
	h.write(t, "hello.md", "hello world")
	h.run(t)

	// Simulate a crashed run that pushed the same content under another key and
	// then lost the state file.
	h.srv.put("duplicate", simperium.Note{Content: "hello world"})
	if err := os.Remove(h.cfg.StateFile); err != nil {
		t.Fatal(err)
	}
	st, err := store.LoadState(h.cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	h.st = st
	h.eng = New(h.cfg, h.eng.sn, st)
	h.eng.Log = t.Logf

	pushes := h.srv.pushes
	notes := len(h.srv.notes)

	h.runFull(t)

	if h.srv.pushes != pushes {
		t.Fatalf("recovery pushed new notes: %d -> %d", pushes, h.srv.pushes)
	}
	if len(h.srv.notes) != notes {
		t.Fatalf("recovery changed the server note count: %d -> %d", notes, len(h.srv.notes))
	}
	files, _ := store.ListMarkdown(h.dir)
	if len(files) != 2 {
		t.Fatalf("expected both copies locally, got %v", files)
	}

	// The next pass must be a no-op as well.
	h.runFull(t)
	if h.srv.pushes != pushes {
		t.Fatalf("follow-up sync pushed more notes: %d -> %d", pushes, h.srv.pushes)
	}
	if got, _ := store.ListMarkdown(h.dir); len(got) != 2 {
		t.Fatalf("follow-up sync created more files: %v", got)
	}
}
