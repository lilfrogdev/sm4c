// Package config loads and validates sm4c's on-disk configuration.
//
// Load enforces that the config file (and its parent directory) are owned
// by the current user and are not group- or world-writable. This is a
// defense against an attacker with a lower-privileged local account
// planting a config file on a path that sm4c would otherwise trust.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"github.com/lilfrogdev/sm4c/internal/platform"
	"github.com/pelletier/go-toml/v2"
)

// Config is the parsed, validated configuration for an sm4c invocation.
// Fields left unset on disk are filled from Default() before validation.
type Config struct {
	// TmuxBin, if non-empty, overrides PATH resolution for tmux. Must be an
	// absolute path. Empty means "resolve via exec.LookPath at startup".
	TmuxBin string `toml:"tmux_bin"`

	// ClaudeBin, if non-empty, overrides PATH resolution for claude. Must
	// be an absolute path. Empty means "resolve via exec.LookPath at
	// startup".
	ClaudeBin string `toml:"claude_bin"`

	// SocketName is the short name passed to `tmux -L`. Kept fixed across
	// users by default to avoid socket fragmentation; override with care.
	SocketName string `toml:"socket_name"`

	// PrefixKey is the single-key prefix that triggers sm4c TUI commands,
	// in tmux key notation (e.g. "C-a", "C-b").
	PrefixKey string `toml:"prefix_key"`

	// MonitorSilence is the per-pane silence threshold. When a claude
	// pane produces no output for this duration after a burst of
	// activity, sm4c marks it as idle/done and lights the "attention"
	// dot in the sidebar. This is wired to tmux's `monitor-silence`
	// window option on every managed window at spawn time.
	//
	// The default (3s) is tuned so a claude response that ends at a
	// prompt flips to idle within one poll tick after streaming
	// stops, but a brief mid-response pause (typing indicator, a
	// slow tool call) does not prematurely flip the glyph. Lowering
	// this to 1s produces a more reactive sidebar at the cost of
	// occasional flicker; raising it past 5s delays the "ready for
	// you" cue.
	//
	// Zero disables monitor-silence entirely — the sidebar will
	// still track working/quiet via monitor-activity, but will
	// never surface an "idle / waiting" state. On disk this is a
	// Go duration string: "3s", "250ms", "1m30s".
	MonitorSilence Duration `toml:"monitor_silence"`

	// SessionPollInterval is how often the TUI re-runs `tmux
	// list-windows` to refresh the sidebar. The default ("1s") is
	// effectively "live" on any healthy socket; operators running
	// on constrained hardware or over slow sockets can raise it to
	// reduce subprocess load at the cost of rename/close-lag. On
	// disk this is a Go duration string: "1s", "500ms", "2s".
	//
	// Zero or negative means "fetch once at startup and do not
	// poll" — useful for snapshot-only environments (CI, smoke
	// tests) where polling would waste a ticker goroutine.
	SessionPollInterval Duration `toml:"session_poll_interval"`

	// LogLevel controls slog output. Valid: "debug", "info", "warn",
	// "error".
	LogLevel string `toml:"log_level"`
}

// Default returns the built-in defaults. Safe for concurrent read.
func Default() Config {
	return Config{
		SocketName:          "sm4c",
		PrefixKey:           "C-a",
		MonitorSilence:      Duration(3 * time.Second),
		SessionPollInterval: Duration(1 * time.Second),
		LogLevel:            "info",
	}
}

// ErrUnsafePerms indicates a config file or its parent directory is owned
// by another user or is group/world-writable.
var ErrUnsafePerms = errors.New("config: unsafe ownership or permissions")

// Load reads a TOML config from path and returns a fully-validated
// Config. If path is empty, Load returns Default() without touching the
// filesystem.
//
// Load performs the following checks before reading:
//
//   - path is not a symlink (refuse to follow)
//   - path and its parent directory are owned by the current UID
//   - path and its parent directory are not group- or world-writable
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return cfg, fmt.Errorf("config: resolve path: %w", err)
	}
	if err := checkPathPerms(filepath.Dir(abs)); err != nil {
		return cfg, err
	}
	if err := checkPathPerms(abs); err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(abs) // #nosec G304 -- path explicitly provided by user and perm-checked above
	if err != nil {
		return cfg, fmt.Errorf("config: read %s: %w", abs, err)
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("config: parse %s: %w", abs, err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate checks invariants on a populated Config.
func (c Config) Validate() error {
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: invalid log_level %q (want debug|info|warn|error)", c.LogLevel)
	}
	if c.MonitorSilence.AsDuration() < 0 {
		return errors.New("config: monitor_silence must be non-negative")
	}
	// session_poll_interval of zero or negative means "no polling"
	// and is allowed; only a positive value is meaningful as a
	// cadence. We do not upper-bound it: an operator asking for
	// "10m" has made a deliberate tradeoff.
	_ = c.SessionPollInterval
	if c.TmuxBin != "" && !filepath.IsAbs(c.TmuxBin) {
		return fmt.Errorf("config: tmux_bin must be absolute, got %q", c.TmuxBin)
	}
	if c.ClaudeBin != "" && !filepath.IsAbs(c.ClaudeBin) {
		return fmt.Errorf("config: claude_bin must be absolute, got %q", c.ClaudeBin)
	}
	if c.SocketName == "" {
		return errors.New("config: socket_name must not be empty")
	}
	return nil
}

func checkPathPerms(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("config: stat %s: %w", p, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("config: %s is a symlink; refusing to follow: %w", p, ErrUnsafePerms)
	}
	uid, ok := platform.OwnerUID(fi)
	if ok {
		cur, err := user.Current()
		if err == nil {
			curUID, _ := strconv.Atoi(cur.Uid)
			if int(uid) != curUID {
				return fmt.Errorf("config: %s owned by uid %d, expected %d: %w", p, uid, curUID, ErrUnsafePerms)
			}
		}
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("config: %s is group- or world-writable (mode %#o): %w", p, fi.Mode().Perm(), ErrUnsafePerms)
	}
	return nil
}
