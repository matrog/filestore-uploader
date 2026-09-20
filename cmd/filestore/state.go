package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// StateEntry records one file that was successfully uploaded.
type StateEntry struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Code  string `json:"code"`
	Link  string `json:"link"`
	When  string `json:"when"`
	FldID string `json:"fld_id,omitempty"`
}

// StateStore makes an interrupted transfer resumable: every success is written
// and fsynced immediately, so it survives a crash or a Ctrl-C.
type StateStore struct {
	path string
	mu   sync.Mutex
	f    *os.File
	done map[string]StateEntry
}

func OpenStateStore(path string) (*StateStore, error) {
	s := &StateStore{path: path, done: make(map[string]StateEntry)}

	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			var e StateEntry
			if json.Unmarshal(line, &e) == nil && e.Path != "" && e.Code != "" {
				s.done[e.Path] = e
			}
		}
		f.Close()
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("state file %s: %w", path, err)
	}
	s.f = f
	return s, nil
}

// Done reports the record if that file was already uploaded at the same size.
// If the size changed, the file must be uploaded again.
func (s *StateStore) Done(path string, size int64) (StateEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.done[path]
	if !ok || e.Size != size {
		return StateEntry{}, false
	}
	return e, true
}

func (s *StateStore) Add(e StateEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done[e.Path] = e
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := s.f.Write(append(b, '\n')); err != nil {
		return err
	}
	// Immediate fsync: without it a crash would lose the most recent lines.
	return s.f.Sync()
}

func (s *StateStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.done)
}

func (s *StateStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	return s.f.Close()
}

// LinkLog appends links as they come in, instead of only at the end.
type LinkLog struct {
	mu sync.Mutex
	f  *os.File
}

func OpenLinkLog(path string) (*LinkLog, error) {
	if path == "" {
		return nil, nil
	}
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &LinkLog{f: f}, nil
}

func (l *LinkLog) Add(name, link string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.f, "%s\t%s\n", link, name)
	l.f.Sync()
}

func (l *LinkLog) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.f.Close()
}
