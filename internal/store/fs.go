// Package store handles the local markdown directory and the sync state file.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Normalize converts file/server content to a canonical form for comparison: it
// strips a UTF-8 BOM and normalizes line endings to LF. Front matter is handled
// separately by SplitFrontMatter.
func Normalize(s string) string {
	s = strings.TrimPrefix(s, "\ufeff")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

// HashContent returns a stable hash of normalized content.
func HashContent(s string) string {
	sum := sha256.Sum256([]byte(Normalize(s)))
	return hex.EncodeToString(sum[:])
}

// ReadFile reads a UTF-8 text file and normalizes it.
func ReadFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return Normalize(string(b)), nil
}

// SplitFrontMatter separates an optional front matter block from the note body.
//
// Front matter is a block at the very top of the file whose first line is
// "---" (YAML) or "+++" (TOML) and which is closed by the same marker (or by
// "..." for YAML). It is kept on disk and, apart from its "tags" field, is
// never synced. The returned strings are LF-normalized. When no complete block
// is present, front is empty and body is the whole input.
func SplitFrontMatter(s string) (front, body string) {
	s = Normalize(s)
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "+++\n") {
		return "", s
	}
	open := s[:3]
	lines := strings.SplitAfter(s, "\n")
	for i := 1; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r\n")
		if line == open || (open == "---" && line == "...") {
			return strings.Join(lines[:i+1], ""), strings.Join(lines[i+1:], "")
		}
	}
	return "", s
}

// FrontMatterTags returns the tags declared by a YAML front matter block and
// whether a "tags" key was present. Both inline lists ("tags: [a, b]") and
// block sequences ("tags:\n  - a") are understood. TOML ("+++") front matter
// is left alone: tags are only read from YAML blocks.
func FrontMatterTags(front string) ([]string, bool) {
	if !strings.HasPrefix(front, "---") {
		return nil, false
	}
	inner := frontMatterInner(front)
	for i := 0; i < len(inner); i++ {
		if !strings.HasPrefix(inner[i], "tags:") {
			continue
		}
		rest := strings.TrimSpace(inner[i][len("tags:"):])
		if rest == "" {
			var tags []string
			for j := i + 1; j < len(inner); j++ {
				item := strings.TrimSpace(inner[j])
				if !strings.HasPrefix(item, "-") {
					break
				}
				if item = strings.TrimSpace(item[1:]); item != "" {
					tags = append(tags, unquoteTag(item))
				}
			}
			return tags, true
		}
		rest = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(rest, "["), "]"))
		var tags []string
		for _, part := range splitInlineTags(rest) {
			tags = append(tags, unquoteTag(part))
		}
		return tags, true
	}
	return nil, false
}

// SetFrontMatterTags returns front matter with its "tags" key replaced by tags.
// The key is dropped when tags is empty, and the whole block is dropped when it
// would be left empty. TOML blocks are returned unchanged. The result uses LF
// line endings; EncodeNote turns them into CRLF when writing.
func SetFrontMatterTags(front string, tags []string) string {
	if strings.HasPrefix(front, "+++") {
		return front
	}
	inner := frontMatterInner(front)
	out := make([]string, 0, len(inner)+1)
	skipping := false
	for _, line := range inner {
		if skipping {
			trimmed := strings.TrimSpace(line)
			if line != trimmed || strings.HasPrefix(trimmed, "-") {
				continue // still part of the old tags value
			}
			skipping = false
		}
		if strings.HasPrefix(line, "tags:") {
			skipping = true
			continue
		}
		out = append(out, line)
	}
	if len(tags) > 0 {
		out = append(out, "tags:")
		for _, t := range tags {
			out = append(out, "  - "+quoteTag(t))
		}
	}
	if len(out) == 0 {
		return ""
	}
	return "---\n" + strings.Join(out, "\n") + "\n---\n"
}

// FrontMatterIDKey is the YAML front matter key that records which Simplenote
// note a file belongs to. Like the rest of the front matter it is local-only
// bookkeeping and is never uploaded.
const FrontMatterIDKey = "simplenote_id"

