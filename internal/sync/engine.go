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
	"strings"
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

	// AllowMassDelete permits propagating a large number of local deletions
	// (for example after deliberately clearing the notes directory). Without
	// it a sync that would trash most of the account aborts before writing
	// anything, so a moved/restored directory cannot wipe the remote side.
	AllowMassDelete bool
}

const (
	// massDeleteMin is the smallest number of local deletions that can trip
	// the guard; small everyday deletions are never blocked.
	massDeleteMin = 20
	// massDeletePercent is the share of tracked notes that must be deleted for
	// the guard to trigger (25%).
	massDeletePercent = 25
)

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
	localExists   bool
	localContent  string
	localFront    string // front matter as found on disk
	localTitle    string
	localTitleSet bool // whether the front matter has an explicit "title" key
	localTags     []string
	localTagsSet  bool // whether the front matter has an explicit "tags" key
	localPinned   bool // absent "pinned" means false
	// localPinnedKnown is false for TOML ("+++") front matter, which is never
	// rewritten, so its pin is left to the server instead of being unpinned.
	localPinnedKnown bool
	localHash        string
	localFilename    string
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
	// inferredRename marks a rename that was guessed (only one file and one
	// note were left over) rather than proven by note id or content. The body
	// that is about to be overwritten is copied into the trash first, so a
	// wrong guess costs a stray file instead of the note's content.
	inferredRename bool
}

// noteID is the Simplenote key this work belongs to, or "" while the note has
// not been given one yet. It is written into the file's front matter so that a
// later rename can be recognised even after an edit.
func (w *work) noteID() string {
	if w.st != nil && w.st.ServerKey != "" {
		return w.st.ServerKey
	}
	return ""
}

