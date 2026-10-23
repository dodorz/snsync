package sync

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func writeRenameFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPlanLocalRenames(t *testing.T) {
	dir := t.TempDir()
	writeRenameFile(t, dir, "a.md", "---\ntitle: Zeta\n---\nalpha")
	writeRenameFile(t, dir, "b.md", "Beta body")
	writeRenameFile(t, dir, "c.md", "---\ntitle: Alpha\n---\ngamma")
	// Already follows the rule: Sanitize(title)=="Alpha" and stemOf strips " (2)".
	writeRenameFile(t, dir, "Alpha (2).md", "---\ntitle: Alpha\n---\nkept")

	got, err := PlanLocalRenames(dir)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].From < got[j].From })
	want := []Rename{
		{From: "a.md", To: "Zeta.md"},
		{From: "b.md", To: "Beta body.md"},
		{From: "c.md", To: "Alpha.md"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PlanLocalRenames = %v, want %v", got, want)
	}
}

func TestApplyLocalRenamesHandlesSwap(t *testing.T) {
	dir := t.TempDir()
	writeRenameFile(t, dir, "Foo.md", "---\ntitle: Bar\n---\none")
	writeRenameFile(t, dir, "Bar.md", "---\ntitle: Foo\n---\ntwo")

	plan, err := PlanLocalRenames(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 2 {
		t.Fatalf("expected 2 renames, got %v", plan)
	}
	if err := ApplyLocalRenames(dir, plan); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"Foo.md": "two", "Bar.md": "one"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(string(b), body) {
			t.Fatalf("%s = %q, want body %q", name, b, body)
		}
	}
	again, err := PlanLocalRenames(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("second plan is not empty: %v", again)
	}
}

func TestPlanLocalRenamesUniquifies(t *testing.T) {
	dir := t.TempDir()
	writeRenameFile(t, dir, "x.md", "Same")
	writeRenameFile(t, dir, "y.md", "Same")

	plan, err := PlanLocalRenames(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyLocalRenames(dir, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Same.md")); err != nil {
		t.Fatalf("Same.md missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Same (2).md")); err != nil {
		t.Fatalf("Same (2).md missing: %v", err)
	}
	again, err := PlanLocalRenames(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("second plan is not empty: %v", again)
	}
}
