package store

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// NoteState is the per-note sync bookkeeping. ID is a stable local identifier
// (the server key once known, otherwise a generated local id). ServerKey is
// empty until the note has been pushed. Tags are mirrored into the file's front
// matter and kept here as the last-synced baseline; SystemTags live only here.
type NoteState struct {
	ID         string   `json:"id"`
	ServerKey  string   `json:"server_key,omitempty"`
	Filename   string   `json:"filename,omitempty"`
	BaseHash   string   `json:"base_hash,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	SystemTags []string `json:"system_tags,omitempty"`
	Deleted    bool     `json:"deleted,omitempty"`
	Version    int      `json:"version,omitempty"`
	ModTime    float64  `json:"mod_time,omitempty"`
}

// State is the persisted sync state.
type State struct {
	// Cursor is the Simperium index cursor ("current") from the last sync.
	Cursor string                `json:"cursor,omitempty"`
	Notes  map[string]*NoteState `json:"notes"`

	// IncrementalSinceFull counts how many incremental syncs have run since
	// the last full reconciliation; a full sync is forced periodically so
	// that hard-deleted notes (which never appear in incremental data) are
	// noticed.
	IncrementalSinceFull int     `json:"incremental_since_full,omitempty"`
	LastFullSync         float64 `json:"last_full_sync,omitempty"`

	// PinnedInFile records that the "pinned" front matter key is authoritative
	// (a missing key means "not pinned"). It is false in state files written
	// before pin support, so the first sync after upgrading migrates those
	// files instead of unpinning every pinned note.
	PinnedInFile bool `json:"pinned_in_file,omitempty"`

	path string
}

// LoadState reads the state file. A missing file yields an empty state.
func LoadState(path string) (*State, error) {
	s := &State{Notes: map[string]*NoteState{}, path: path}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	if s.Notes == nil {
		s.Notes = map[string]*NoteState{}
	}
	s.path = path
	return s, nil
}

// Path returns the file the state is stored in.
func (s *State) Path() string { return s.path }

// Save writes the state file atomically.
func (s *State) Save() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	// Use a unique temp file so a race with another writer can never make two
	// processes write into the same temporary path.
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}