// localFile is a markdown file on disk that no state entry claims yet. It is
// either a manual rename of a tracked note or a brand new note.
type localFile struct {
	name        string
	front       string
	body        string
	title       string
	titleSet    bool
	tags        []string
	tagsSet     bool
	pinned      bool // absent "pinned" means false
	pinnedKnown bool
	hash        string
	id          string // front matter note id, empty when absent
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
	e.assignKeys(works)

	ids := sortedIDs(works)

	if err := e.checkMassDeletion(works); err != nil {
		return err
	}

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
	// Files have now been migrated (the pinned key is written where needed),
	// so a missing key means "not pinned" from the next run on.
	e.st.PinnedInFile = true
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
		title, titleSet := store.FrontMatterTitle(front)
		tags, tagsSet := store.FrontMatterTags(front)
		pinned, pinnedOK := store.FrontMatterPinned(front)
		w.localExists = true
		w.localFront = front
		w.localTitle = title
		w.localTitleSet = titleSet
		w.localTags = tags
		w.localTagsSet = tagsSet
		w.localPinned = pinned
		// Before the pin feature existed, files had no "pinned" key. Treat a
		// missing key as "unknown" for that first sync so upgrading cannot
		// unpin every note; the run then writes the key and marks the state as
		// migrated.
		w.localPinnedKnown = !strings.HasPrefix(front, "+++") &&
			(pinnedOK || e.st.PinnedInFile)
		w.localContent = body
		w.localHash = store.HashContent(body)
		w.localFilename = w.st.Filename
	}

	// Files on disk that no tracked note claims: manual renames or brand new
	// notes.
	files, _ := store.ListMarkdown(e.cfg.LocalDir)
	claimed := map[string]bool{}
	for _, w := range works {
		if w.localExists {
			claimed[w.localFilename] = true
		}
	}

	// Tracked notes whose file vanished locally are rename candidates. They are
	// recognised, in decreasing order of confidence, by the note id in the
	// file's front matter, by an identical body hash, and finally — when
	// exactly one file and one note are left unaccounted for — by a plain
	// one-to-one pairing. The last two exist because the id is only written by
	// newer versions and never for TOML front matter; the pairing also catches
	// a rename that was combined with an edit in a single step.
	var missing []*work
	missingByID := map[string]*work{}
	liveByID := map[string]bool{}
	for _, id := range sortedIDs(works) {
		w := works[id]
		if w.localExists {
			if w.st.ServerKey != "" {
				liveByID[strings.ToLower(w.st.ServerKey)] = true
			}
			continue
		}
		if w.st.ServerKey == "" || w.st.Deleted {
			continue
		}
		missing = append(missing, w)
		missingByID[strings.ToLower(w.st.ServerKey)] = w
	}

	// Remote notes that no state entry claims. If state was lost (crash,
	// interrupted run, deleted state file) these notes look brand new even
	// though a local file may already belong to them. Match them by id, and by
	// content as a fallback, so a lost state file cannot cause an endless
	// stream of duplicate notes.
	unclaimedRemoteByHash := map[string][]string{}
	unclaimedRemoteByID := map[string]string{}
	for id, r := range remoteByID {
		if r.Note.Deleted {
			continue
		}
		h := store.HashContent(r.Note.Content)
		unclaimedRemoteByHash[h] = append(unclaimedRemoteByHash[h], id)
		unclaimedRemoteByID[strings.ToLower(id)] = id
	}
	for h := range unclaimedRemoteByHash {
		sort.Strings(unclaimedRemoteByHash[h])
	}

	var strays []*localFile
	for _, name := range files {
		if claimed[name] {
			continue
		}
		full, err := store.ReadFile(filepath.Join(e.cfg.LocalDir, name))
		if err != nil {
			continue
		}
		front, body := store.SplitFrontMatter(full)
		title, titleSet := store.FrontMatterTitle(front)
		tags, tagsSet := store.FrontMatterTags(front)
		pinned, pinnedOK := store.FrontMatterPinned(front)
		strays = append(strays, &localFile{
			name: name, front: front, body: body,
			title: title, titleSet: titleSet,
			tags: tags, tagsSet: tagsSet,
			pinned: pinned, pinnedKnown: !strings.HasPrefix(front, "+++") && (pinnedOK || e.st.PinnedInFile),
			hash: store.HashContent(body),
			id:   store.FrontMatterID(front),
		})
	}

	adopted := map[*work]bool{}
	var pending []*localFile

	// Pass 1: the note id in the front matter.
	for _, f := range strays {
		key := strings.ToLower(f.id)
		if key != "" {
			if w := missingByID[key]; w != nil && !adopted[w] {
				// The file that used to carry this id is gone, this one carries
				// it: a rename, which survives an edit done in the same step.
				e.adoptRename(w, f, "id")
				adopted[w] = true
				continue
			}
			if rid, ok := unclaimedRemoteByID[key]; ok {
				// No state entry, but the id names an existing note: re-adopt it
				// instead of pushing a duplicate.
				w := e.adoptRemote(works, remoteByID, rid, f)
				adopted[w] = true
				delete(unclaimedRemoteByID, key)
				deleteUnclaimedHash(unclaimedRemoteByHash, rid)
				continue
			}
		}
		pending = append(pending, f)
	}

	// Recompute the candidates now that pass 1 consumed some of them.
	var stillMissing []*work
	idleByHash := map[string]*work{}
	for _, w := range missing {
		if adopted[w] {
			continue
		}
		stillMissing = append(stillMissing, w)
		if w.st.BaseHash != "" {
			idleByHash[w.st.BaseHash] = w
		}
	}

	// Pass 2: identical body content.
	var rest []*localFile
	for _, f := range pending {
		if w, ok := idleByHash[f.hash]; ok {
			// Same content, new name: a manual rename. Adopt it.
			e.adoptRename(w, f, "content")
			adopted[w] = true
			delete(idleByHash, f.hash)
			continue
		}
		if ids := unclaimedRemoteByHash[f.hash]; len(ids) > 0 {
			// Same content on both sides but no state entry: adopt the pair
			// instead of pushing a duplicate and pulling another copy.
			rID := ids[0]
			unclaimedRemoteByHash[f.hash] = ids[1:]
			w := e.adoptRemote(works, remoteByID, rID, f)
			adopted[w] = true
			delete(unclaimedRemoteByID, strings.ToLower(rID))
			continue
		}
		rest = append(rest, f)
	}

	// Pass 3: exactly one file and one note left over. Trust the pairing; this
	// is what makes "rename and edit at once" keep the original note.
	var lastMissing []*work
	for _, w := range stillMissing {
		if !adopted[w] {
			lastMissing = append(lastMissing, w)
		}
	}
	if len(rest) == 1 && len(lastMissing) == 1 && !liveByID[strings.ToLower(rest[0].id)] {
		e.adoptRename(lastMissing[0], rest[0], "sole local change")
		lastMissing[0].inferredRename = true
		adopted[lastMissing[0]] = true
		rest = nil
	}

	// Whatever is left over is a brand new local note. A stale or duplicated
	// note id in its front matter is overwritten once the note gets its own
	// key (see applyLocal), so it cannot keep pointing at another note.
	for _, f := range rest {
		id := "local:" + localHex(8)
		works[id] = &work{
			id:               id,
			st:               &store.NoteState{ID: id},
			localExists:      true,
			localFront:       f.front,
			localTitle:       f.title,
			localTitleSet:    f.titleSet,
			localTags:        f.tags,
			localTagsSet:     f.tagsSet,
			localPinned:      f.pinned,
			localPinnedKnown: f.pinnedKnown,
			localContent:     f.body,
			localHash:        f.hash,
			localFilename:    f.name,
		}
	}

	// Anything still unclaimed in the index is a new remote note.
	for id, r := range remoteByID {
		rr := r
		works[id] = &work{id: id, remote: &rr, remoteExists: true}
	}
	return works
}

