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
		{"", []string{"a", "b"}, "---\ntags: [a, b]\n---\n"},
		{"---\ntitle: x\ntags: [old]\n---\n", []string{"new"}, "---\ntitle: x\ntags: [new]\n---\n"},
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
