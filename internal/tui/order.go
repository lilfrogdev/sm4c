package tui

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// sessionOrderPath returns the default path for the session order file.
// Falls back gracefully when os.UserConfigDir fails.
func sessionOrderPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		// Unusual (no HOME set, sandboxed env). Use ~/.config as fallback.
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "sm4c", "session_order")
}

// loadSessionOrder reads the order file at path and returns the list of
// session names in stored order. Returns nil (not an error) when the file
// does not exist — that just means no order has been saved yet.
func loadSessionOrder(path string) []string {
	f, err := os.Open(path) // #nosec G304 -- path is constructed by sm4c, not user input
	if err != nil {
		return nil
	}
	defer f.Close() //nolint:errcheck
	var names []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if name := strings.TrimSpace(sc.Text()); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// saveSessionOrder writes names to path, one per line, creating the parent
// directory if needed. Errors are non-fatal — the caller logs or ignores them
// so a write failure (read-only FS, no HOME, etc.) doesn't crash the TUI.
func saveSessionOrder(path string, names []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("session order: mkdir %s: %w", filepath.Dir(path), err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), "session_order.*.tmp")
	if err != nil {
		return fmt.Errorf("session order: create temp: %w", err)
	}
	tmpPath := f.Name()
	w := bufio.NewWriter(f)
	for _, name := range names {
		// tmux doesn't enforce unique window names; two sessions sharing a
		// name will both appear at adjacent positions, which is harmless.
		if _, err := fmt.Fprintln(w, name); err != nil {
			f.Close() //nolint:errcheck
			os.Remove(tmpPath)
			return fmt.Errorf("session order: write: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		f.Close() //nolint:errcheck
		os.Remove(tmpPath)
		return fmt.Errorf("session order: flush: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("session order: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("session order: rename to %s: %w", path, err)
	}
	return nil
}

// applySessionOrder returns sessions sorted according to order (a list of
// session names). Sessions whose names appear in order are placed first, in
// that order. Sessions not listed append at the end in their original relative
// order. Sessions listed in order that are absent from sessions are silently
// skipped — they no longer exist.
//
// An empty or nil order returns sessions unchanged.
func applySessionOrder(sessions []Session, order []string) []Session {
	if len(order) == 0 {
		return sessions
	}
	pos := make(map[string]int, len(order))
	for i, name := range order {
		// First occurrence wins; duplicates in the order file are ignored.
		if _, dup := pos[name]; !dup {
			pos[name] = i
		}
	}
	out := make([]Session, len(sessions))
	copy(out, sessions)
	sort.SliceStable(out, func(i, j int) bool {
		pi, iKnown := pos[out[i].Name]
		pj, jKnown := pos[out[j].Name]
		switch {
		case iKnown && jKnown:
			return pi < pj
		case iKnown:
			return true // known before unknown
		case jKnown:
			return false
		default:
			return false // preserve original relative order for both unknown
		}
	})
	return out
}

// moveSession returns a new slice with the session at from moved to to.
// The relative order of all other sessions is preserved. Both indices must
// be in [0, len(sessions)).
func moveSession(sessions []Session, from, to int) []Session {
	if from == to || from < 0 || to < 0 || from >= len(sessions) || to >= len(sessions) {
		return sessions
	}
	out := make([]Session, len(sessions))
	copy(out, sessions)
	s := out[from]
	if from < to {
		copy(out[from:], out[from+1:to+1])
	} else {
		copy(out[to+1:], out[to:from])
	}
	out[to] = s
	return out
}

// sessionNames extracts the Name field from each session in order.
func sessionNames(sessions []Session) []string {
	names := make([]string, len(sessions))
	for i, s := range sessions {
		names[i] = s.Name
	}
	return names
}
