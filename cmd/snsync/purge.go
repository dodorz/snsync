package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dodorz/snsync/internal/simperium"
	"github.com/dodorz/snsync/internal/store"
)

// purgeTarget is a trashed note that the user asked to hard-delete.
type purgeTarget struct {
	serverKey string
	hash      string
	label     string
	modTime   float64
}

// runPurgeTrash permanently deletes notes that are currently in the Simplenote
// trash and were modified (trashed) within the --since window. Older trash is
// left untouched.
//
// The state file usually already lists every trashed note, so the common case
// needs no network round-trip beyond the deletes themselves. When the state
// has no tombstones left (for example after `prune-state`), or when --remote is
// given, the full remote index is scanned instead.
func runPurgeTrash(path string, since time.Duration, yes bool, jobs int, purgeOrphans, forceRemote bool) error {
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

	var targets []purgeTarget
	older, orphans := 0, 0
	fromState := !forceRemote
	if fromState {
		targets, older, orphans, fromState = collectStateTargets(st, cutoff, purgeOrphans)
	}
	if !fromState {
		fmt.Println("fetching the note index (this can take a while on a large account)...")
		remotes, _, err := sn.Index("")
		if err != nil {
			return err
		}
		targets, older, orphans = collectRemoteTargets(remotes, cutoff, purgeOrphans)
		fmt.Println("source: remote index")
	} else {
		fmt.Println("source: state file (use --remote to scan the full index)")
	}

	sort.Slice(targets, func(i, j int) bool {
		if targets[i].modTime != targets[j].modTime {
			return targets[i].modTime < targets[j].modTime
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
			fmt.Printf("  would purge %-32s %s\n", t.serverKey, t.label)
		}
		fmt.Println("dry run: re-run with --yes to permanently delete these notes")
		return nil
	}

	// Build a server-key -> state-key index so the state file can be updated
	// without scanning it once per deletion.
	stateKeys := map[string][]string{}
	for key, n := range st.Notes {
		if n.ServerKey != "" {
			stateKeys[n.ServerKey] = append(stateKeys[n.ServerKey], key)
		}
	}

	// Deletions are independent HTTP requests; a few workers keep a large
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
			fmt.Printf("failed %s (%s): %v\n", r.target.label, r.target.serverKey, r.err)
			continue
		}
		for _, key := range stateKeys[r.target.serverKey] {
			delete(st.Notes, key)
		}
		done++
		fmt.Printf("purge  %s\n", r.target.label)
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
		return fmt.Errorf("%d note(s) could not be purged", failed)
	}
	return nil
}

// collectStateTargets uses the tombstones in the state file. The fourth return
// value reports whether the state had any usable tombstones; when it did not,
// the caller falls back to the remote index.
func collectStateTargets(st *store.State, cutoff float64, purgeOrphans bool) ([]purgeTarget, int, int, bool) {
	liveHashes := map[string]bool{}
	hasTombstone := false
	for _, n := range st.Notes {
		if !n.Deleted {
			if n.BaseHash != "" {
				liveHashes[n.BaseHash] = true
			}
			continue
		}
		if n.ServerKey != "" {
			hasTombstone = true
		}
	}
	if !hasTombstone {
		return nil, 0, 0, false
	}

	var targets []purgeTarget
	older, orphans := 0, 0
	for _, n := range st.Notes {
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
		targets = append(targets, purgeTarget{
			serverKey: n.ServerKey,
			hash:      n.BaseHash,
			label:     shortHash(n.BaseHash),
			modTime:   n.ModTime,
		})
	}
	return targets, older, orphans, true
}

// collectRemoteTargets finds trashed notes in a full index response.
func collectRemoteTargets(remotes []simperium.RemoteNote, cutoff float64, purgeOrphans bool) ([]purgeTarget, int, int) {
	liveHashes := map[string]bool{}
	for _, r := range remotes {
		if !r.Note.Deleted {
			liveHashes[store.HashContent(r.Note.Content)] = true
		}
	}

	var targets []purgeTarget
	older, orphans := 0, 0
	for _, r := range remotes {
		if !r.Note.Deleted {
			continue
		}
		if r.Note.ModificationDate < cutoff {
			older++
			continue
		}
		h := store.HashContent(r.Note.Content)
		if !purgeOrphans && !liveHashes[h] {
			orphans++
			continue
		}
		targets = append(targets, purgeTarget{
			serverKey: r.ID,
			hash:      h,
			label:     noteLabel(r.Note.Content),
			modTime:   r.Note.ModificationDate,
		})
	}
	return targets, older, orphans
}

// noteLabel returns a short, human-readable label for a trashed note.
func noteLabel(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > 60 {
			return string(r[:60]) + "..."
		}
		return line
	}
	return "(empty)"
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
