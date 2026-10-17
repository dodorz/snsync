// Package store handles the local markdown directory and the sync state file.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Normalize converts file/server content to a canonical form: it strips a
// UTF-8 BOM and normalizes line endings to LF. Nothing else is changed, so the
// bytes in the local file stay faithful to what is stored on the server.
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

// WriteFileAtomic writes content to path via a temp file + rename.
func WriteFileAtomic(path, content string) error {
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
