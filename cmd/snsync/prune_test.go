package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dodorz/snsync/internal/store"
)

func TestPruneStateDropsTrashedNotes(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, ".snsync", "state.json")
	cfgPath := filepath.Join(dir, "config.json")

	st, err := store.LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	st.Notes = map[string]*store.NoteState{
		"live":  {ID: "live", ServerKey: "live", Filename: "live.md", BaseHash: "h1"},
		"trash": {ID: "trash", ServerKey: "trash", Deleted: true, BaseHash: "h2"},
		"gone":  {ID: "gone", ServerKey: "gone", Deleted: true, BaseHash: "h3"},
	}
	st.Cursor = "cursor-1"
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	cfg, _ := json.Marshal(map[string]any{
		"email":     "test@example.com",
		"local_dir": dir,
		"token":     "tok",
	})
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		t.Fatal(err)
	}

	// A dry run must leave the file alone.
	if err := runPruneState(cfgPath, true); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Notes) != 3 {
		t.Fatalf("dry run changed the state: %d entries", len(got.Notes))
	}

	if err := runPruneState(cfgPath, false); err != nil {
		t.Fatal(err)
	}
	got, err = store.LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Notes) != 1 {
		t.Fatalf("expected 1 entry after prune, got %d", len(got.Notes))
	}
	if _, ok := got.Notes["live"]; !ok {
		t.Fatal("live entry was removed")
	}
	if got.Cursor != "cursor-1" {
		t.Fatalf("cursor changed: %q", got.Cursor)
	}
}
