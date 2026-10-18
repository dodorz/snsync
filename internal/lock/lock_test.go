package lock

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestAcquireIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".snsync", "state.json.lock")

	first, err := Acquire(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	t.Cleanup(func() { _ = first.Release() })

	if _, err := Acquire(path); !errors.Is(err, ErrHeld) {
		t.Fatalf("second acquire: got %v, want ErrHeld", err)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	third, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := third.Release(); err != nil {
		t.Fatalf("second release: %v", err)
	}
}
