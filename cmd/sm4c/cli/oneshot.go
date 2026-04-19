package cli

import (
	"fmt"

	"github.com/lilfrogdev/sm4c/internal/config"
	"github.com/lilfrogdev/sm4c/internal/tmuxctl"
)

// setupOneShot is the shared pre-execution path for every CLI subcommand
// that talks to tmux: load config, run preflight, and produce a tmuxctl
// handle bound to the resolved absolute tmux binary.
//
// Unlike `sm4c doctor`, which is happy to run even when preflight is
// fatal (reporting is the whole point), other subcommands must refuse
// to proceed when tmux is unresolvable — running any tmux command with
// an empty TmuxPath would either panic in NewOneShot (if empty) or
// shell out to whatever `tmux` means in the ambient environment,
// neither of which we want.
//
// A claude-not-found finding is *not* fatal for read-mostly subcommands
// (ls, status, stop): you can still list and inspect sessions on a
// machine where claude isn't installed. Callers that need claude
// (future: the launch path) are expected to check report.ClaudePath.
func setupOneShot(pf *persistentFlags) (tmuxctl.OneShot, Report, config.Config, error) {
	cfg, err := config.Load(pf.configPath)
	if err != nil {
		return tmuxctl.OneShot{}, Report{}, config.Config{}, fmt.Errorf("load config: %w", err)
	}
	report := Preflight(cfg)
	if report.TmuxPath == "" {
		return tmuxctl.OneShot{}, report, cfg, fmt.Errorf("tmux is not available: %s", summarizeFatals(report))
	}
	o := tmuxctl.OneShot{
		TmuxBin:     report.TmuxPath,
		SocketName:  cfg.SocketName,
		SessionName: tmuxctl.DefaultSessionName,
	}
	return o, report, cfg, nil
}

// summarizeFatals returns a short human-readable summary of fatal
// preflight findings, for use in error messages surfaced by commands
// that refuse to proceed. The full per-check detail is available via
// `sm4c doctor`.
func summarizeFatals(r Report) string {
	var fatals []string
	for _, f := range r.Findings {
		if f.Severity == SevFatal {
			fatals = append(fatals, fmt.Sprintf("%s (%s)", f.Message, f.Check))
		}
	}
	if len(fatals) == 0 {
		return "unknown preflight failure; run `sm4c doctor` for details"
	}
	if len(fatals) == 1 {
		return fatals[0] + "; run `sm4c doctor` for details"
	}
	return fmt.Sprintf("%d fatal check(s); run `sm4c doctor` for details", len(fatals))
}
