package store

import (
	"reflect"
	"testing"
)

func TestSplitFrontMatter(t *testing.T) {
	cases := []struct {
		in          string
		front, body string
	}{
		{"", "", ""},
		{"no front matter here", "", "no front matter here"},
		{"---\ntitle: x\n---\nbody", "---\ntitle: x\n---\n", "body"},
		{"---\n---\n", "---\n---\n", ""},
		{"---\ntitle: x\n...\nbody", "---\ntitle: x\n...\n", "body"},
		{"+++\ntitle = \"x\"\n+++\nbody", "+++\ntitle = \"x\"\n+++\n", "body"},
		// CRLF input is normalized before parsing.
		{"---\r\ntitle: x\r\n---\r\nbody", "---\ntitle: x\n---\n", "body"},
		// An unterminated block is not front matter.
		{"---\ntitle: x\nbody", "", "---\ntitle: x\nbody"},
		{"+++\ntitle = \"x\"\nbody", "", "+++\ntitle = \"x\"\nbody"},
		// Only the very first line can open a block.
		{"\n---\n---\nbody", "", "\n---\n---\nbody"},
	}
	for _, tc := range cases {
		front, body := SplitFrontMatter(tc.in)
		if front != tc.front || body != tc.body {
			t.Errorf("SplitFrontMatter(%q) = (%q, %q), want (%q, %q)",
				tc.in, front, body, tc.front, tc.body)
		}
	}
}

func TestFrontMatterTags(t *testing.T) {
	cases := []struct {
		front string
		tags  []string
		set   bool
	}{
		{"", nil, false},
		{"---\ntitle: x\n---\n", nil, false},
		{"---\ntags: [a, b]\n---\n", []string{"a", "b"}, true},
		{"---\ntags: [a, \"b, c\"]\n---\n", []string{"a", "b, c"}, true},
		{"---\ntags:\n  - a\n  - b\n---\n", []string{"a", "b"}, true},
		{"---\ntags: []\n---\n", nil, true},
		{"---\ntags:\n---\n", nil, true},
		{"+++\ntags = [\"a\"]\n+++\n", nil, false},
	}
	for _, tc := range cases {
		tags, set := FrontMatterTags(tc.front)
		if set != tc.set || !reflect.DeepEqual(tags, tc.tags) {
			t.Errorf("FrontMatterTags(%q) = (%v, %v), want (%v, %v)",
				tc.front, tags, set, tc.tags, tc.set)
		}
	}
}

func TestSetFrontMatterTags(t *testing.T) {
	cases := []struct {
		front string
		tags  []string
		want  string
	}{
		{"", []string{"a", "b"}, "---\ntags:\n  - a\n  - b\n---\n"},
		{"---\ntitle: x\ntags: [old]\n---\n", []string{"new"}, "---\ntitle: x\ntags:\n  - new\n---\n"},
		{"---\ntitle: x\ntags: [a]\n---\n", nil, "---\ntitle: x\n---\n"},
		// Dropping the only key drops the whole block.
		{"---\ntags:\n  - a\n---\n", nil, ""},
		{"", nil, ""},
	}
	for _, tc := range cases {
		if got := SetFrontMatterTags(tc.front, tc.tags); got != tc.want {
			t.Errorf("SetFrontMatterTags(%q, %v) = %q, want %q", tc.front, tc.tags, got, tc.want)
		}
	}
}

func TestFrontMatterID(t *testing.T) {
	cases := []struct {
		front string
		id    string
	}{
		{"", ""},
		{"---\ntitle: x\n---\n", ""},
		{"---\nsimplenote_id: abc\n---\n", "abc"},
		{"---\nsimplenote_id: \"a b\"\n---\n", "a b"},
		{"---\nsimplenote_id:\n---\n", ""},
		// TOML blocks are not interpreted, like FrontMatterTags.
		{"+++\nsimplenote_id = \"abc\"\n+++\n", ""},
	}
	for _, tc := range cases {
		if got := FrontMatterID(tc.front); got != tc.id {
			t.Errorf("FrontMatterID(%q) = %q, want %q", tc.front, got, tc.id)
		}
	}
}

func TestFrontMatterTitle(t *testing.T) {
	cases := []struct {
		front string
		title string
		ok    bool
	}{
		{"", "", false},
		{"---\ntags: [a]\n---\n", "", false},
		{"---\ntitle: My Note\n---\n", "My Note", true},
		{"---\ntitle: \"My: Note\"\n---\n", "My: Note", true},
		{"---\ntitle:\n---\n", "", false},
		// TOML blocks are not interpreted, like the other helpers.
		{"+++\ntitle = \"x\"\n+++\n", "", false},
	}
	for _, tc := range cases {
		title, ok := FrontMatterTitle(tc.front)
		if title != tc.title || ok != tc.ok {
			t.Errorf("FrontMatterTitle(%q) = (%q, %v), want (%q, %v)",
				tc.front, title, ok, tc.title, tc.ok)
		}
	}
}

