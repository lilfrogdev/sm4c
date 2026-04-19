package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lilfrogdev/sm4c/internal/config"
	"github.com/lilfrogdev/sm4c/internal/platform"
	"github.com/lilfrogdev/sm4c/internal/safe"
)

// Severity classifies a preflight finding.
//
// The three-level ladder — OK / Warn / Fatal — is deliberately coarse. Any
// check that sm4c cannot proceed past (missing tmux, tmux too old, claude
// missing when TUI is requested) is Fatal. Anything the TUI can paper over
// (stale socket dir, claude version we don't recognize) is Warn.
type Severity int

const (
	SevOK Severity = iota
	SevWarn
	SevFatal
)

func (s Severity) String() string {
	switch s {
	case SevOK:
		return "ok"
	case SevWarn:
		return "warn"
	case SevFatal:
		return "fatal"
	default:
		return fmt.Sprintf("sev(%d)", int(s))
	}
}

// Finding is a single preflight result. Message is already passed through
// safe.Label so it is guaranteed to be free of control bytes and ANSI
// escape sequences before it ever reaches the terminal.
type Finding struct {
	Check    string
	Severity Severity
	Message  string
	// Detail is optional extra context (resolved paths, detected version
	// strings). Also safe.Label'd.
	Detail string
}

// Report is the output of Preflight. It is intentionally plain-data so
// tests can assert on it and `sm4c doctor` can format it without a lot
// of ceremony.
type Report struct {
	Findings []Finding
	// TmuxPath and ClaudePath are the resolved absolute paths if
	// resolution succeeded; empty otherwise.
	TmuxPath   string
	ClaudePath string
	// TmuxVersion is the parsed version (e.g. "3.6a") or empty.
	TmuxVersion string
}

// Fatal reports whether any finding is SevFatal.
func (r Report) Fatal() bool {
	for _, f := range r.Findings {
		if f.Severity == SevFatal {
			return true
		}
	}
	return false
}

// MinTmuxMajor / MinTmuxMinor is the minimum tmux version sm4c supports.
// Control mode is stable from 3.2 onward; earlier releases handle some
// notifications differently.
const (
	MinTmuxMajor = 3
	MinTmuxMinor = 2
)

// tmuxVersionTimeout bounds the `tmux -V` probe. A well-behaved tmux
// returns instantly; a hang here almost certainly means a compromised or
// broken binary on PATH.
const tmuxVersionTimeout = 2 * time.Second

// Preflight runs every environment check and returns a structured Report.
// It never writes to the terminal and never modifies state outside of
// transient os.Stat and exec.Command("tmux", "-V") calls.
func Preflight(cfg config.Config) Report {
	r := Report{}
	r.TmuxPath = r.checkTmux(cfg)
	if r.TmuxPath != "" {
		r.TmuxVersion = r.checkTmuxVersion(r.TmuxPath)
	}
	r.ClaudePath = r.checkClaude(cfg)
	r.checkSocketDir()
	return r
}

func (r *Report) add(f Finding) {
	f.Message = safe.Label(f.Message)
	f.Detail = safe.Label(f.Detail)
	r.Findings = append(r.Findings, f)
}

// checkTmux resolves the tmux binary either from cfg.TmuxBin or PATH and
// returns its absolute path (or "" on failure).
func (r *Report) checkTmux(cfg config.Config) string {
	const check = "tmux:resolve"

	candidate := cfg.TmuxBin
	if candidate == "" {
		p, err := exec.LookPath("tmux")
		if err != nil {
			r.add(Finding{
				Check:    check,
				Severity: SevFatal,
				Message:  "tmux not found on PATH; install tmux >= 3.2",
			})
			return ""
		}
		candidate = p
	}
	abs, err := filepath.Abs(candidate)
	if err != nil {
		r.add(Finding{
			Check:    check,
			Severity: SevFatal,
			Message:  "cannot resolve tmux path",
			Detail:   err.Error(),
		})
		return ""
	}
	if !filepath.IsAbs(abs) {
		r.add(Finding{
			Check:    check,
			Severity: SevFatal,
			Message:  "tmux path must be absolute",
			Detail:   abs,
		})
		return ""
	}
	fi, err := os.Stat(abs)
	if err != nil {
		r.add(Finding{
			Check:    check,
			Severity: SevFatal,
			Message:  "tmux binary not accessible",
			Detail:   err.Error(),
		})
		return ""
	}
	if fi.Mode()&0o111 == 0 {
		r.add(Finding{
			Check:    check,
			Severity: SevFatal,
			Message:  "tmux binary is not executable",
			Detail:   abs,
		})
		return ""
	}
	r.add(Finding{
		Check:    check,
		Severity: SevOK,
		Message:  "tmux binary resolved",
		Detail:   abs,
	})
	return abs
}

