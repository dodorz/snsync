// Package sync implements the bidirectional reconciliation between a local
// markdown directory and a Simplenote account.
package sync

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/dodorz/snsync/internal/config"
	"github.com/dodorz/snsync/internal/simperium"
	"github.com/dodorz/snsync/internal/store"
)

// Engine performs one reconciliation pass.
type Engine struct {
	cfg      *config.Config
	sn       *simperium.Client
	st       *store.State
	Log      func(format string, args ...any)
	Full     bool // force a full reconciliation
	fullSync bool // whether the current run is a full reconciliation
}

// New builds an engine.
func New(cfg *config.Config, sn *simperium.Client, st *store.State) *Engine {
	return &Engine{
		cfg: cfg,
		sn:  sn,
		st:  st,
		Log: func(string, ...any) {},
	}
}

// work is the in-memory decision for a single note.
type work struct {
	id string
	st *store.NoteState
	// remote state
	remote       *simperium.RemoteNote
	remoteExists bool
	// local state
	localExists  bool
	localContent string
	localFront   string // front matter as found on disk
	localTags    []string
	localTagsSet bool // whether the front matter has an explicit "tags" key
	localHash    string
	localFilename string
	// decision
	finalContent    string
	finalTags       []string
	finalSystemTags []string
	finalDeleted    bool
	finalFilename   string
	isConflict      bool // this note detected a conflict
	conflictCopy    bool // this work is the generated conflict copy
	writeLocal      bool
	trashLocal      bool
	createRemote    bool
	updateRemote    bool
	pushedDelete    bool

	// remoteUnknown marks a tracked note that was not part of an incremental
	// index response, so its server state is assumed unchanged.
	remoteUnknown bool
	// needRemoteContent means the remote note must be fetched before pushing,
	// so trashing it does not wipe its content.
	needRemoteContent bool
	// preserveBase keeps the stored base hash instead of recomputing it from
	// content we do not have (incremental tombstones).
	preserveBase bool
}

