package main

import (
	"fmt"
	"time"

	"github.com/dodorz/snsync/internal/config"
	"github.com/dodorz/snsync/internal/simperium"
)

// runRecoverPins restores pins that snsync 0.1.9 removed by treating a missing
// "pinned" front matter key as "unpinned". For every live note whose current
// version is not pinned, it looks at the previous version on the server and
// re-pins the note when that version still had the "pinned" system tag.
//
// It is a dry run unless --yes is given.
func runRecoverPinsProfile(p *config.Profile, yes bool) error {
	cfg, sn, st, lk, err := loadProfileLocked(p)
	if err != nil {
		return err
	}
	defer lk.Release()
	if err := ensureClientAuth(cfg, sn); err != nil {
		return err
	}

	// Only notes this tool tracks are considered, so unrelated notes that were
	// unpinned elsewhere are never touched.
	tracked := map[string]bool{}
	names := map[string]string{}
	for _, ns := range st.Notes {
		if ns.ServerKey != "" {
			tracked[ns.ServerKey] = true
			names[ns.ServerKey] = ns.Filename
		}
	}

	fmt.Println("fetching the note index (this can take a while on a large account)...")
	remotes, _, err := sn.Index("")
	if err != nil {
		return err
	}

	type candidate struct {
		key  string
		name string
		note simperium.Note
	}
	var todo []candidate
	var failed int
	for _, r := range remotes {
		if !tracked[r.ID] || r.Note.Deleted || containsPin(r.Note.SystemTags) || r.Version <= 1 {
			continue
		}
		prev, err := sn.GetVersion(r.ID, r.Version-1)
		if err != nil || prev == nil {
			// A missing history entry is not fatal: skip this note.
			failed++
			continue
		}
		if !containsPin(prev.SystemTags) {
			continue
		}
		todo = append(todo, candidate{key: r.ID, name: names[r.ID], note: r.Note})
	}

	if len(todo) == 0 {
		fmt.Println("no recoverable pins found")
		if failed > 0 {
			fmt.Printf("(%d note(s) had no readable previous version)\n", failed)
		}
		return nil
	}
	for _, c := range todo {
		name := c.name
		if name == "" {
			name = c.key
		}
		fmt.Printf("re-pin  %s\n", name)
	}
	if !yes {
		fmt.Printf("dry run: %d note(s) would be re-pinned (use --yes to apply)\n", len(todo))
		return nil
	}
	now := float64(time.Now().Unix())
	for _, c := range todo {
		n := c.note
		n.SystemTags = append(n.SystemTags, "pinned")
		n.ModificationDate = now
		if _, _, err := sn.Save(c.key, n); err != nil {
			return fmt.Errorf("re-pin %s: %w", c.key, err)
		}
	}
	fmt.Printf("re-pinned %d note(s); run `snsync sync` to write pinned: true to the files\n", len(todo))
	return nil
}

func containsPin(systemTags []string) bool {
	for _, t := range systemTags {
		if t == "pinned" {
			return true
		}
	}
	return false
}

// ensureClientAuth makes sure the client has a token, authorizing with the
// configured password if necessary and caching the result.
func ensureClientAuth(cfg *config.Config, sn *simperium.Client) error {
	if cfg.Token != "" {
		return nil
	}
	if cfg.Password == "" {
		return fmt.Errorf("no cached token and no password; run `snsync login` first")
	}
	tok, err := sn.Authorize(cfg.Email, cfg.Password)
	if err != nil {
		return err
	}
	cfg.Token = tok
	return cfg.Save()
}