// frontMatterValue returns the scalar value of a top-level key in a YAML front
// matter block, or "" when the block or the key is absent. TOML ("+++") blocks
// are not interpreted, matching FrontMatterTags.
func frontMatterValue(front, key string) string {
	if !strings.HasPrefix(front, "---") {
		return ""
	}
	for _, line := range frontMatterInner(front) {
		if !strings.HasPrefix(line, key+":") {
			continue
		}
		return unquoteTag(strings.TrimSpace(line[len(key)+1:]))
	}
	return ""
}

// FrontMatterID returns the note id declared by a YAML front matter block, or
// "" when there is none.
func FrontMatterID(front string) string { return frontMatterValue(front, FrontMatterIDKey) }

// FrontMatterTitleKey is the YAML front matter key that overrides the file name.
// Like the other front matter keys it is local-only and never uploaded.
const FrontMatterTitleKey = "title"

// FrontMatterTitle returns the title declared by a YAML front matter block. ok
// is false when the key is absent or empty, so callers fall back to the body's
// first line. TOML ("+++") blocks are not interpreted, matching the other
// front matter helpers.
func FrontMatterTitle(front string) (string, bool) {
	title := frontMatterValue(front, FrontMatterTitleKey)
	return title, title != ""
}

// SetFrontMatterID returns front matter with its note id set to id. An empty id
// removes the key, and the whole block is dropped when nothing is left. TOML
// blocks are returned unchanged.
func SetFrontMatterID(front, id string) string {
	if strings.HasPrefix(front, "+++") {
		return front
	}
	inner := frontMatterInner(front)
	out := make([]string, 0, len(inner)+1)
	skipping := false
	for _, line := range inner {
		if skipping {
			trimmed := strings.TrimSpace(line)
			if line != trimmed || strings.HasPrefix(trimmed, "-") {
				continue // still part of the old value
			}
			skipping = false
		}
		if strings.HasPrefix(line, FrontMatterIDKey+":") {
			skipping = true
			continue
		}
		out = append(out, line)
	}
	if id != "" {
		out = append(out, FrontMatterIDKey+": "+quoteTag(id))
	}
	if len(out) == 0 {
		return ""
	}
	return "---\n" + strings.Join(out, "\n") + "\n---\n"
}

// FrontMatterPinnedKey is the YAML front matter key that mirrors Simplenote's
// "pinned" system tag. It is local-only; the pin itself is sent as systemTags.
const FrontMatterPinnedKey = "pinned"

// FrontMatterPinned returns the pin state declared by a YAML front matter
// block. ok reports whether the key was present with a recognised value;
// callers that treat a missing key as false can ignore it. TOML ("+++") blocks
// are not interpreted, matching the other front matter helpers.
func FrontMatterPinned(front string) (pinned, ok bool) {
	if !strings.HasPrefix(front, "---") {
		return false, false
	}
	for _, line := range frontMatterInner(front) {
		if !strings.HasPrefix(line, FrontMatterPinnedKey+":") {
			continue
		}
		v := strings.ToLower(unquoteTag(strings.TrimSpace(line[len(FrontMatterPinnedKey)+1:])))
		switch v {
		case "true", "yes", "on", "1":
			return true, true
		case "false", "no", "off", "0":
			return false, true
		}
		return false, false
	}
	return false, false
}

// SetFrontMatterPinned returns front matter with its "pinned" key set to true,
// or removed when pinned is false. The whole block is dropped when nothing is
// left. TOML blocks are returned unchanged.
func SetFrontMatterPinned(front string, pinned bool) string {
	if strings.HasPrefix(front, "+++") {
		return front
	}
	inner := frontMatterInner(front)
	out := make([]string, 0, len(inner)+1)
	skipping := false
	for _, line := range inner {
		if skipping {
			trimmed := strings.TrimSpace(line)
			if line != trimmed || strings.HasPrefix(trimmed, "-") {
				continue // still part of the old value
			}
			skipping = false
		}
		if strings.HasPrefix(line, FrontMatterPinnedKey+":") {
			skipping = true
			continue
		}
		out = append(out, line)
	}
	if pinned {
		out = append(out, FrontMatterPinnedKey+": true")
	}
	if len(out) == 0 {
		return ""
	}
	return "---\n" + strings.Join(out, "\n") + "\n---\n"
}

