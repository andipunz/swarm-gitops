// Package state persists the little the controller needs across restarts.
// Everything else (what runs, which commit) lives in Swarm service labels.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Data is the persisted state.
type Data struct {
	Processed      map[string]string `json:"processed"`       // stack -> last handled commit
	Paused         map[string]bool   `json:"paused"`          // stacks excluded from deploys/updates
	Adopted        map[string]bool   `json:"adopted"`         // unmanaged stacks the controller may take over
	Force          map[string]bool   `json:"force"`           // redeploy requested via CLI
	PendingRemoval map[string]int    `json:"pending_removal"` // stack -> consecutive scans wanting removal
	Orphaned       map[string]string `json:"orphaned"`        // stack -> reason it is kept
	PruneBlocked   []string          `json:"prune_blocked"`   // removals waiting for approval
	PruneApproved  bool              `json:"prune_approved"`
	LastScan       time.Time         `json:"last_scan"`
	LastScanError  string            `json:"last_scan_error"`
	LastImageCheck time.Time         `json:"last_image_check"`
	// BindHistory: bind rule root -> sources that were ever mounted writable.
	BindHistory map[string][]string `json:"bind_history"`
}

// Store is a mutex-protected, file-backed state.
type Store struct {
	mu   sync.Mutex
	file string
	d    Data
}

// Open loads the state file (missing file = empty state).
func Open(file string) (*Store, error) {
	s := &Store{file: file}
	data, err := os.ReadFile(file)
	if err == nil {
		if err := json.Unmarshal(data, &s.d); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	s.init()
	return s, nil
}

func (s *Store) init() {
	if s.d.Processed == nil {
		s.d.Processed = map[string]string{}
	}
	if s.d.Paused == nil {
		s.d.Paused = map[string]bool{}
	}
	if s.d.Adopted == nil {
		s.d.Adopted = map[string]bool{}
	}
	if s.d.Force == nil {
		s.d.Force = map[string]bool{}
	}
	if s.d.PendingRemoval == nil {
		s.d.PendingRemoval = map[string]int{}
	}
	if s.d.BindHistory == nil {
		s.d.BindHistory = map[string][]string{}
	}
	if s.d.Orphaned == nil {
		s.d.Orphaned = map[string]string{}
	}
}

// Update mutates the state under lock and saves it atomically.
func (s *Store) Update(fn func(d *Data)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.d)
	data, err := json.MarshalIndent(s.d, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.file), 0o700); err != nil {
		return err
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.file)
}

// View reads the state under lock.
func (s *Store) View(fn func(d *Data)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.d)
}
