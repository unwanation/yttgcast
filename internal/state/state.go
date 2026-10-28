// Package state persists which videos have already been processed, so restarts
// never re-upload a track. It is a small JSON file written atomically.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry describes a processed (or given-up) video.
type Entry struct {
	Title       string    `json:"title"`
	ProcessedAt time.Time `json:"processed_at"`
	MessageID   int       `json:"message_id,omitempty"`
	DocumentID  int64     `json:"document_id,omitempty"`
	Skipped     bool      `json:"skipped,omitempty"` // marked as seen without uploading
	Reason      string    `json:"reason,omitempty"`
}

// Failure counts attempts for a video that has not succeeded yet.
type Failure struct {
	Attempts    int       `json:"attempts"`
	LastError   string    `json:"last_error"`
	LastAttempt time.Time `json:"last_attempt"`
}

type file struct {
	Version     int                `json:"version"`
	Initialized bool               `json:"initialized"`
	Processed   map[string]Entry   `json:"processed"`
	Failures    map[string]Failure `json:"failures,omitempty"`
	LastPollAt  time.Time          `json:"last_poll_at,omitempty"`
}

// Store is a mutex-protected, file-backed state store.
type Store struct {
	path string
	mu   sync.Mutex
	f    file
}

// Open loads the store from path (a missing file yields an empty store).
func Open(path string) (*Store, error) {
	s := &Store{path: path, f: file{Version: 1, Processed: map[string]Entry{}, Failures: map[string]Failure{}}}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(b, &s.f); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	if s.f.Processed == nil {
		s.f.Processed = map[string]Entry{}
	}
	if s.f.Failures == nil {
		s.f.Failures = map[string]Failure{}
	}
	return s, nil
}

// Initialized reports whether the first-run bootstrap already happened.
func (s *Store) Initialized() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Initialized
}

// SetInitialized marks the bootstrap as done.
func (s *Store) SetInitialized() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.f.Initialized = true
	return s.saveLocked()
}

// Has reports whether the video was already processed or skipped.
func (s *Store) Has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.f.Processed[id]
	return ok
}

// Count returns the number of processed entries.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.f.Processed)
}

// Mark records a video as processed and clears any failure counter.
func (s *Store) Mark(id string, e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.ProcessedAt.IsZero() {
		e.ProcessedAt = time.Now().UTC()
	}
	s.f.Processed[id] = e
	delete(s.f.Failures, id)
	return s.saveLocked()
}

// RecordFailure increments the attempt counter and returns the new count.
func (s *Store) RecordFailure(id string, err error) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.f.Failures[id]
	f.Attempts++
	f.LastError = err.Error()
	f.LastAttempt = time.Now().UTC()
	s.f.Failures[id] = f
	return f.Attempts, s.saveLocked()
}

// Attempts returns how many times the video has failed so far.
func (s *Store) Attempts(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Failures[id].Attempts
}

// TouchPoll records the time of the last successful feed poll.
func (s *Store) TouchPoll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.f.LastPollAt = time.Now().UTC()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(&s.f, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("commit state: %w", err)
	}
	return nil
}
