package cli

// This file holds the shared pre-flight machinery used by the TUI launcher
// and `sm4c doctor`. In M0 it only defines the contract; the actual checks
// (tmux version, claude version, socket directory permission, PATH
// resolution) arrive in M1 alongside the tmuxctl package.
//
// The contract is intentionally minimal: preflight is side-effect-free
// beyond os.Stat / exec.LookPath calls, returns a structured report, and
// distinguishes "hard failure" (cannot proceed) from "warning" (degraded
// but runnable). No caller of preflight is allowed to `os.Exit` on a
// warning; that policy is the TUI launcher's job.

// Finding severity.
type Severity int

const (
	SevOK Severity = iota
	SevWarn
	SevFatal
)

// Finding is a single preflight result. Message is already safe-to-print
// (no control chars) — callers must ensure that.
type Finding struct {
	Check    string
	Severity Severity
	Message  string
}

// Report is the output of Preflight.
type Report struct {
	Findings []Finding
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

// Preflight is the shared entry point. M1 will populate it.
func Preflight() Report {
	return Report{
		Findings: []Finding{
			{Check: "m0-stub", Severity: SevWarn, Message: "preflight not implemented until M1"},
		},
	}
}