// Run executes one full sync pass. When dryRun is set nothing is written.
func (e *Engine) Run(dryRun bool) error {
	if err := e.ensureAuth(); err != nil {
		return err
	}
	if err := store.EnsureDir(e.cfg.LocalDir); err != nil {
		return err
	}
	if !dryRun {
		if err := store.EnsureDir(e.cfg.TrashDir); err != nil {
			return err
		}
	}

	e.fullSync = e.shouldFullSync()
	since := ""
	if e.fullSync {
		e.Log("full sync: fetching complete note index...")
	} else {
		since = e.st.Cursor
		e.Log("incremental sync since cursor %s...", since)
	}
	remotes, cursor, err := e.sn.Index(since)
	if err != nil {
		return e.wrapAuth(err)
	}
	remoteByID := map[string]simperium.RemoteNote{}
	for _, r := range remotes {
		remoteByID[r.ID] = r
	}
	if e.fullSync {
		e.Log("server has %d notes", len(remoteByID))
	} else {
		e.Log("%d note(s) changed remotely", len(remoteByID))
	}

	works := e.buildWorks(remoteByID)
	e.decideAll(works)
	e.addConflictCopies(works)
	e.assignFilenames(works)

	ids := sortedIDs(works)

	if err := e.applyLocal(works, ids, dryRun); err != nil {
		return err
	}
	if err := e.applyRemote(works, ids, dryRun); err != nil {
		return e.wrapAuth(err)
	}
	e.rebuildState(works, ids, cursor)

	if dryRun {
		e.Log("dry run: state not saved")
		return nil
	}
	if e.fullSync {
		e.st.IncrementalSinceFull = 0
		e.st.LastFullSync = float64(time.Now().Unix())
	} else {
		e.st.IncrementalSinceFull++
	}
	if err := e.st.Save(); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

// shouldFullSync decides whether this run reconciles the whole account or only
// applies the changes since the stored cursor.
func (e *Engine) shouldFullSync() bool {
	if e.Full || e.st.Cursor == "" {
		return true
	}
	if e.cfg.FullSyncEvery <= 0 {
		return false
	}
	return e.st.IncrementalSinceFull >= e.cfg.FullSyncEvery
}

// buildWorks turns the state file, the server index and the local directory
// into a single set of work items.
func (e *Engine) buildWorks(remoteByID map[string]simperium.RemoteNote) map[string]*work {
	works := map[string]*work{}
	for id, st := range e.st.Notes {
		works[id] = &work{id: id, st: st}
	}

	// Attach remote notes to works by their server key.
	for _, w := range works {
		if w.st.ServerKey == "" {
			continue
		}
		if r, ok := remoteByID[w.st.ServerKey]; ok {
			rr := r
			w.remote = &rr
			w.remoteExists = true
			delete(remoteByID, w.st.ServerKey)
		} else if !e.fullSync {
			// Incremental index only returns changed notes, so an absent
			// tracked note simply has not changed.
			w.remoteUnknown = true
		}
	}

	// Read the local file for each tracked note.
	for _, w := range works {
		if w.st.Filename == "" {
			continue
		}
		p := filepath.Join(e.cfg.LocalDir, w.st.Filename)
		full, err := store.ReadFile(p)
		if err != nil {
			continue // missing or unreadable: treated as a local deletion
		}
		front, body := store.SplitFrontMatter(full)
		tags, tagsSet := store.FrontMatterTags(front)
		w.localExists = true
		w.localFront = front
		w.localTags = tags
		w.localTagsSet = tagsSet
		w.localContent = body
		w.localHash = store.HashContent(body)
		w.localFilename = w.st.Filename
	}

	// Files on disk that no tracked note claims: renames or brand new notes.
	files, _ := store.ListMarkdown(e.cfg.LocalDir)
	claimed := map[string]bool{}
	for _, w := range works {
		if w.localExists {
			claimed[w.localFilename] = true
		}
	}
	missingByHash := map[string]*work{}
	for _, w := range works {
		if !w.localExists && w.st.ServerKey != "" && !w.st.Deleted && w.st.BaseHash != "" {
			missingByHash[w.st.BaseHash] = w
		}
	}
	for _, name := range files {
		if claimed[name] {
			continue
		}
		p := filepath.Join(e.cfg.LocalDir, name)
		full, err := store.ReadFile(p)
		if err != nil {
			continue
		}
		front, body := store.SplitFrontMatter(full)
		tags, tagsSet := store.FrontMatterTags(front)
		h := store.HashContent(body)
		if w, ok := missingByHash[h]; ok {
			// Same content, new name: a manual rename. Adopt it.
			w.localExists = true
			w.localFront = front
			w.localTags = tags
			w.localTagsSet = tagsSet
			w.localContent = body
			w.localHash = h
			w.localFilename = name
			claimed[name] = true
			delete(missingByHash, h)
			continue
		}
		id := "local:" + localHex(8)
		works[id] = &work{
			id:            id,
			st:            &store.NoteState{ID: id},
			localExists:   true,
			localFront:    front,
			localTags:     tags,
			localTagsSet:  tagsSet,
			localContent:  body,
			localHash:     h,
			localFilename: name,
		}
	}

	// Anything still unclaimed in the index is a new remote note.
	for id, r := range remoteByID {
		rr := r
		works[id] = &work{id: id, remote: &rr, remoteExists: true}
	}
	return works
}

func (e *Engine) decideAll(works map[string]*work) {
	for _, w := range works {
		e.decide(w)
	}
}

func (e *Engine) decide(w *work) {
	baseHash := ""
	if w.st != nil {
		baseHash = w.st.BaseHash
	}

	remoteDeleted := w.remoteExists && w.remote.Note.Deleted
	remoteHash := ""
	if w.remoteExists && !remoteDeleted {
		remoteHash = store.HashContent(w.remote.Note.Content)
	}

	// Brand new remote note.
	if w.st == nil {
		if remoteDeleted {
			w.finalDeleted = true
			w.finalContent = w.remote.Note.Content
			return
		}
		w.finalContent = w.remote.Note.Content
		w.finalTags = w.remote.Note.Tags
		w.finalSystemTags = w.remote.Note.SystemTags
		w.writeLocal = true
		return
	}

	hasBase := w.st.ServerKey != ""

	// Incremental sync: the server state of this tracked note is unknown but
	// assumed unchanged, so only local edits are acted on.
	if w.remoteUnknown {
		w.decideLocalOnly()
		return
	}

	if w.remote == nil && !hasBase {
		// Brand new local note (or rename of one).
		if !w.localExists {
			w.finalDeleted = true
			return
		}
		w.finalContent = w.localContent
		w.finalTags = w.tags()
		w.finalSystemTags = w.st.SystemTags
		w.createRemote = true
		return
	}

	localContentChanged := w.localExists && w.localHash != baseHash
	localChanged := localContentChanged || w.tagsChanged()
	localDeleted := !w.localExists
	remoteContentChanged := w.remoteExists && !remoteDeleted && remoteHash != baseHash
	remoteMetaChanged := w.remoteExists &&
		(!equalStrings(w.remote.Note.Tags, w.st.Tags) ||
			!equalStrings(w.remote.Note.SystemTags, w.st.SystemTags))
	remoteFlagChanged := w.remoteExists && w.remote.Note.Deleted != w.st.Deleted
	remoteChanged := remoteContentChanged || remoteMetaChanged || remoteFlagChanged

	switch {
	case w.remoteExists && !remoteDeleted:
		switch {
		case !localDeleted && !localChanged && !remoteChanged:
			w.finalContent = w.localContent
			w.finalTags = w.st.Tags
			w.finalSystemTags = w.st.SystemTags
		case !localDeleted && !localChanged && remoteChanged:
			w.pull()
		case !localDeleted && localChanged && !remoteChanged:
			w.pushLocal()
		case !localDeleted && localChanged && remoteChanged:
			switch {
			case w.localHash == remoteHash:
				w.pull()
			case !localContentChanged:
				// Only the local tags changed; keep the remote content and
				// merge the tags before pushing them back.
				w.finalContent = w.remote.Note.Content
				w.finalTags = mergeTags(w.tags(), w.remote.Note.Tags)
				w.finalSystemTags = w.remote.Note.SystemTags
				w.writeLocal = true
				w.updateRemote = true
			case !remoteContentChanged:
				// The remote only changed metadata; keep the local content and
				// merge the tags before pushing them back.
				w.finalContent = w.localContent
				w.finalTags = mergeTags(w.tags(), w.remote.Note.Tags)
				w.finalSystemTags = w.remote.Note.SystemTags
				w.updateRemote = true
			default:
				w.isConflict = true
				w.pull()
			}
		case localDeleted && !remoteChanged:
			// Local file removed: move the remote note to the trash.
			w.finalDeleted = true
			w.finalContent = w.remote.Note.Content
			w.finalTags = w.st.Tags
			w.finalSystemTags = w.st.SystemTags
			w.updateRemote = true
			w.pushedDelete = true
		case localDeleted && remoteChanged:
			// Remote changed while the file was gone: restore it.
			w.pull()
		}
	case w.remoteExists && remoteDeleted:
		switch {
		case !localDeleted && !localChanged:
			w.finalDeleted = true
			w.finalContent = w.remote.Note.Content
			w.trashLocal = true
		case !localDeleted && localChanged:
			// Edited locally after remote deletion: keep it as a new note.
			w.finalContent = w.localContent
			w.finalTags = w.tags()
			w.finalSystemTags = w.st.SystemTags
			w.createRemote = true
		default:
			w.finalDeleted = true
			w.finalContent = w.remote.Note.Content
		}
	default: // remote note no longer exists (purged or never pushed)
		if hasBase {
			switch {
			case localDeleted:
				w.finalDeleted = true
			case localChanged:
				w.finalContent = w.localContent
				w.finalTags = w.tags()
				w.finalSystemTags = w.st.SystemTags
				w.createRemote = true
			default:
				w.finalDeleted = true
				w.trashLocal = true
			}
		} else if !w.localExists {
			w.finalDeleted = true
		} else {
			w.finalContent = w.localContent
			w.finalTags = w.tags()
			w.finalSystemTags = w.st.SystemTags
			w.createRemote = true
		}
	}
}

func (w *work) pull() {
	if !w.remoteExists {
		return
	}
	w.finalContent = w.remote.Note.Content
	w.finalTags = w.remote.Note.Tags
	w.finalSystemTags = w.remote.Note.SystemTags
	w.writeLocal = true
}

func (w *work) pushLocal() {
	w.finalContent = w.localContent
	w.finalTags = w.tags()
	w.finalSystemTags = w.st.SystemTags
	w.updateRemote = true
}

// tags returns the local tags when the file declares them, otherwise the tags
// remembered in the state. Legacy files without a "tags" key therefore keep the
// server tags instead of being read as "no tags".
func (w *work) tags() []string {
	if w.localTagsSet {
		return w.localTags
	}
	if w.st != nil {
		return w.st.Tags
	}
	return nil
}

// tagsChanged reports whether the file's tags differ from the last synced tags.
func (w *work) tagsChanged() bool {
	return w.localExists && w.localTagsSet && w.st != nil && !equalStrings(w.localTags, w.st.Tags)
}

// mergeTags returns the union of a and b, keeping the order of a first.
func mergeTags(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	seen := map[string]bool{}
	for _, t := range append(append([]string(nil), a...), b...) {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// decideLocalOnly reconciles a tracked note whose server state was not part of
// an incremental index response, so only local changes are considered.
func (w *work) decideLocalOnly() {
	switch {
	case w.localExists && w.st.Deleted:
		// Remote is still trashed but a local file exists again: re-create it.
		w.finalContent = w.localContent
		w.finalTags = w.tags()
		w.finalSystemTags = w.st.SystemTags
		w.createRemote = true
	case w.localExists && (w.localHash != w.st.BaseHash || w.tagsChanged()):
		w.pushLocal()
	case w.localExists:
		// Unchanged; keep the file and the recorded base hash.
		w.finalContent = w.localContent
		w.finalTags = w.tags()
		w.finalSystemTags = w.st.SystemTags
		w.preserveBase = true
	default:
		// Local file removed: propagate the deletion.
		w.finalDeleted = true
		w.preserveBase = true
		if !w.st.Deleted {
			w.updateRemote = true
			w.pushedDelete = true
			w.needRemoteContent = true
		}
	}
}

// addConflictCopies turns a detected conflict into an extra local note holding
// the local version, which is then pushed as a brand new Simplenote note.
func (e *Engine) addConflictCopies(works map[string]*work) {
	var conflicts []*work
	for _, w := range works {
		if w.isConflict {
			conflicts = append(conflicts, w)
		}
	}
	for _, w := range conflicts {
		id := "local:" + localHex(8)
		cw := &work{
			id:              id,
			st:              &store.NoteState{ID: id},
			localFront:      w.localFront,
			localTags:       w.localTags,
			localTagsSet:    w.localTagsSet,
			localContent:    w.localContent,
			localHash:       w.localHash,
			finalContent:    w.localContent,
			finalTags:       w.tags(),
			finalSystemTags: w.st.SystemTags,
			conflictCopy:    true,
			createRemote:    true,
		}
		e.Log("conflict on %q: keeping local copy", w.st.Filename)
		works[id] = cw
	}
}

func (e *Engine) assignFilenames(works map[string]*work) {
	type target struct {
		w    *work
		stem string
	}
	var targets []target
	for _, id := range sortedIDs(works) {
		w := works[id]
		if w.finalDeleted {
			continue
		}
		stem := Sanitize(Title(w.finalContent))
		if w.conflictCopy {
			stem += " (conflict)"
		}
		targets = append(targets, target{w, stem})
	}

	taken := map[string]bool{}
	// Pass 1: keep an existing name when its stem still matches.
	for _, t := range targets {
		if t.w.localFilename != "" && stemOf(t.w.localFilename) == t.stem {
			t.w.finalFilename = t.w.localFilename
			taken[t.w.localFilename] = true
		}
	}
	// Pass 2: assign fresh unique names.
	for _, t := range targets {
		if t.w.finalFilename != "" {
			continue
		}
		name := t.stem + ".md"
		for i := 2; taken[name]; i++ {
			name = t.stem + " (" + itoa(i) + ").md"
		}
		taken[name] = true
		t.w.finalFilename = name
	}
}

func (e *Engine) applyLocal(works map[string]*work, ids []string, dryRun bool) error {
	for _, id := range ids {
		w := works[id]
		oldPath := ""
		if w.localExists && w.localFilename != "" {
			oldPath = filepath.Join(e.cfg.LocalDir, w.localFilename)
		}

		if w.finalDeleted {
			if oldPath != "" && store.Exists(oldPath) {
				e.Log("trash  %s", w.localFilename)
				if !dryRun {
					if _, err := store.MoveToTrash(oldPath, e.cfg.TrashDir); err != nil {
						return err
					}
				}
			}
			continue
		}

		target := filepath.Join(e.cfg.LocalDir, w.finalFilename)
		// Front matter is preserved, but its tags field mirrors the synced
		// tags, and line endings are forced to CRLF.
		front := store.SetFrontMatterTags(w.localFront, w.finalTags)
		if oldPath != "" && oldPath != target && store.Exists(oldPath) {
			e.Log("rename %s -> %s", w.localFilename, w.finalFilename)
			if !dryRun {
				if err := os.Rename(oldPath, target); err != nil {
					if err := store.WriteNoteAtomic(target, front, w.finalContent); err != nil {
						return err
					}
					_ = os.Remove(oldPath)
				}
			}
		}

		// Only rewrite when the bytes on disk already differ, so repeated syncs
		// are idempotent.
		desired := store.EncodeNote(front, w.finalContent)
		if b, err := os.ReadFile(target); err == nil && string(b) == desired {
			continue
		}
		e.Log("write  %s", w.finalFilename)
		if !dryRun {
			if err := store.WriteNoteAtomic(target, front, w.finalContent); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) applyRemote(works map[string]*work, ids []string, dryRun bool) error {
	now := float64(time.Now().Unix())
	for _, id := range ids {
		w := works[id]
		if !w.createRemote && !w.updateRemote {
			continue
		}
		// In incremental mode a local deletion is propagated without having
		// fetched the note; fetch it first so its content is not wiped.
		if w.needRemoteContent && w.st != nil && w.st.ServerKey != "" {
			if rn, err := e.sn.Get(w.st.ServerKey); err == nil && rn != nil {
				w.finalContent = rn.Content
				if w.finalTags == nil {
					w.finalTags = rn.Tags
				}
				if w.finalSystemTags == nil {
					w.finalSystemTags = rn.SystemTags
				}
			}
		}
		note := simperium.Note{
			Content:          w.finalContent,
			Deleted:          w.finalDeleted,
			CreationDate:     now,
			ModificationDate: now,
			Tags:             nonNil(w.finalTags),
			SystemTags:       nonNil(w.finalSystemTags),
			ShareURL:         "",
			PublishURL:       "",
		}
		if w.remoteExists && w.remote.Note.CreationDate > 0 {
			note.CreationDate = w.remote.Note.CreationDate
		}

		if w.createRemote {
			e.Log("push   %s (new)", w.displayName())
			if dryRun {
				continue
			}
			key, _, err := e.sn.Save("", note)
			if err != nil {
				return err
			}
			if w.st == nil {
				w.st = &store.NoteState{ID: w.id}
			}
			w.st.ServerKey = key
			w.st.Version = 0
		} else {
			e.Log("push   %s (update)", w.displayName())
			if dryRun {
				continue
			}
			if _, _, err := e.sn.Save(w.st.ServerKey, note); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) rebuildState(works map[string]*work, ids []string, cursor string) {
	next := map[string]*store.NoteState{}
	for _, id := range ids {
		w := works[id]
		if w.finalDeleted {
			keep := w.remoteExists || w.pushedDelete
			if !keep {
				continue
			}
		}
		baseHash := store.HashContent(w.finalContent)
		if w.preserveBase && w.st != nil && w.st.BaseHash != "" {
			baseHash = w.st.BaseHash
		}
		ns := &store.NoteState{
			ID:         w.id,
			BaseHash:   baseHash,
			Tags:       w.finalTags,
			SystemTags: w.finalSystemTags,
			Deleted:    w.finalDeleted,
			ModTime:    float64(time.Now().Unix()),
		}
		if w.st != nil {
			ns.ServerKey = w.st.ServerKey
		}
		if w.remoteExists {
			if ns.ServerKey == "" {
				ns.ServerKey = w.remote.ID
			}
			ns.Version = w.remote.Version
			ns.ModTime = w.remote.Note.ModificationDate
			if w.finalDeleted {
				ns.BaseHash = store.HashContent(w.remote.Note.Content)
			}
		}
		if !w.finalDeleted {
			ns.Filename = w.finalFilename
		}
		next[id] = ns
	}
	e.st.Notes = next
	e.st.Cursor = cursor
}

func (w *work) displayName() string {
	if w.finalFilename != "" {
		return w.finalFilename
	}
	if w.st != nil && w.st.Filename != "" {
		return w.st.Filename
	}
	return w.id
}

func (e *Engine) ensureAuth() error {
	if e.sn.Token != "" {
		return nil
	}
	if e.cfg.Password == "" {
		return fmt.Errorf("no cached token and no password (set \"password\" in the config or SN_PASSWORD)")
	}
	e.Log("logging in as %s...", e.cfg.Email)
	tok, err := e.sn.Authorize(e.cfg.Email, e.cfg.Password)
	if err != nil {
		return err
	}
	e.cfg.Token = tok
	if err := e.cfg.Save(); err != nil {
		return fmt.Errorf("cache token: %w", err)
	}
	return nil
}

func (e *Engine) wrapAuth(err error) error {
	if _, ok := err.(*simperium.AuthError); ok {
		return fmt.Errorf("%w (cached token may have expired; clear \"token\" in the config)", err)
	}
	return err
}

var stemUniqRe = regexp.MustCompile(` \(\d+\)$`)

func stemOf(filename string) string {
	s := filename
	if ext := filepath.Ext(s); ext != "" {
		s = s[:len(s)-len(ext)]
	}
	return stemUniqRe.ReplaceAllString(s, "")
}

func sortedIDs(works map[string]*work) []string {
	ids := make([]string, 0, len(works))
	for id := range works {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func localHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
