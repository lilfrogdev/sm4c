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

	// MonitorSilence is the per-pane silence threshold. When a claude pane
	// produces no output for this duration we mark it as idle/done.
	//
	// On disk this is a Go duration string: "5s", "250ms", "1m30s".
	MonitorSilence Duration `toml:"monitor_silence"`

	// LogLevel controls slog output. Valid: "debug", "info", "warn",
	// "error".
	LogLevel string `toml:"log_level"`
}

// Default returns the built-in defaults. Safe for concurrent read.
func Default() Config {
	return Config{
		SocketName:     "sm4c",
		PrefixKey:      "C-a",
		MonitorSilence: Duration(5 * time.Second),
		LogLevel:       "info",
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
	uid, ok := ownerUID(fi)
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