// deleteUnclaimedHash drops a remote id from the by-content index once the note
// has been claimed, so it cannot be handed out twice.
func deleteUnclaimedHash(byHash map[string][]string, id string) {
	for h, ids := range byHash {
		for i, got := range ids {
			if got == id {
				byHash[h] = append(ids[:i:i], ids[i+1:]...)
				if len(byHash[h]) == 0 {
					delete(byHash, h)
				}
				return
			}
		}
	}
}

// adoptRename binds a stray file to an existing note, keeping that note's
// server key. how names the signal that identified the pair, for the log.
func (e *Engine) adoptRename(w *work, f *localFile, how string) {
	w.localExists = true
	w.localFront = f.front
	w.localTitle = f.title
	w.localTitleSet = f.titleSet
	w.localTags = f.tags
	w.localTagsSet = f.tagsSet
	w.localPinned = f.pinned
	w.localPinnedKnown = f.pinnedKnown
	w.localContent = f.body
	w.localHash = f.hash
	w.localFilename = f.name
	e.Log("adopt  %s -> %s (%s)", f.name, w.st.Filename, how)
}

// adoptRemote pairs a stray file with a remote note that no state entry claims
// (state was lost, or a push succeeded but its response was never seen). The
// base is taken from the remote content, so any local edit is still pushed.
func (e *Engine) adoptRemote(works map[string]*work, remoteByID map[string]simperium.RemoteNote, remoteID string, f *localFile) *work {
	r := remoteByID[remoteID]
	delete(remoteByID, remoteID)
	w := &work{
		id: remoteID,
		// Base the state on the remote metadata; any local tag differences
		// are merged and pushed normally.
		st: &store.NoteState{
			ID: remoteID, ServerKey: remoteID, BaseHash: store.HashContent(r.Note.Content),
			Tags: r.Note.Tags, SystemTags: r.Note.SystemTags,
			ShareURL: r.Note.ShareURL, PublishURL: r.Note.PublishURL,
		},
		remote:           &r,
		remoteExists:     true,
		localExists:      true,
		localFront:       f.front,
		localTitle:       f.title,
		localTitleSet:    f.titleSet,
		localTags:        f.tags,
		localTagsSet:     f.tagsSet,
		localPinned:      f.pinned,
		localPinnedKnown: f.pinnedKnown,
		localContent:     f.body,
		localHash:        f.hash,
		localFilename:    f.name,
	}
	works[remoteID] = w
	e.Log("adopt  %s (existing note %s)", f.name, remoteID)
	return w
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
		w.finalSystemTags = w.withLocalPin(w.st.SystemTags)
		w.createRemote = true
		return
	}

	localContentChanged := w.localExists && w.localHash != baseHash
	localChanged := localContentChanged || w.tagsChanged() || w.pinnedChanged()
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
				w.finalSystemTags = w.withLocalPin(w.remote.Note.SystemTags)
				w.writeLocal = true
				w.updateRemote = true
			case !remoteContentChanged:
				// The remote only changed metadata; keep the local content and
				// merge the tags before pushing them back.
				w.finalContent = w.localContent
				w.finalTags = mergeTags(w.tags(), w.remote.Note.Tags)
				w.finalSystemTags = w.withLocalPin(w.remote.Note.SystemTags)
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
			w.finalSystemTags = w.withLocalPin(w.st.SystemTags)
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
				w.finalSystemTags = w.withLocalPin(w.st.SystemTags)
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
			w.finalSystemTags = w.withLocalPin(w.st.SystemTags)
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
	w.finalSystemTags = w.withLocalPin(w.st.SystemTags)
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