// checkTmuxVersion runs `tmux -V`, parses the version string, and returns
// the parsed version (e.g. "3.6a") for display.
func (r *Report) checkTmuxVersion(tmuxPath string) string {
	const check = "tmux:version"

	ctx, cancel := context.WithTimeout(context.Background(), tmuxVersionTimeout)
	defer cancel()
	// #nosec G204 -- tmuxPath was validated absolute+executable above, and
	// "-V" is a compile-time constant.
	cmd := exec.CommandContext(ctx, tmuxPath, "-V")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		r.add(Finding{
			Check:    check,
			Severity: SevFatal,
			Message:  "tmux -V failed",
			Detail:   err.Error(),
		})
		return ""
	}
	raw := strings.TrimSpace(out.String())
	ver, major, minor, ok := parseTmuxVersion(raw)
	if !ok {
		r.add(Finding{
			Check:    check,
			Severity: SevFatal,
			Message:  "cannot parse tmux version",
			Detail:   raw,
		})
		return ""
	}
	if major < MinTmuxMajor || (major == MinTmuxMajor && minor < MinTmuxMinor) {
		r.add(Finding{
			Check:    check,
			Severity: SevFatal,
			Message:  fmt.Sprintf("tmux %s is too old; sm4c needs >= %d.%d", ver, MinTmuxMajor, MinTmuxMinor),
			Detail:   raw,
		})
		return ver
	}
	r.add(Finding{
		Check:    check,
		Severity: SevOK,
		Message:  fmt.Sprintf("tmux %s", ver),
		Detail:   raw,
	})
	return ver
}

// parseTmuxVersion extracts the version from tmux's "-V" output. tmux
// prints one of:
//
//	"tmux 3.6a"
//	"tmux next-3.6"
//	"tmux master" (rare, only in self-builds)
//
// We parse the first token starting with a digit and split on the first
// "." into major/minor. A trailing letter (like "a" in "3.6a") is
// preserved in the returned version string but ignored for comparison.
func parseTmuxVersion(s string) (version string, major, minor int, ok bool) {
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return "", 0, 0, false
	}
	ver := fields[1]
	ver = strings.TrimPrefix(ver, "next-")
	if ver == "" {
		return "", 0, 0, false
	}
	if !isAsciiDigit(ver[0]) {
		return "", 0, 0, false
	}
	dot := strings.IndexByte(ver, '.')
	if dot < 0 {
		n, err := strconv.Atoi(ver)
		if err != nil {
			return "", 0, 0, false
		}
		return ver, n, 0, true
	}
	majS := ver[:dot]
	rest := ver[dot+1:]
	maj, err := strconv.Atoi(majS)
	if err != nil {
		return "", 0, 0, false
	}
	minS := rest
	for i := 0; i < len(rest); i++ {
		if !isAsciiDigit(rest[i]) {
			minS = rest[:i]
			break
		}
	}
	if minS == "" {
		return "", 0, 0, false
	}
	min, err := strconv.Atoi(minS)
	if err != nil {
		return "", 0, 0, false
	}
	return ver, maj, min, true
}

func isAsciiDigit(b byte) bool { return b >= '0' && b <= '9' }

