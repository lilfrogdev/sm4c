package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// claude_hooks.go auto-installs sm4c's Claude Code lifecycle hooks into
// ~/.claude/settings.json on startup. The merge is idempotent: if the
// SM4C_HOOK_FIFO marker is already present in any hook entry for a given
// event, that event is left untouched. Unknown fields and existing hooks
// from other tools are preserved.
//
// The installed hooks are guarded by [ -n "$SM4C_HOOK_FIFO" ] so they
// silently no-op when claude runs outside an sm4c-managed tmux pane —
// plain terminal sessions, VS Code, CI, and so on are unaffected.
//
// Failure to install (file unreadable, malformed JSON, permission error)
// is non-fatal: sm4c logs at debug level and continues. Status indicators
// simply remain at StatusQuiet/StatusIdle until hooks are working.

const hookMarker = "SM4C_HOOK_FIFO"

// claudeHookCommand is the inner command handler. Claude Code validates that
// each element in an event array is a matcher group with a nested "hooks"
// sub-array of these objects.
type claudeHookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Async   bool   `json:"async"`
}

// claudeHookGroup is the outer matcher group. For events without matcher
// support (Stop, Notification, UserPromptSubmit) the matcher is ignored but
// the nested "hooks" array is still required by the validator.
type claudeHookGroup struct {
	Matcher string              `json:"matcher"`
	Hooks   []claudeHookCommand `json:"hooks"`
}

// hookScript queries the tmux global environment at hook-fire time for the
// FIFO path. This avoids the shell-inheritance race: if claude was already
// running when sm4c started and set SM4C_HOOK_FIFO, the hook subprocess
// wouldn't see the var through normal env inheritance. `tmux show-environment
// -v` reads the server's global table directly, so it works regardless of
// when claude was launched.
// hookScript is a fmt.Sprintf template: %%s becomes %s (for the shell's
// printf verb), %s is replaced by the event name (prompt_submit/stop/notification).
// Uses $SM4C_HOOK_FIFO from the shell environment first (fast path for sessions
// started after sm4c); falls back to tmux show-environment -g for sessions that
// predate sm4c's startup. Uses sed to extract the value since -v is not available
// in tmux < 3.4.
const hookScript = `[ -n "$TMUX_PANE" ] && { _fifo=$SM4C_HOOK_FIFO; [ -z "$_fifo" ] && _fifo=$(tmux show-environment -g SM4C_HOOK_FIFO 2>/dev/null | sed -n 's/^SM4C_HOOK_FIFO=//p'); true; } && [ -n "$_fifo" ] && printf '%%s %s\n' "$TMUX_PANE" >> "$_fifo" 2>/dev/null; true`

var sm4cHooks = map[string]claudeHookGroup{
	"UserPromptSubmit": {
		Matcher: "*",
		Hooks: []claudeHookCommand{{
			Type:    "command",
			Command: fmt.Sprintf(hookScript, "prompt_submit"),
			Async:   true,
		}},
	},
	"Stop": {
		Matcher: "*",
		Hooks: []claudeHookCommand{{
			Type:    "command",
			Command: fmt.Sprintf(hookScript, "stop"),
			Async:   true,
		}},
	},
	"Notification": {
		Matcher: "*",
		Hooks: []claudeHookCommand{{
			Type:    "command",
			Command: fmt.Sprintf(hookScript, "notification"),
			Async:   true,
		}},
	},
}