// pinnedChanged reports whether the file's pin differs from the last synced
// "pinned" system tag. A missing "pinned" key counts as false.
func (w *work) pinnedChanged() bool {
	return w.localExists && w.localPinnedKnown && w.st != nil &&
		w.localPinned != containsString(w.st.SystemTags, "pinned")
}

// withLocalPin applies the file's pin setting on top of base. A missing
// "pinned" key means false, so removing the key unpins.
func (w *work) withLocalPin(base []string) []string {
	if !w.localPinnedKnown {
		return base
	}
	return applyPinned(base, w.localPinned)
}

// shareURL returns the note's current share URL, preferring the remote value.
func (w *work) shareURL() string {
	if w.remoteExists {
		return w.remote.Note.ShareURL
	}
	if w.st != nil {
		return w.st.ShareURL
	}
	return ""
}

// publishURL returns the note's current publish URL, preferring the remote value.
func (w *work) publishURL() string {
	if w.remoteExists {
		return w.remote.Note.PublishURL
	}
	if w.st != nil {
		return w.st.PublishURL
	}
	return ""
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
		w.finalSystemTags = w.withLocalPin(w.st.SystemTags)
		w.createRemote = true
	case w.localExists && (w.localHash != w.st.BaseHash || w.tagsChanged() || w.pinnedChanged()):
		w.pushLocal()
	case w.localExists:
		// Unchanged; keep the file and the recorded base hash.
		w.finalContent = w.localContent
		w.finalTags = w.tags()
		w.finalSystemTags = w.withLocalPin(w.st.SystemTags)
		w.preserveBase = true
	default:
		// Local file removed: propagate the deletion.
		w.finalDeleted = true
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
			id:               id,
			st:               &store.NoteState{ID: id},
			localFront:       w.localFront,
			localTitle:       w.localTitle,
			localTitleSet:    w.localTitleSet,
			localTags:        w.localTags,
			localTagsSet:     w.localTagsSet,
			localPinned:      w.localPinned,
			localPinnedKnown: w.localPinnedKnown,
			localContent:     w.localContent,
			localHash:        w.localHash,
			finalContent:     w.localContent,
			finalTags:        w.tags(),
			finalSystemTags:  w.withLocalPin(w.st.SystemTags),
			conflictCopy:     true,
			createRemote:     true,
		}
		e.Log("conflict on %q: keeping local copy", w.st.Filename)
		works[id] = cw
	}
}

// filenameTitle returns the title used for the file name: the front matter's
// "title" key when the file declares one, otherwise the body's first non-empty
// line. The front matter is local-only, so a title there is a way to pin a file
// name that the note's text would not produce — which is also what keeps a
// renamed file from being renamed back to its body title.
func (w *work) filenameTitle() string {
	if w.localTitleSet {
		return w.localTitle
	}
	return Title(w.finalContent)
}

// assignFilenames gives every note that stays on disk its file name.
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
		stem := Sanitize(w.filenameTitle())
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

