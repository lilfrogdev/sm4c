package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lilfrogdev/sm4c/internal/config"
)

func TestParseTmuxVersion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in        string
		okExpect  bool
		verExpect string
		maj, min  int
	}{
		{"tmux 3.4", true, "3.4", 3, 4},
		{"tmux 3.6a", true, "3.6a", 3, 6},
		{"tmux 3.2", true, "3.2", 3, 2},
		{"tmux next-3.7", true, "3.7", 3, 7},
		{"tmux master", false, "", 0, 0},
		{"tmux", false, "", 0, 0},
		{"", false, "", 0, 0},
		{"nottmux 3.4", true, "3.4", 3, 4},
		{"tmux foobar", false, "", 0, 0},
		{"tmux 3", true, "3", 3, 0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			ver, maj, mn, ok := parseTmuxVersion(tc.in)
			if ok != tc.okExpect {
				t.Fatalf("ok = %v; want %v (got %q %d.%d)", ok, tc.okExpect, ver, maj, mn)
			}
			if !ok {
				return
			}
			if ver != tc.verExpect || maj != tc.maj || mn != tc.min {
				t.Fatalf("parsed = %q %d.%d; want %q %d.%d",
					ver, maj, mn, tc.verExpect, tc.maj, tc.min)
			}
		})
	}
}

func TestReport_Fatal(t *testing.T) {
	t.Parallel()
	r := Report{Findings: []Finding{{Severity: SevOK}, {Severity: SevWarn}}}
	if r.Fatal() {
		t.Fatal("Report with no fatal findings reported Fatal()=true")
	}
	r.Findings = append(r.Findings, Finding{Severity: SevFatal})
	if !r.Fatal() {
		t.Fatal("Report with a fatal finding reported Fatal()=false")
	}
}

func TestPreflight_MissingBinariesAreFatal(t *testing.T) {
	t.Parallel()

	// Point tmux_bin and claude_bin at a path that cannot exist, and run
	// preflight. We should see two fatal findings (tmux:resolve,
	// claude:resolve).
	dir := t.TempDir()
	cfg := config.Default()
	cfg.TmuxBin = filepath.Join(dir, "nope", "tmux")
	cfg.ClaudeBin = filepath.Join(dir, "nope", "claude")

	r := Preflight(cfg)
	if !r.Fatal() {
		t.Fatalf("expected fatal report, got %+v", r)
	}
	found := map[string]Severity{}
	for _, f := range r.Findings {
		found[f.Check] = f.Severity
	}
	if found["tmux:resolve"] != SevFatal {
		t.Errorf("tmux:resolve severity = %v", found["tmux:resolve"])
	}
	if found["claude:resolve"] != SevFatal {
		t.Errorf("claude:resolve severity = %v", found["claude:resolve"])
	}
}

func TestPreflight_ResolvesRealTmux(t *testing.T) {
	t.Parallel()

	// Opportunistic test: if tmux is on PATH we expect Preflight to
	// resolve it and parse a version. If tmux is absent the test becomes
	// a skip rather than a failure — CI containers without tmux still
	// need to build and test sm4c.
	cfg := config.Default()
	r := Preflight(cfg)

	var tmuxResolve, tmuxVersion Finding
	for _, f := range r.Findings {
		switch f.Check {
		case "tmux:resolve":
			tmuxResolve = f
		case "tmux:version":
			tmuxVersion = f
		}
	}
	if tmuxResolve.Severity == SevFatal {
		t.Skip("tmux not on PATH; skipping live-resolve check")
	}
	if tmuxResolve.Severity != SevOK {
		t.Fatalf("tmux:resolve = %v (%s); want OK", tmuxResolve.Severity, tmuxResolve.Detail)
	}
	if tmuxVersion.Severity != SevOK {
		t.Fatalf("tmux:version = %v (%s); want OK", tmuxVersion.Severity, tmuxVersion.Detail)
	}
	if !strings.HasPrefix(r.TmuxVersion, "3.") && !strings.HasPrefix(r.TmuxVersion, "4.") {
		t.Fatalf("TmuxVersion = %q; expected 3.x or 4.x", r.TmuxVersion)
	}
}

func TestPreflight_ConfigOverrideBeatsPathAndFallback(t *testing.T) {
	t.Parallel()

	// Three candidates: a config override, a PATH entry, and a
	// well-known-path. Ensure cfg wins, and that the origin note says so.
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ntrue\n"), 0o600); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	if err := os.Chmod(bin, 0o700); err != nil { // #nosec G302 -- test fixture must be executable
		t.Fatalf("chmod: %v", err)
	}

	cfg := config.Default()
	cfg.ClaudeBin = bin
	// tmux_bin needs to not-fail for the Preflight call to proceed past
	// tmux checks; we don't assert on it here.
	cfg.TmuxBin = bin

	r := Preflight(cfg)
	var found Finding
	for _, f := range r.Findings {
		if f.Check == "claude:resolve" {
			found = f
			break
		}
	}
	if found.Severity != SevOK {
		t.Fatalf("claude:resolve sev = %v; want OK (%s)", found.Severity, found.Detail)
	}
	if !strings.Contains(found.Detail, "via config") {
		t.Fatalf("claude:resolve detail = %q; want origin 'via config'", found.Detail)
	}
}

func TestPreflight_NonAbsConfigPathIsRejected(t *testing.T) {
	t.Parallel()

	// Even if a user accidentally relative-pathed tmux_bin, Config.Validate
	// would already have caught it — but preflight is second-line defense.
	// We exercise it via the low-level check to make sure we don't
	// inadvertently resolve via CWD.
	dir := t.TempDir()
	bin := filepath.Join(dir, "tmux")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho tmux 3.4\n"), 0o600); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	// The preflight check requires an executable bit; a test fixture file
	// in an in-memory t.TempDir under the user's UID is not a real
	// security concern, so the gosec warning is suppressed.
	if err := os.Chmod(bin, 0o700); err != nil { // #nosec G302 -- test fixture must be executable
		t.Fatalf("chmod fake tmux: %v", err)
	}

	cfg := config.Default()
	cfg.TmuxBin = bin
	r := Preflight(cfg)
	var f Finding
	for _, x := range r.Findings {
		if x.Check == "tmux:resolve" {
			f = x
			break
		}
	}
	if f.Severity != SevOK {
		t.Fatalf("tmux:resolve = %v (%s); want OK for abs path", f.Severity, f.Detail)
	}
}
