package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestDefaultValidates(t *testing.T) {
	t.Parallel()
	if err := Default().Validate(); err != nil {
		t.Fatalf("Default() failed to validate: %v", err)
	}
}

func TestLoadEmptyPathReturnsDefault(t *testing.T) {
	t.Parallel()
	got, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") unexpected error: %v", err)
	}
	if got != Default() {
		t.Fatalf("Load(\"\") returned %+v; want %+v", got, Default())
	}
}

func TestLoadParsesValidTOML(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sm4c.toml")
	body := `
socket_name   = "sm4c"
prefix_key    = "C-b"
monitor_silence = "10s"
log_level     = "debug"
`
	mustWriteFile(t, path, body, 0o600)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PrefixKey != "C-b" {
		t.Errorf("PrefixKey = %q; want C-b", cfg.PrefixKey)
	}
	if cfg.MonitorSilence.AsDuration() != 10*time.Second {
		t.Errorf("MonitorSilence = %v; want 10s", cfg.MonitorSilence)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q; want debug", cfg.LogLevel)
	}
}

func TestLoadRejectsWorldWritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("perm checks are unix-only")
	}
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sm4c.toml")
	mustWriteFile(t, path, "socket_name=\"sm4c\"\n", 0o666)

	_, err := Load(path)
	if !errors.Is(err, ErrUnsafePerms) {
		t.Fatalf("Load on world-writable file: got %v; want ErrUnsafePerms", err)
	}
}

func TestLoadRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior is unix-only here")
	}
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "real.toml")
	link := filepath.Join(dir, "link.toml")
	mustWriteFile(t, target, "socket_name=\"sm4c\"\n", 0o600)
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, err := Load(link)
	if !errors.Is(err, ErrUnsafePerms) {
		t.Fatalf("Load on symlink: got %v; want ErrUnsafePerms", err)
	}
}

func TestValidateRejectsBadLogLevel(t *testing.T) {
	t.Parallel()
	c := Default()
	c.LogLevel = "verbose"
	if err := c.Validate(); err == nil {
		t.Fatal("Validate accepted log_level=verbose")
	}
}

func TestValidateRejectsRelativeBinary(t *testing.T) {
	t.Parallel()
	c := Default()
	c.TmuxBin = "tmux"
	if err := c.Validate(); err == nil {
		t.Fatal("Validate accepted relative tmux_bin")
	}
	c = Default()
	c.ClaudeBin = "claude"
	if err := c.Validate(); err == nil {
		t.Fatal("Validate accepted relative claude_bin")
	}
}

func mustWriteFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	// os.WriteFile honors umask; force the mode explicitly.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}