// installClaudeHooks merges sm4c's lifecycle hooks into ~/.claude/settings.json.
// It is safe to call on every startup: it is a no-op when the hooks are
// already present.
func installClaudeHooks() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("claude hooks: home dir: %w", err)
	}
	path := filepath.Join(home, ".claude", "settings.json")

	// Read the existing file, or start from an empty object.
	raw, err := os.ReadFile(path) // #nosec G304 -- ~/.claude/settings.json is a well-known path
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("claude hooks: read %s: %w", path, err)
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}

	// Parse top level as a generic map so we preserve unknown fields.
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return fmt.Errorf("claude hooks: parse %s: %w — file unchanged", path, err)
	}
	if root == nil {
		root = make(map[string]json.RawMessage)
	}

	// Parse the hooks section (may be absent).
	hooks := make(map[string]json.RawMessage)
	if hooksRaw, ok := root["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &hooks); err != nil {
			return fmt.Errorf("claude hooks: parse hooks section: %w — file unchanged", err)
		}
	}

	changed := false
	for event, group := range sm4cHooks {
		// Parse the existing array for this event.
		var arr []json.RawMessage
		if existing, ok := hooks[event]; ok {
			if err := json.Unmarshal(existing, &arr); err != nil {
				// Can't parse this event's array; skip it rather than clobber.
				debugBridgef("claude hooks: skip %s: cannot parse existing array: %v", event, err)
				continue
			}
		}

		// Look for an existing sm4c entry by marker. If found, compare the
		// command to detect stale installs and replace in-place. If not found,
		// append. Either way, only the sm4c entry is touched; other tools'
		// entries in the same event array are preserved.
		idx, found := findSm4cEntry(arr, hookMarker)
		if found {
			var existing claudeHookGroup
			if err := json.Unmarshal(arr[idx], &existing); err == nil &&
				len(existing.Hooks) > 0 && len(group.Hooks) > 0 &&
				existing.Hooks[0].Command == group.Hooks[0].Command {
				continue // up to date
			}
			// Stale — replace in-place so the entry stays at the same position.
			groupJSON, err := jsonMarshal(group)
			if err != nil {
				return fmt.Errorf("claude hooks: marshal %s group: %w", event, err)
			}
			arr[idx] = json.RawMessage(groupJSON)
			debugBridgef("claude hooks: updated stale %s hook", event)
		} else {
			groupJSON, err := jsonMarshal(group)
			if err != nil {
				return fmt.Errorf("claude hooks: marshal %s group: %w", event, err)
			}
			arr = append(arr, json.RawMessage(groupJSON))
		}

		arrJSON, err := jsonMarshal(arr)
		if err != nil {
			return fmt.Errorf("claude hooks: marshal %s array: %w", event, err)
		}
		hooks[event] = json.RawMessage(arrJSON)
		changed = true
	}

	if !changed {
		return nil
	}

	hooksJSON, err := jsonMarshal(hooks)
	if err != nil {
		return fmt.Errorf("claude hooks: marshal hooks map: %w", err)
	}
	root["hooks"] = json.RawMessage(hooksJSON)

	out, err := jsonMarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("claude hooks: marshal root: %w", err)
	}
	out = append(out, '\n')

	// Atomic write: temp file in the same directory, then rename.
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("claude hooks: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".sm4c-settings-*.json")
	if err != nil {
		return fmt.Errorf("claude hooks: create temp: %w", err)
	}
	tmpName := tmp.Name()
	// Ensure temp file is cleaned up if rename fails.
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("claude hooks: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("claude hooks: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("claude hooks: rename to %s: %w", path, err)
	}
	debugBridgef("claude hooks: installed into %s", path)
	return nil
}

// findSm4cEntry scans arr for the first element whose raw JSON contains marker.
// Returns the index and true if found, or -1 and false if not.
func findSm4cEntry(arr []json.RawMessage, marker string) (int, bool) {
	for i, item := range arr {
		if strings.Contains(string(item), marker) {
			return i, true
		}
	}
	return -1, false
}

// jsonMarshal encodes v without HTML-escaping & < > so shell commands in
// the settings file remain human-readable.
func jsonMarshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// jsonMarshalIndent is jsonMarshal + pretty-printing.
func jsonMarshalIndent(v any, prefix, indent string) ([]byte, error) {
	raw, err := jsonMarshal(v)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, prefix, indent); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
