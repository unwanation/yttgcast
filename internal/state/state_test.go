package state

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Initialized() || s.Has("a") {
		t.Fatal("fresh store should be empty")
	}
	if err := s.Mark("a", Entry{Title: "A", MessageID: 7}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.RecordFailure("b", errors.New("boom")); n != 1 {
		t.Fatalf("attempts = %d", n)
	}
	if err := s.SetInitialized(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Initialized() || !s2.Has("a") || s2.Has("b") || s2.Attempts("b") != 1 || s2.Count() != 1 {
		t.Fatalf("state not persisted correctly: %+v", s2.f)
	}
	if err := s2.Mark("b", Entry{}); err != nil {
		t.Fatal(err)
	}
	if s2.Attempts("b") != 0 {
		t.Fatal("Mark should clear failures")
	}
}
