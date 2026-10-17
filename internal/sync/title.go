package sync

import (
	"regexp"
	"strings"
)

var (
	// Characters that are unsafe or illegal in file names on common platforms.
	illegalRe = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]`)
	spaceRe   = regexp.MustCompile(`\s+`)
)

// Title derives a note title from its content. Following Simplenote's
// convention the title is the first line, but blank leading lines are skipped
// so that notes starting with an empty line still get a useful name.
func Title(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		// Strip leading markdown heading markers for a cleaner file name.
		line = strings.TrimLeft(line, "#")
		line = strings.TrimSpace(line)
		if line != "" {
			return truncateRunes(line, 60)
		}
	}
	return "untitled"
}

// Sanitize turns an arbitrary title into a safe file name stem.
func Sanitize(name string) string {
	name = illegalRe.ReplaceAllString(name, " ")
	name = spaceRe.ReplaceAllString(name, " ")
	name = strings.Trim(name, " .")
	if name == "" {
		name = "untitled"
	}
	if isReserved(name) {
		name = "_" + name
	}
	return truncateRunes(name, 80)
}

var reserved = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

func isReserved(name string) bool {
	return reserved[strings.ToLower(name)]
}

// FilenameFor builds a unique "<title>.md" name that is not in taken. When the
// title collides, " (2)", " (3)", ... is appended. If suffix is non-empty it
// is inserted before the uniquifier, e.g. "note (conflict).md".
func FilenameFor(content string, suffix string, taken map[string]bool) string {
	stem := Sanitize(Title(content))
	if suffix != "" {
		stem = stem + " " + suffix
	}
	name := stem + ".md"
	for i := 2; taken[name]; i++ {
		name = stem + " (" + itoa(i) + ").md"
	}
	return name
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
