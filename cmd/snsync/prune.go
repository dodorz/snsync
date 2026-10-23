package main

import (
	"fmt"
)

// runPruneState removes bookkeeping for notes that are already in the trash.
//
// Only live notes need a state entry: they map a local file to a server key and
// remember the last synced content. Trashed notes are not written locally, and
// a full sync recognises them as deleted anyway, so keeping a tombstone for
// every one of them only bloats the state file.
func runPruneState(path string, dryRun bool) error {
	_, _, st, lk, err := loadLocked(path)
	if err != nil {
		return err
	}
	defer lk.Release()

	before := len(st.Notes)
	removed := 0
	for key, n := range st.Notes {
		if n.Deleted {
			delete(st.Notes, key)
			removed++
		}
	}
	kept := len(st.Notes)
	fmt.Printf("removed %d deleted note(s); %d live note(s) kept\n", removed, kept)
	if removed == 0 {
		return nil
	}
	if dryRun {
		fmt.Println("dry run: state not saved")
		return nil
	}
	if err := st.Save(); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	fmt.Printf("wrote %s (%d -> %d entries)\n", st.Path(), before, kept)
	return nil
}