// checkClaude resolves the claude binary either from cfg.ClaudeBin or
// PATH and returns its absolute path. A missing claude is fatal: sm4c is
// only useful with Claude Code installed.
func (r *Report) checkClaude(cfg config.Config) string {
	const check = "claude:resolve"

	candidate := cfg.ClaudeBin
	if candidate == "" {
		p, err := exec.LookPath("claude")
		if err != nil {
			r.add(Finding{
				Check:    check,
				Severity: SevFatal,
				Message:  "claude not found on PATH; install the official Claude Code CLI",
			})
			return ""
		}
		candidate = p
	}
	abs, err := filepath.Abs(candidate)
	if err != nil {
		r.add(Finding{
			Check:    check,
			Severity: SevFatal,
			Message:  "cannot resolve claude path",
			Detail:   err.Error(),
		})
		return ""
	}
	fi, err := os.Stat(abs)
	if err != nil {
		r.add(Finding{
			Check:    check,
			Severity: SevFatal,
			Message:  "claude binary not accessible",
			Detail:   err.Error(),
		})
		return ""
	}
	if fi.Mode()&0o111 == 0 {
		r.add(Finding{
			Check:    check,
			Severity: SevFatal,
			Message:  "claude binary is not executable",
			Detail:   abs,
		})
		return ""
	}
	r.add(Finding{
		Check:    check,
		Severity: SevOK,
		Message:  "claude binary resolved",
		Detail:   abs,
	})
	return abs
}

// checkSocketDir verifies that the directory tmux will put our socket in
// is safe. tmux uses, in order of preference:
//
//  1. $TMUX_TMPDIR if set
//  2. /tmp/tmux-<uid>/
//
// The directory must exist (or its parent must be writable so tmux can
// create it), be owned by the current uid, and have 0700 permissions.
// Anything more permissive could let another local user hijack our
// control socket.
func (r *Report) checkSocketDir() {
	const check = "socket:dir"

	parent := tmuxSocketParent()
	fi, err := os.Lstat(parent)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			r.add(Finding{
				Check:    check,
				Severity: SevWarn,
				Message:  "tmux socket directory does not yet exist; will be created on first run",
				Detail:   parent,
			})
			return
		}
		r.add(Finding{
			Check:    check,
			Severity: SevFatal,
			Message:  "cannot stat tmux socket directory",
			Detail:   err.Error(),
		})
		return
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		r.add(Finding{
			Check:    check,
			Severity: SevFatal,
			Message:  "tmux socket directory is a symlink; refusing to trust",
			Detail:   parent,
		})
		return
	}
	if !fi.IsDir() {
		r.add(Finding{
			Check:    check,
			Severity: SevFatal,
			Message:  "tmux socket path exists but is not a directory",
			Detail:   parent,
		})
		return
	}
	uid, ok := platform.OwnerUID(fi)
	if ok {
		cur, err := user.Current()
		if err == nil {
			curUID, _ := strconv.Atoi(cur.Uid)
			if int(uid) != curUID {
				r.add(Finding{
					Check:    check,
					Severity: SevFatal,
					Message:  fmt.Sprintf("tmux socket dir owned by uid %d, expected %d", uid, curUID),
					Detail:   parent,
				})
				return
			}
		}
	}
	if fi.Mode().Perm()&0o077 != 0 {
		r.add(Finding{
			Check:    check,
			Severity: SevFatal,
			Message:  fmt.Sprintf("tmux socket dir mode %#o is too permissive; expected 0700", fi.Mode().Perm()),
			Detail:   parent,
		})
		return
	}
	r.add(Finding{
		Check:    check,
		Severity: SevOK,
		Message:  "tmux socket directory looks sane",
		Detail:   parent,
	})
}

// tmuxSocketParent computes the directory tmux will use for its per-user
// socket. It mirrors tmux's own algorithm from server.c.
func tmuxSocketParent() string {
	if d := os.Getenv("TMUX_TMPDIR"); d != "" {
		return d
	}
	u, err := user.Current()
	if err != nil || u.Uid == "" {
		return "/tmp/tmux-unknown"
	}
	return "/tmp/tmux-" + u.Uid
}
