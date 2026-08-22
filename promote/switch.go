// Package promote implements FACTORY.md §6: both promotion modes, the five
// conditions autonomous promotion must satisfy, and the kill switch.
//
// Two design constraints from the spec shape everything here.
//
// **Both modes must work and neither is privileged in the code** (§6). So there
// is one evaluation path. Mode is an input to it, checked alongside the other
// conditions, not a branch that selects between two implementations. A reader
// looking for "the autonomous code path" will not find one, and that is the
// point: an autonomous-only path is a path the gated majority never exercises.
//
// **The conditions are enforced in the binary, not in documentation** (§6.3). All
// five are function calls with tests, and a refusal names which one failed.
package promote

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Mode is how promotion happens.
type Mode string

// The two modes. Gated is the default, everywhere, always: a cell with no
// configuration promotes nothing on its own.
const (
	ModeGated      Mode = "gated"
	ModeAutonomous Mode = "autonomous"
)

// ParseMode validates a mode string.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeGated:
		return ModeGated, nil
	case ModeAutonomous:
		return ModeAutonomous, nil
	}
	return "", fmt.Errorf("promote: unknown mode %q (want %q or %q)", s, ModeGated, ModeAutonomous)
}

// Switch holds a cell's live promotion mode and its per-path autonomous
// enablement.
//
// It is backed by a file and **re-read on every decision**, which is what makes
// `varvig-factory promote --mode gated` take effect immediately across the cell
// without a restart (§6.5). No signal, no socket, no IPC: the CLI writes the
// file and the running loop sees it on its next promotion decision.
//
// Re-reading per decision rather than caching by mtime is deliberate. A
// promotion decision happens at most a few times a minute; a file read is
// nothing; and an mtime cache introduces a window in which the kill switch has
// been thrown and the cell is still promoting. That window is the whole thing
// the kill switch exists to eliminate.
type Switch struct {
	mu sync.Mutex
	// path is where the state lives. An empty path keeps the switch in memory,
	// which is what a one-shot run and the tests want.
	path  string
	state switchState
}

type switchState struct {
	Mode Mode `json:"mode"`
	// AutonomousPaths are the path scopes autonomous promotion is enabled for.
	// Autonomous mode is per-path, never global (§6.3 condition 4), so this is a
	// list and there is deliberately no "all" value it can hold.
	AutonomousPaths []string `json:"autonomous_paths,omitempty"`
}

// NewSwitch opens or creates a switch at path. A missing file means gated, which
// is the safe default and the documented one.
func NewSwitch(path string) (*Switch, error) {
	s := &Switch{path: path, state: switchState{Mode: ModeGated}}
	if path == "" {
		return s, nil
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads the state file. Called with the lock held, or before publication.
func (s *Switch) load() error {
	if s.path == "" {
		return nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.state = switchState{Mode: ModeGated}
			return nil
		}
		return err
	}
	var st switchState
	if err := json.Unmarshal(raw, &st); err != nil {
		// An unreadable switch must not read as autonomous, and must not read as
		// gated either — silently falling back would hide a corrupted kill
		// switch. Fail loudly.
		return fmt.Errorf("promote: switch state at %s is unreadable: %w", s.path, err)
	}
	if st.Mode == "" {
		st.Mode = ModeGated
	}
	if _, err := ParseMode(string(st.Mode)); err != nil {
		return fmt.Errorf("promote: switch state at %s: %w", s.path, err)
	}
	s.state = st
	return nil
}

func (s *Switch) save() error {
	if s.path == "" {
		return nil
	}
	raw, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Mode returns the current mode, re-read from disk.
func (s *Switch) Mode() (Mode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return ModeGated, err
	}
	return s.state.Mode, nil
}

// SetMode changes the mode. It takes effect on the next promotion decision made
// anywhere in the cell, including in an already-running loop.
func (s *Switch) SetMode(m Mode) error {
	if _, err := ParseMode(string(m)); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	s.state.Mode = m
	return s.save()
}

// EnableAutonomous adds a path scope to the autonomous set. It does not change
// the mode: enabling a path and switching the cell to autonomous are two
// deliberate acts, so that flipping the mode back to gated does not silently
// discard which paths an operator had reviewed.
func (s *Switch) EnableAutonomous(path string) error {
	clean := cleanScope(path)
	if clean == "" {
		// A global autonomous scope is the one thing this must not accept: a
		// factory key with promote at / is the same risk as an unattended root
		// credential (§6.1), and per-path enablement is condition 4.
		return fmt.Errorf("promote: refusing to enable autonomous promotion at %q; autonomous mode is per-path, never global (§6.3 condition 4)", path)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	for _, p := range s.state.AutonomousPaths {
		if p == clean {
			return nil
		}
	}
	s.state.AutonomousPaths = append(s.state.AutonomousPaths, clean)
	sort.Strings(s.state.AutonomousPaths)
	return s.save()
}

// DisableAutonomous removes a path scope.
func (s *Switch) DisableAutonomous(path string) error {
	clean := cleanScope(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	out := s.state.AutonomousPaths[:0]
	for _, p := range s.state.AutonomousPaths {
		if p != clean {
			out = append(out, p)
		}
	}
	s.state.AutonomousPaths = append([]string(nil), out...)
	return s.save()
}

// AutonomousPaths lists the enabled scopes.
func (s *Switch) AutonomousPaths() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil, err
	}
	return append([]string(nil), s.state.AutonomousPaths...), nil
}

// EnabledFor reports whether autonomous promotion is enabled for a path, and
// returns the enabling scope so a decision can say which entry permitted it.
//
// The match is on whole path segments, the same rule the trust store uses, so
// an enabled "src/generated/" does not cover "src/generated-secrets/".
func (s *Switch) EnabledFor(path string) (string, bool, error) {
	paths, err := s.AutonomousPaths()
	if err != nil {
		return "", false, err
	}
	target := cleanScope(path)
	if target == "" {
		return "", false, nil
	}
	// Longest match wins, so the most specific enabled scope is the one
	// reported.
	best := ""
	for _, p := range paths {
		if segmentPrefix(p, target) && len(p) > len(best) {
			best = p
		}
	}
	return best, best != "", nil
}

// cleanScope normalizes a path scope, returning "" for anything that means
// "everything" — the value this package refuses to accept as an autonomous
// scope.
func cleanScope(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, "/")
	if p == "" || p == "." || p == "*" {
		return ""
	}
	return p + "/"
}

// segmentPrefix reports whether scope covers path, matching whole segments.
func segmentPrefix(scope, path string) bool {
	if !strings.HasSuffix(scope, "/") {
		scope += "/"
	}
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	return strings.HasPrefix(path, scope)
}