// SetFrontMatterField returns front matter with the top-level key set to value,
// removing it when value is empty. The value is quoted when needed, and the
// whole block is dropped when nothing is left. TOML blocks are returned
// unchanged.
func SetFrontMatterField(front, key, value string) string {
	if strings.HasPrefix(front, "+++") {
		return front
	}
	inner := frontMatterInner(front)
	out := make([]string, 0, len(inner)+1)
	skipping := false
	for _, line := range inner {
		if skipping {
			trimmed := strings.TrimSpace(line)
			if line != trimmed || strings.HasPrefix(trimmed, "-") {
				continue // still part of the old value
			}
			skipping = false
		}
		if strings.HasPrefix(line, key+":") {
			skipping = true
			continue
		}
		out = append(out, line)
	}
	if value != "" {
		out = append(out, key+": "+quoteTag(value))
	}
	if len(out) == 0 {
		return ""
	}
	return "---\n" + strings.Join(out, "\n") + "\n---\n"
}

// frontMatterInner returns the lines between the delimiters of front matter.
func frontMatterInner(front string) []string {
	if front == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(front, "\n"), "\n")
	if len(lines) < 2 {
		return nil
	}
	return lines[1 : len(lines)-1]
}

// splitInlineTags splits a YAML flow list such as `a, "b, c"` on commas that
// are not inside single or double quotes. Empty items are dropped.
func splitInlineTags(s string) []string {
	var out []string
	var cur strings.Builder
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			cur.WriteByte(c)
			if quote == '"' && c == '\\' && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
			cur.WriteByte(c)
		case ',':
			if part := strings.TrimSpace(cur.String()); part != "" {
				out = append(out, part)
			}
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if part := strings.TrimSpace(cur.String()); part != "" {
		out = append(out, part)
	}
	return out
}

func quoteTag(s string) string {
	if s == "" || strings.TrimSpace(s) != s || strings.ContainsAny(s, ",[]{}#:\"'\\") {
		b, _ := json.Marshal(s)
		return string(b)
	}
	return s
}

func unquoteTag(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var out string
		if err := json.Unmarshal([]byte(s), &out); err == nil {
			return out
		}
	} else if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return s[1 : len(s)-1]
	}
	return s
}

// EncodeNote renders a note for its local markdown file: any front matter is
// kept as-is and every line ending is written as CRLF.
func EncodeNote(front, body string) string {
	s := strings.ReplaceAll(front+body, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

// WriteNoteAtomic writes a markdown note (front matter plus body) to path via a
// temp file + rename, using CRLF line endings.
func WriteNoteAtomic(path, front, body string) error {
	return writeAtomic(path, EncodeNote(front, body))
}

// writeAtomic writes content to path via a temp file + rename.
func writeAtomic(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// ListMarkdown returns the relative names of markdown files directly inside
// dir. Hidden entries (including the .snsync directory) and subdirectories are
// skipped.
func ListMarkdown(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if !strings.EqualFold(filepath.Ext(name), ".md") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// WriteToTrash writes content into trashDir under name (uniquified like
// MoveToTrash). It is used to keep a version that is about to be overwritten.
func WriteToTrash(trashDir, name, content string) (string, error) {
	if name == "" {
		name = "note.md"
	}
	if err := os.MkdirAll(trashDir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(trashDir, name)
	for i := 2; ; i++ {
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			break
		}
		dest = filepath.Join(trashDir, fmt.Sprintf("%s.%d", name, i))
	}
	if err := writeAtomic(dest, content); err != nil {
		return "", err
	}
	return dest, nil
}

// MoveToTrash moves path into trashDir, uniquifying the name if needed.
func MoveToTrash(path, trashDir string) (string, error) {
	if err := os.MkdirAll(trashDir, 0o755); err != nil {
		return "", err
	}
	base := filepath.Base(path)
	dest := filepath.Join(trashDir, base)
	for i := 2; ; i++ {
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			break
		}
		dest = filepath.Join(trashDir, fmt.Sprintf("%s.%d", base, i))
	}
	if err := os.Rename(path, dest); err != nil {
		// Fall back to copy+remove when rename crosses devices.
		if err2 := copyFile(path, dest); err2 != nil {
			return "", err
		}
		_ = os.Remove(path)
	}
	return dest, nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

// Exists reports whether a path exists.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// EnsureDir creates dir if it does not exist.
func EnsureDir(dir string) error {
	if dir == "" {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}