// assignKeys makes sure every note that is going to be written has a server
// key before any file is touched. New notes get a client-chosen key: Simperium
// accepts one at creation time and Save puts it in the URL, so the key can be
// recorded in the file's front matter in the same pass instead of one sync
// later (which also means a push whose response is lost can still be
// recognised, by that key, on the next run).
func (e *Engine) assignKeys(works map[string]*work) {
	for _, id := range sortedIDs(works) {
		w := works[id]
		if w.st == nil {
			w.st = &store.NoteState{ID: id}
		}
		if w.st.ID == "" {
			w.st.ID = id
		}
		switch {
		case w.st.ServerKey != "":
			// Known already.
		case w.remoteExists:
			w.st.ServerKey = w.remote.ID
		case w.createRemote:
			w.st.ServerKey = localHex(16)
			w.st.Version = 0
		}
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
		// Front matter is preserved, but its tags field mirrors the synced tags,
		// its note id records which Simplenote note the file belongs to, and
		// line endings are forced to CRLF. The id is rewritten from the state on
		// every pass, so a hand-edited or copied id is corrected here.
		front := store.SetFrontMatterID(w.localFront, w.noteID())
		front = store.SetFrontMatterTags(front, w.finalTags)
		front = store.SetFrontMatterPinned(front, containsString(w.finalSystemTags, "pinned"))
		front = store.SetFrontMatterField(front, "share_url", w.shareURL())
		front = store.SetFrontMatterField(front, "publish_url", w.publishURL())
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

// checkMassDeletion refuses to propagate a large batch of local deletions.
// A state file that no longer matches the directory (for example after the
// directory was moved or restored from a backup) would otherwise trash most of
// the Simplenote account, one note at a time.
func (e *Engine) checkMassDeletion(works map[string]*work) error {
	tracked, deleting := 0, 0
	for _, w := range works {
		if w.st == nil || w.st.ServerKey == "" || w.st.Deleted {
			continue
		}
		tracked++
		if w.pushedDelete {
			deleting++
		}
	}
	if deleting < massDeleteMin || tracked == 0 || deleting*100/tracked < massDeletePercent {
		return nil
	}
	if e.AllowMassDelete {
		e.Log("warning: propagating %d local deletions out of %d tracked notes (--allow-mass-delete)", deleting, tracked)
		return nil
	}
	return fmt.Errorf("refusing to delete %d of %d tracked notes because their local files are missing; "+
		"if this is intentional re-run with --allow-mass-delete, otherwise restore the files first",
		deleting, tracked)
}

func (e *Engine) applyRemote(works map[string]*work, ids []string, dryRun bool) error {
	now := float64(time.Now().Unix())
	for _, id := range ids {
		w := works[id]
		if !w.createRemote && !w.updateRemote {
			continue
		}
		// In incremental mode a tracked note is not in the index. Fetch it when
		// its content is needed (deletion) or when its server-managed metadata
		// (share/publish URLs and timestamps) must be preserved.
		needMeta := w.updateRemote && !w.createRemote && w.remote == nil
		if (w.needRemoteContent || needMeta) && w.st != nil && w.st.ServerKey != "" {
			if rn, err := e.sn.Get(w.st.ServerKey); err == nil && rn != nil {
				if w.needRemoteContent {
					w.finalContent = rn.Content
					if w.finalTags == nil {
						w.finalTags = rn.Tags
					}
					if w.finalSystemTags == nil {
						w.finalSystemTags = rn.SystemTags
					}
				}
				w.st.ShareURL = rn.ShareURL
				w.st.PublishURL = rn.PublishURL
			}
		}
		unpinFirst := w.finalDeleted && !w.createRemote && containsString(w.finalSystemTags, "pinned")
		note := simperium.Note{
			Content:          w.finalContent,
			Deleted:          w.finalDeleted,
			CreationDate:     now,
			ModificationDate: now,
			Tags:             nonNil(w.finalTags),
			SystemTags:       nonNil(w.finalSystemTags),
			ShareURL:         w.shareURL(),
			PublishURL:       w.publishURL(),
		}
		if w.remoteExists && w.remote.Note.CreationDate > 0 {
			note.CreationDate = w.remote.Note.CreationDate
		}

		if unpinFirst {
			e.Log("unpin  %s (before remote trash)", w.displayName())
		}
		if w.finalDeleted {
			// A local deletion is propagated as a remote trash (deleted=true).
			// Log it as such instead of calling it a push: a mass cleanup then
			// looks like what it is.
			e.Log("trash  %s (remote)", w.displayName())
		} else if w.createRemote {
			e.Log("push   %s (new)", w.displayName())
		} else {
			e.Log("push   %s (update)", w.displayName())
		}
		if dryRun {
			continue
		}

		if w.createRemote {
			// The key was chosen up front (assignKeys) so that the file could
			// already record it. Save falls back to generating one if it is
			// empty, so keep whatever it reports.
			key, _, err := e.sn.Save(w.noteID(), note)
			if err != nil {
				return err
			}
			if w.st == nil {
				w.st = &store.NoteState{ID: w.id}
			}
			w.st.ServerKey = key
			w.st.Version = 0
			e.recordProgress(w)
		} else {
			if err := e.keepOverwrittenBody(w); err != nil {
				return err
			}
			if unpinFirst {
				// Remove the pin first, then trash the note. Two writes mean a
				// failed delete still leaves the unpin recorded.
				unpinned := applyPinned(w.finalSystemTags, false)
				unpinNote := note
				unpinNote.Deleted = false
				unpinNote.SystemTags = nonNil(unpinned)
				if _, _, err := e.sn.Save(w.st.ServerKey, unpinNote); err != nil {
					return err
				}
				note.SystemTags = nonNil(unpinned)
				w.finalSystemTags = unpinned
				// Persist the unpin so an interrupted trash cannot resurrect the
				// note on the next run.
				if e.st.Notes == nil {
					e.st.Notes = map[string]*store.NoteState{}
				}
				w.st.SystemTags = unpinned
				w.st.Deleted = false
				w.st.ModTime = now
				e.st.Notes[w.id] = w.st
				if err := e.st.Save(); err != nil {
					return fmt.Errorf("save state after unpin: %w", err)
				}
			}
			if _, _, err := e.sn.Save(w.st.ServerKey, note); err != nil {
				return err
			}
			e.recordProgress(w)
		}
	}
	return nil
}

// keepOverwrittenBody copies the server version into the local trash before an
// inferred rename overwrites it. See work.inferredRename.
func (e *Engine) keepOverwrittenBody(w *work) error {
	if !w.inferredRename {
		return nil
	}
	old := ""
	switch {
	case w.remoteExists:
		old = w.remote.Note.Content
	case w.st != nil && w.st.ServerKey != "":
		if rn, err := e.sn.Get(w.st.ServerKey); err == nil && rn != nil {
			old = rn.Content
		}
	}
	if old == "" || store.HashContent(old) == store.HashContent(w.finalContent) {
		return nil // pure rename: there is nothing to lose
	}
	name := w.st.Filename
	if name == "" {
		name = w.displayName()
	}
	dest, err := store.WriteToTrash(e.cfg.TrashDir, name, old)
	if err != nil {
		return err
	}
	e.Log("trash  %s (previous version of an inferred rename)", filepath.Base(dest))
	return nil
}

// recordProgress updates the in-memory state after a successful remote write.
// If a later step fails (or the process is interrupted), a subsequent run sees
// the note as tracked instead of treating it as brand new and pushing a
// duplicate. The cursor is only advanced once the whole pass has completed.
func (e *Engine) recordProgress(w *work) {
	if w.st == nil {
		w.st = &store.NoteState{ID: w.id}
	}
	if w.st.ID == "" {
		w.st.ID = w.id
	}
	w.st.BaseHash = store.HashContent(w.finalContent)
	w.st.Tags = w.finalTags
	w.st.SystemTags = w.finalSystemTags
	w.st.ShareURL = w.shareURL()
	w.st.PublishURL = w.publishURL()
	w.st.Deleted = w.finalDeleted
	if w.finalFilename != "" {
		w.st.Filename = w.finalFilename
	}
	w.st.ModTime = float64(time.Now().Unix())
	if e.st.Notes == nil {
		e.st.Notes = map[string]*store.NoteState{}
	}
	e.st.Notes[w.id] = w.st
}

func (e *Engine) rebuildState(works map[string]*work, ids []string, cursor string) {
	next := map[string]*store.NoteState{}
	for _, id := range ids {
		w := works[id]
		if w.finalDeleted {
			// Trashed notes need no bookkeeping: only live notes map a local
			// file to a server key. Keeping tombstones here made every full
			// sync copy the entire Simplenote trash into the state file.
			continue
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
			ModTime:    float64(time.Now().Unix()),
		}
		if w.st != nil {
			ns.ServerKey = w.st.ServerKey
			ns.ShareURL = w.st.ShareURL
			ns.PublishURL = w.st.PublishURL
		}
		if w.remoteExists {
			if ns.ServerKey == "" {
				ns.ServerKey = w.remote.ID
			}
			ns.Version = w.remote.Version
			ns.ModTime = w.remote.Note.ModificationDate
			ns.ShareURL = w.remote.Note.ShareURL
			ns.PublishURL = w.remote.Note.PublishURL
		}
		ns.Filename = w.finalFilename
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

// applyPinned returns systemTags with the "pinned" entry set or cleared. It
// reuses the input when nothing needs to change, keeping the order stable.
func applyPinned(systemTags []string, pinned bool) []string {
	if containsString(systemTags, "pinned") == pinned {
		return systemTags
	}
	out := make([]string, 0, len(systemTags)+1)
	for _, t := range systemTags {
		if t != "pinned" {
			out = append(out, t)
		}
	}
	if pinned {
		out = append(out, "pinned")
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func localHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
