package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/dodorz/snsync/internal/store"
)

// Rename is a planned local file rename from one base name to another.
type Rename struct {
	From string
	To   string
}

// PlanLocalRenames computes the names the local markdown files should have,
// using the same rule as sync: the front matter "title" when present, otherwise
// the first non-empty line of the body. A name that already follows the rule
// (including a " (2)" uniquifier) is left alone.
//
// Existing entries that are not part of the plan stay reserved, so a rename
// never overwrites an unrelated file or directory.
func PlanLocalRenames(dir string) ([]Rename, error) {
	names, err := store.ListMarkdown(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sort.Strings(names)

	type item struct {
		name  string
		stem  string
		match bool
	}
	items := make([]item, 0, len(names))
	for _, name := range names {
		full, err := store.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		front, body := store.SplitFrontMatter(full)
		title, ok := store.FrontMatterTitle(front)
		if !ok {
			title = Title(body)
		}
		stem := Sanitize(title)
		items = append(items, item{name: name, stem: stem, match: stemOf(name) == stem})
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	for _, e := range entries {
		taken[e.Name()] = true
	}
	// Names that are about to be vacated can be reused by other files, which
	// makes swaps (a<->b) and chains (a->b, b->c) come out right.
	for _, it := range items {
		if !it.match {
			delete(taken, it.name)
		}
	}

	used := map[string]bool{}
	var out []Rename
	for _, it := range items {
		if it.match {
			continue
		}
		name := it.stem + ".md"
		for i := 2; taken[name] || used[name]; i++ {
			name = it.stem + " (" + itoa(i) + ").md"
		}
		used[name] = true
		out = append(out, Rename{From: it.name, To: name})
	}
	return out, nil
}

// ApplyLocalRenames performs the planned renames. Sources are moved to
// temporary names first, so swaps and chains do not clobber each other.
func ApplyLocalRenames(dir string, renames []Rename) error {
	if len(renames) == 0 {
		return nil
	}
	tmps := make(map[string]string, len(renames))
	for i, r := range renames {
		tmp := filepath.Join(dir, fmt.Sprintf(".snsync-rename-%d-%s.tmp", i, localHex(4)))
		if err := os.Rename(filepath.Join(dir, r.From), tmp); err != nil {
			for from, t := range tmps {
				_ = os.Rename(t, filepath.Join(dir, from))
			}
			return err
		}
		tmps[r.From] = tmp
	}
	for _, r := range renames {
		if err := os.Rename(tmps[r.From], filepath.Join(dir, r.To)); err != nil {
			return err
		}
	}
	return nil
}