func TestSetFrontMatterID(t *testing.T) {
	cases := []struct {
		front string
		id    string
		want  string
	}{
		{"", "abc", "---\nsimplenote_id: abc\n---\n"},
		{"---\ntitle: x\n---\n", "abc", "---\ntitle: x\nsimplenote_id: abc\n---\n"},
		{"---\ntitle: x\nsimplenote_id: old\n---\n", "new", "---\ntitle: x\nsimplenote_id: new\n---\n"},
		// Removing the only key drops the whole block.
		{"---\nsimplenote_id: old\n---\n", "", ""},
		{"", "", ""},
		{"+++\nx = 1\n+++\n", "abc", "+++\nx = 1\n+++\n"},
	}
	for _, tc := range cases {
		if got := SetFrontMatterID(tc.front, tc.id); got != tc.want {
			t.Errorf("SetFrontMatterID(%q, %q) = %q, want %q", tc.front, tc.id, got, tc.want)
		}
	}
}

// applyLocal sets the id first and the tags second; doing it twice must produce
// the same bytes, otherwise every sync would rewrite every file.
func TestFrontMatterIDComposesWithTags(t *testing.T) {
	build := func(front string) string {
		front = SetFrontMatterID(front, "abc")
		return SetFrontMatterTags(front, []string{"a", "b"})
	}
	got := build("---\ntitle: x\n---\n")
	want := "---\ntitle: x\nsimplenote_id: abc\ntags:\n  - a\n  - b\n---\n"
	if got != want {
		t.Fatalf("first pass = %q, want %q", got, want)
	}
	if again := build(got); again != got {
		t.Fatalf("second pass is not stable: %q -> %q", got, again)
	}
}

func TestFrontMatterPinned(t *testing.T) {
	cases := []struct {
		front  string
		pinned bool
		ok     bool
	}{
		{"", false, false},
		{"---\ntitle: x\n---\n", false, false},
		{"---\npinned: true\n---\n", true, true},
		{"---\npinned: false\n---\n", false, true},
		{"---\npinned: yes\n---\n", true, true},
		{"---\npinned: \"true\"\n---\n", true, true},
		{"---\npinned:\n---\n", false, false},
		// TOML blocks are not interpreted, like the other helpers.
		{"+++\npinned = true\n+++\n", false, false},
	}
	for _, tc := range cases {
		pinned, ok := FrontMatterPinned(tc.front)
		if pinned != tc.pinned || ok != tc.ok {
			t.Errorf("FrontMatterPinned(%q) = (%v, %v), want (%v, %v)",
				tc.front, pinned, ok, tc.pinned, tc.ok)
		}
	}
}

func TestSetFrontMatterPinned(t *testing.T) {
	cases := []struct {
		front  string
		pinned bool
		want   string
	}{
		{"", true, "---\npinned: true\n---\n"},
		{"---\ntitle: x\n---\n", true, "---\ntitle: x\npinned: true\n---\n"},
		{"---\ntitle: x\npinned: true\n---\n", false, "---\ntitle: x\n---\n"},
		// Removing the only key drops the whole block.
		{"---\npinned: true\n---\n", false, ""},
		{"", false, ""},
		{"+++\nx = 1\n+++\n", true, "+++\nx = 1\n+++\n"},
	}
	for _, tc := range cases {
		if got := SetFrontMatterPinned(tc.front, tc.pinned); got != tc.want {
			t.Errorf("SetFrontMatterPinned(%q, %v) = %q, want %q", tc.front, tc.pinned, got, tc.want)
		}
	}
}

// applyLocal runs id, tags and pinned in that order; doing it twice must be
// stable, otherwise every sync would rewrite every file.
func TestFrontMatterPinnedComposes(t *testing.T) {
	build := func(front string) string {
		front = SetFrontMatterID(front, "abc")
		front = SetFrontMatterTags(front, []string{"a"})
		return SetFrontMatterPinned(front, true)
	}
	got := build("---\ntitle: x\n---\n")
	want := "---\ntitle: x\nsimplenote_id: abc\ntags:\n  - a\npinned: true\n---\n"
	if got != want {
		t.Fatalf("first pass = %q, want %q", got, want)
	}
	if again := build(got); again != got {
		t.Fatalf("second pass is not stable: %q -> %q", got, again)
	}
}

func TestEncodeNoteUsesCRLF(t *testing.T) {
	got := EncodeNote("---\ntitle: x\n---\n", "a\nb")
	want := "---\r\ntitle: x\r\n---\r\na\r\nb"
	if got != want {
		t.Fatalf("EncodeNote() = %q, want %q", got, want)
	}
	// Already-CRLF input must not be doubled up.
	if again := EncodeNote("", "a\r\nb"); again != "a\r\nb" {
		t.Fatalf("EncodeNote() = %q, want %q", again, "a\r\nb")
	}
}

func TestNormalizeStripsCR(t *testing.T) {
	if got := Normalize("a\r\nb\rc"); got != "a\nb\nc" {
		t.Fatalf("Normalize() = %q", got)
	}
}
