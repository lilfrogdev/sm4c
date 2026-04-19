package platform

import (
	"os"
	"path/filepath"
)

// KnownClaudeLocations returns a priority-ordered list of absolute paths
// where `claude` is commonly installed, covering the supported install
// methods:
//
//   - Official native installer (the `curl ... install.sh` flow in the
//     Claude Code docs): drops a launcher at `~/.local/bin/claude`.
//   - Older installer revisions put a launcher at `~/.claude/local/claude`.
//   - `bun install -g @anthropic-ai/claude-code`: `~/.bun/bin/claude`.
//   - `npm install -g @anthropic-ai/claude-code` with a user-local prefix:
//     `~/.npm-global/bin/claude`.
//   - System package managers (Homebrew on Apple Silicon / Intel, many
//     Linux distros): `/opt/homebrew/bin/claude`, `/usr/local/bin/claude`,
//     `/usr/bin/claude`.
//
// The list is intentionally conservative — every entry is either scoped
// to the current user's home directory or a well-known system binary
// directory. It does NOT walk $PATH, does NOT scan the filesystem, and
// does NOT follow any relative paths: the goal is a small allowlist the
// caller can stat() with full confidence about what they are opening.
//
// When the caller's $HOME cannot be determined, the home-scoped entries
// are skipped rather than expanded to bogus paths.
func KnownClaudeLocations() []string {
	return knownLocations("claude")
}

// KnownTmuxLocations returns well-known install paths for tmux. tmux is
// almost always on $PATH when installed, but a fallback costs essentially
// nothing and lets sm4c doctor report usefully on exotic setups.
func KnownTmuxLocations() []string {
	return knownLocations("tmux")
}

func knownLocations(binName string) []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = append(out,
			filepath.Join(home, ".local", "bin", binName),
			filepath.Join(home, ".claude", "local", binName),
			filepath.Join(home, ".bun", "bin", binName),
			filepath.Join(home, ".npm-global", "bin", binName),
			filepath.Join(home, "bin", binName),
		)
	}
	out = append(out,
		"/opt/homebrew/bin/"+binName,
		"/usr/local/bin/"+binName,
		"/usr/bin/"+binName,
	)
	return out
}

// FindKnownBinary walks a slice of candidate absolute paths and returns
// the first one that exists, is a regular file (following symlinks), and
// has the executable bit set. Returns ("", false) if no candidate passes
// the checks.
//
// This helper is deliberately tiny: the surrounding preflight / client
// code decides what to do with the result (absolute-path check, owner
// check, etc.). Keeping those policies in one place — preflight — avoids
// scattering duplicated safety logic across callers.
func FindKnownBinary(candidates []string) (string, bool) {
	for _, c := range candidates {
		if !filepath.IsAbs(c) {
			continue
		}
		fi, err := os.Stat(c)
		if err != nil {
			continue
		}
		if !fi.Mode().IsRegular() {
			continue
		}
		if fi.Mode()&0o111 == 0 {
			continue
		}
		return c, true
	}
	return "", false
}
