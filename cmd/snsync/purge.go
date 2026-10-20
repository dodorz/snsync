package main

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/dodorz/snsync/internal/simperium"
)

// purgeTarget is a trashed note that the user asked to hard-delete.
type purgeTarget struct {
	stateKey  string // key in state.Notes (not always the server key)
	serverKey string
	filename  string
	hash      string
}

// runPurgeTrash permanently deletes notes that were moved to the Simplenote
// trash within the --since window. Older trash entries are left untouched.
//
// The state file already records every trashed note and the time the deletion
// was synced, so no full index download is needed. Without --yes the command
// only reports what it would do.
func runPurgeTrash(path string, since time.Duration, yes bool, jobs int, purgeOrphans bool) error {
	_, sn, st, lk, err := loadLocked(path)
	if err != nil {
		return err
	}
	defer lk.Release()

	if since <= 0 {
		return fmt.Errorf("purge-trash: --since must be positive")
	}
	if jobs < 1 || jobs > 16 {
		return fmt.Errorf("purge-trash: --jobs must be between 1 and 16")
	}
	cutoff := float64(time.Now().Add(-since).Unix())

	// Content hashes that still have a live note. A trashed note whose content
	// is not represented anywhere else is kept by default: purging it would
	// permanently remove the only remaining copy.
	liveHashes := map[string]bool{}
	for _, n := range st.Notes {
		if !n.Deleted && n.BaseHash != "" {
			liveHashes[n.BaseHash] = true
		}
	}

	var targets []purgeTarget
	older, orphans := 0, 0
	for key, n := range st.Notes {
		if !n.Deleted || n.ServerKey == "" {
			continue
		}
		if n.ModTime < cutoff {
			older++
			continue
		}
		if !purgeOrphans && (n.BaseHash == "" || !liveHashes[n.BaseHash]) {
			orphans++
			continue
		}
		targets = append(targets, purgeTarget{stateKey: key, serverKey: n.ServerKey, filename: n.Filename, hash: n.BaseHash})
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].filename != targets[j].filename {
			return targets[i].filename < targets[j].filename
		}
		return targets[i].serverKey < targets[j].serverKey
	})

	fmt.Printf("cutoff: %s (only trash entries newer than this are purged)\n",
		time.Unix(int64(cutoff), 0).Format(time.RFC3339))
	fmt.Printf("%d trashed note(s) to purge; %d older trashed note(s) will be kept\n", len(targets), older)
	if orphans > 0 {
		fmt.Printf("%d recently trashed note(s) kept because no live copy of their content remains (use --purge-orphans to delete them)\n", orphans)
	}
	if len(targets) == 0 {
		return nil
	}
	if !yes {
		for i, t := range targets {
			if i == 10 {
				fmt.Printf("... and %d more\n", len(targets)-10)
				break
			}
			label := t.filename
			if label == "" {
				label = shortHash(t.hash)
			}
			fmt.Printf("  would purge %-32s %s\n", t.serverKey, label)
		}
		fmt.Println("dry run: re-run with --yes to permanently delete these notes")
		return nil
	}

	// Deletions are independent HTTP requests; a few workers keep a 19k-note
	// cleanup from taking hours while staying polite to the API.
	todo := make(chan purgeTarget)
	type result struct {
		target purgeTarget
		err    error
	}
	results := make(chan result)
	var wg sync.WaitGroup
	for i := 0; i < jobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range todo {
				err := sn.Delete(t.serverKey)
				if err != nil && simperium.IsNotFound(err) {
					err = nil // already hard-deleted by an earlier/aborted run
				}
				results <- result{target: t, err: err}
			}
		}()
	}
	go func() {
		for _, t := range targets {
			todo <- t
		}
		close(todo)
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	done, failed := 0, 0
	for r := range results {
		if r.err != nil {
			failed++
			fmt.Printf("failed %s (%s): %v\n", r.target.filename, r.target.serverKey, r.err)
			continue
		}
		delete(st.Notes, r.target.stateKey)
		done++
		label := r.target.filename
		if label == "" {
			label = shortHash(r.target.hash)
		}
		fmt.Printf("purge  %s\n", label)
		// Persist progress so an interrupted cleanup can resume cheaply.
		if done%1000 == 0 {
			if err := st.Save(); err != nil {
				return fmt.Errorf("save state after %d deletions: %w", done, err)
			}
		}
	}
	if err := st.Save(); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	fmt.Printf("purged %d note(s) permanently; %d failed\n", done, failed)
	if failed > 0 {
		return fmt.Errorf("%d note(s) could not be purged; they remain in the state file", failed)
	}
	return nil
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
