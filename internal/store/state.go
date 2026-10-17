package store

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// NoteState is the per-note sync bookkeeping. ID is a stable local identifier
// (the server key once known, otherwise a generated local id). ServerKey is
// empty until the note has been pushed. Tags and SystemTags live here rather
// than in the file so that the local file stays pure note content.
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
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
