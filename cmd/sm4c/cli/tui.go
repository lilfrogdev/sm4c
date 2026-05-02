package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lilfrogdev/sm4c/internal/tmuxctl"
	"github.com/lilfrogdev/sm4c/internal/tui"
	"github.com/spf13/cobra"
	xterm "golang.org/x/term"
)

// tui.go wires `sm4c` (and `sm4c [claude-args…]`, via runLaunch)
// into the Bubble Tea TUI in internal/tui and realizes any Action
// the TUI returns on exit.
//
// Why this lives in cmd/sm4c/cli and not internal/tui:
//
//   - internal/tui is deliberately side-effect-free (no tmux, no
//     subprocess, no filesystem) so it can be unit-tested as a pure
//     state machine. Spawning a new claude window is a CLI concern.
//
//   - Preflight is CLI-only too. Running it *before* we hand the
//     terminal to Bubble Tea means a missing tmux / claude produces
//     a readable error from the shell prompt instead of a TUI that
//     flashes for a frame and then dies. This matches the "fail
//     fast with the user's normal error channel" posture we use for
//     `sm4c ls` / `sm4c status`.
//
// A note on what this file deliberately does NOT do: there is no
// exec-into-tmux path anywhere in sm4c. The TUI is the only surface
// through which users interact with sessions; if they want a plain
// tmux attach, they can use tmux directly. This decision is
// reflected in the absence of an "attach" Action in internal/tui
// and in the loop below, which re-enters the TUI after a spawn
// rather than handing the terminal to tmux.

// runTUI is the entry point for the bare `sm4c` invocation (no
// positional args). It performs preflight and hands off to openTUI
// with no initial-highlight hint and sidebar focus, so the user
// can browse sessions before committing to one. This matches the
// bare-invocation UX: nothing has been launched yet, so typing
// into a pane would have nowhere to land.
func runTUI(cmd *cobra.Command, pf *persistentFlags) error {
	o, report, cfg, err := setupOneShot(pf)
	if err != nil {
		return err
	}
	if report.ClaudePath == "" {
		return fmt.Errorf("claude is not available: %s", summarizeFatals(report))
	}
	return openTUI(cmd, o, report.ClaudePath, "", tui.FocusSidebar, cfg.SessionPollInterval.AsDuration(), cfg.MonitorSilence.AsDuration(), cfg.SidebarHighlightBG, cfg.SidebarHighlightFG)
}

// openTUI is the shared entry into the Bubble Tea runtime. Both
// `runTUI` (bare `sm4c`) and `runLaunch` (`sm4c [claude-args]`)
// funnel through here so there is exactly one place in the codebase
// that decides how to handle interactive-stdin detection, the
// pane-preview bridge, and the Action-based re-entry loop.
//
// The loop exists because `ActionNewSession` no longer exec-attaches
// into tmux — it spawns a new window outside the TUI and then re-
// opens the TUI with that window's ID as initial highlight. Each
// trip through the loop is one Bubble Tea program invocation; the
// outer `for` just threads the initial-highlight hint forward.
//
// Stdin MUST be a real TTY for Bubble Tea to work (it needs to put
// the terminal into raw mode). If stdin is a pipe — typical when
// sm4c is invoked from a non-interactive context like a CI runner
// or a shell script — we refuse to open the TUI and instead return
// a short pointer to the shell shortcuts. This prevents sm4c from
// hanging waiting for a keystroke that will never come.
func openTUI(cmd *cobra.Command, o tmuxctl.OneShot, claudeBin, initialWindowID string, initialFocus tui.Focus, pollInterval, silenceThreshold time.Duration, sidebarHighlightBG, sidebarHighlightFG string) error {
	if !interactiveStdin() {
		// Non-interactive stdin: refuse to launch the TUI (it would
		// hang) and give the user a concrete alternative. We return
		// a compact error (the `sm4c: …` prefix is added by
		// Execute, so we do NOT include it here — doing so produces
		// "sm4c: sm4c: …" in the user's shell).
		return fmt.Errorf(
			"stdin is not a TTY; refusing to open the TUI. For non-interactive use, run `sm4c ls` or `sm4c status`")
	}

	// Auto-install sm4c's lifecycle hooks into ~/.claude/settings.json.
	// Idempotent: a no-op if hooks are already present. Non-fatal on error.
	if err := installClaudeHooks(); err != nil {
		debugBridgef("installClaudeHooks: %v", err)
	}

	// Derive the hook FIFO path from the socket name so each sm4c
	// instance gets an isolated pipe. Propagate it to the tmux
	// environment so Claude Code hook scripts inherit it via $SM4C_HOOK_FIFO.
	fifoPath := filepath.Join(os.TempDir(), "sm4c-"+o.SocketName+"-hooks.fifo")
	// Embed it in GlobalEnv so every newSessionWithCommand / newWindowOnExistingSession
	// call injects it atomically — covering the first-ever launch where the tmux
	// server doesn't exist yet and SetGlobalEnv below would silently fail.
	o.GlobalEnv = map[string]string{"SM4C_HOOK_FIFO": fifoPath}
	hookCtx := cmd.Context()
	if hookCtx == nil {
		hookCtx = context.Background()
	}
	// Best-effort: silently ignore if the server is not yet running.
	_ = o.SetGlobalEnv(hookCtx, "SM4C_HOOK_FIFO", fifoPath)

	highlight := initialWindowID
	focus := initialFocus
	for {
		final, err := runTUIProgram(cmd, o, highlight, focus, pollInterval, silenceThreshold, sidebarHighlightBG, sidebarHighlightFG, fifoPath)
		if err != nil {
			return fmt.Errorf("tui: %w", err)
		}

		switch final.Action() {
		case tui.ActionNone:
			// Clean exit. Bubble Tea already restored the terminal;
			// we have nothing more to do.
			return nil

		case tui.ActionNewSession:
			// M3a stopgap: bare claude in the process's cwd. M3e
			// will replace this branch with a compose sub-view
			// driven inside the TUI, at which point this CLI-level
			// loop can go away entirely.
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			spawnCtx, cancel := context.WithTimeout(ctx, launchTimeout)
			newID, spawnErr := spawnClaudeWindow(spawnCtx, cmd.OutOrStdout(), o, claudeBin, nil, final.WorkingDir())
			cancel()
			if spawnErr != nil {
				return spawnErr
			}
			// The spawn may have just created the tmux server for the
			// first time (first-ever session). Now that the server is
			// running, ensure SM4C_HOOK_FIFO is in the global env so
			// any subsequent new-window calls inherit it too.
			_ = o.SetGlobalEnv(ctx, "SM4C_HOOK_FIFO", fifoPath)
			// Re-enter the TUI focused on the freshly-spawned
			// window with pane focus so the user can type
			// immediately into claude — matching the UX promise
			// of "press n, land in your new session ready to
			// work".
			highlight = newID
			focus = tui.FocusPane
			continue
		}

		// Defensive: an Action we don't know how to realize should
		// never escape the TUI in a released binary, but if a future
		// Action slips past without a switch arm here, we fail
		// loudly rather than silently returning.
		return fmt.Errorf("tui: unhandled action %v", final.Action())
	}
}

// runTUIProgram is a seam so tests can stub the Bubble Tea runtime
// without having a real TTY. Production code calls tui.Run with the
// process's stdin/stdout and a SessionLister backed by tmuxctl;
// tests substitute an in-memory pair and a fake lister.
//
// The OneShot handle is passed in (rather than rebuilt inside the
// seam) so tests that want to assert ordering around preflight vs.
// the TUI runtime can observe when runTUIProgram was invoked
// relative to setupOneShot.
var runTUIProgram = runTUIProgramReal

func runTUIProgramReal(cmd *cobra.Command, o tmuxctl.OneShot, initialWindowID string, initialFocus tui.Focus, pollInterval, silenceThreshold time.Duration, sidebarHighlightBG, sidebarHighlightFG, hookFifoPath string) (tui.Model, error) {
	// Bubble Tea wants the actual process stdin/stdout to drive
	// raw-mode input and terminal-level escape sequences. cmd.InOrStdin
	// and cmd.OutOrStdout return interfaces that are os.Stdin /
	// os.Stdout in production (Cobra's defaults) and test buffers in
	// unit tests — the latter don't support raw mode, which is why
	// the TUI path has runTUIProgram as a seam and tests substitute
	// it rather than trying to drive tui.Run against a bytes.Buffer.
	//
	// Pane preview (M3b.1) is wired here: if the sm4c session already
	// exists on the socket, we stand up a long-lived tmuxctl.Client
	// in control mode and bridge its %output notifications into the
	// TUI as a PaneEventStream. If the session does not exist yet
	// (the common fresh-install case), we deliberately skip the
	// Client — standing one up would create a default shell window
	// as a side effect of tmux's new-session, polluting the sidebar
	// socket. The user can still press `n` to create the first
	// session; the next TUI launch will have a session and the
	// preview will light up.
	//
	// M3b.3 adds two more seams:
	//
	//   - PaneCapturer wraps tmuxctl.OneShot.CapturePane so the
	//     first time we resolve a pane, the TUI seeds its VT
	//     emulator with that pane's current screen (rather than
	//     waiting for the next live chunk).
	//
	//   - WindowResizer wraps tmuxctl.OneShot.ResizeWindow so the
	//     TUI keeps tmux's pane grid in sync with its own
	//     right-pane viewport across terminal resizes and
	//     session switches.
	//
	// Both seams are independent of the control-mode Client — they
	// use one-shot subprocess invocations — so they stay wired
	// even when setupPaneBridge declines to spawn the Client.
	stream, resolver, closer := setupPaneBridge(cmd, o)
	if closer != nil {
		defer closer()
	}
	var capturer tui.PaneCapturer
	var resizer tui.WindowResizer
	var closerFn tui.WindowCloser
	var keys tui.KeySender
	var splitter tui.PaneSplitter
	var killer tui.PaneKiller
	if o.TmuxBin != "" {
		capturer = func(ctx context.Context, paneID string) ([]byte, error) {
			return o.CapturePane(ctx, paneID)
		}
		resizer = func(ctx context.Context, windowID string, width, height int) error {
			return o.ResizeWindow(ctx, windowID, width, height)
		}
		// WindowCloser: fold tmuxctl.ErrNoSuchWindow (server
		// gone, or window already vanished between `x` and the
		// kill-window round-trip) into a nil error. The user's
		// intent is "this session should no longer exist", which
		// is already true in that case — surfacing it as an
		// error would produce a spurious "session fetch error"
		// line on a perfectly fine close.
		closerFn = func(ctx context.Context, windowID string) error {
			err := o.KillWindow(ctx, windowID)
			if err == nil || errors.Is(err, tmuxctl.ErrNoSuchWindow) {
				return nil
			}
			return err
		}
		// KeySender: translate tmuxctl.ErrNoSuchPane into the
		// TUI-facing tui.ErrPaneGone sentinel so the TUI package
		// can react to "session closed between keystroke and
		// forward" without importing tmuxctl (the import boundary
		// is one-way: CLI imports both packages; neither internal
		// package imports the other).
		keys = func(ctx context.Context, paneID string, data []byte) error {
			err := o.SendKeys(ctx, paneID, data)
			if err == nil {
				return nil
			}
			if errors.Is(err, tmuxctl.ErrNoSuchPane) {
				return tui.ErrPaneGone()
			}
			return err
		}
		splitter = func(ctx context.Context, windowID, cwd, editorBin string) (string, error) {
			return o.SplitWindow(ctx, windowID, cwd, editorBin)
		}
		killer = func(ctx context.Context, paneID string) error {
			err := o.KillPane(ctx, paneID)
			if err == nil || errors.Is(err, tmuxctl.ErrNoSuchPane) {
				return nil
			}
			return err
		}
	}
	editorCmd := detectEditor()
	termCmd := detectShell()
	return tui.Run(
		asReader(cmd.InOrStdin()),
		asWriter(cmd.OutOrStdout()),
		tui.Deps{
			Lister:             sessionLister(o),
			PollInterval:       resolvePollInterval(pollInterval),
			PaneStream:         stream,
			PaneResolver:       resolver,
			PaneCapturer:       capturer,
			WindowResizer:      resizer,
			WindowCloser:       closerFn,
			KeySender:          keys,
			PaneSplitter:       splitter,
			PaneKiller:         killer,
			EditorCmd:          editorCmd,
			TermCmd:            termCmd,
			InitialHighlight:   initialWindowID,
			InitialFocus:       initialFocus,
			SilenceThreshold:   silenceThreshold,
			HookFifoPath:       hookFifoPath,
			SidebarHighlightBG: sidebarHighlightBG,
			SidebarHighlightFG: sidebarHighlightFG,
		},
	)
}

// setupPaneBridge stands up the M3b.1 pane-preview plumbing: a
// long-lived tmuxctl.Client plus a goroutine that filters its
// %output events into a PaneEvent channel the TUI consumes. Returns
// the stream, the resolver, and a closer that tears the bridge down.
//
// All three returns are optional. If we decide to skip the bridge
// (no sm4c session yet, Client.Start errored, preflight did not
// produce a tmux path), we return (nil, nil, nil) and the TUI runs
// with pane preview disabled — the right pane shows a static hint
// in that case rather than live bytes.
func setupPaneBridge(cmd *cobra.Command, o tmuxctl.OneShot) (tui.PaneEventStream, tui.ActivePaneResolver, func()) {
	if o.TmuxBin == "" {
		return nil, nil, nil
	}

	startCtx, cancelStart := context.WithTimeout(context.Background(), paneBridgeStartTimeout)
	defer cancelStart()

	exists, err := o.SessionExists(startCtx)
	if err != nil || !exists {
		// Either the socket is unreachable or the session does not
		// exist yet. In both cases we decline to spawn the control-
		// mode client — see the rationale above setupPaneBridge.
		// The TUI will still render the sidebar and empty-state.
		return nil, nil, nil
	}

	cfg := tmuxctl.ClientConfig{
		TmuxBin:     o.TmuxBin,
		SocketName:  o.SocketName,
		SessionName: o.SessionName,
	}
	// Declare the control-mode client's viewport as the RIGHT-PANE
	// BODY dimensions, not the full terminal dimensions.
	//
	// tmux's default `window-size latest` auto-sizes every window
	// in the session to match the most-recently-active client's
	// viewport. If we lie to tmux and say our client is as wide as
	// the whole terminal, tmux sizes the windows to that, claude
	// draws its UI to the full terminal width, and our right-pane
	// column ends up only showing the LEFT SLICE of a much wider
	// grid — the screenshot symptom "text wraps far beyond the
	// column and bleeds onto the next row".
	//
	// Explicit `resize-window` calls cannot reliably fix this:
	// `window-size latest` snaps every window back to the client
	// viewport on the next triggering event (attach, detach,
	// refresh-client), so the manual size is consistently clobbered.
	// The robust fix is to stop fighting the auto-sizer: declare
	// the viewport that MATCHES the visible surface, let
	// `window-size latest` do its job, and skip the redundant
	// per-window resize round-trip entirely.
	//
	// Falling back to full terminal dims when body dims are not
	// computable (narrow terminal, missing controlling tty) keeps
	// the previous behavior in those degenerate cases; anything
	// is better than tmux defaulting to 80x24.
	if termW, termH, ok := terminalDims(); ok {
		if bodyW, bodyH := tui.RightPaneBodyDims(termW, termH); bodyW > 0 && bodyH > 0 {
			cfg.InitialWidth = bodyW
			cfg.InitialHeight = bodyH
		} else {
			cfg.InitialWidth = termW
			cfg.InitialHeight = termH
		}
	}
	// Client.Start blocks until tmux finishes its handshake. A short
	// bound keeps a hung socket from stalling the TUI; if we time
	// out the user still gets the TUI, just without live preview.
	client, err := tmuxctl.Start(startCtx, cfg)
	if err != nil {
		debugBridgef("Start FAILED err=%v", err)
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"sm4c: pane preview disabled: %v\n", err)
		return nil, nil, nil
	}
	debugBridgef("Start OK initW=%d initH=%d", cfg.InitialWidth, cfg.InitialHeight)

	events := make(chan tui.PaneEvent, paneBridgeBufferSize)

	// The bridge goroutine drains client.Events() for the lifetime
	// of the Client and forwards every OutputEvent into `events` as
	// a PaneEvent. Non-output notifications are discarded in M3b.1;
	// future milestones (status detection, session renaming) will
	// grow this switch.
	var once sync.Once
	closeEvents := func() { once.Do(func() { close(events) }) }
	go func() {
		defer closeEvents()
		defer debugBridgef("bridge-exit reason=client.events-closed")
		for ev := range client.Events() {
			switch out := ev.(type) {
			case tmuxctl.OutputEvent:
				// Non-blocking send with drop: if the TUI is too slow
				// to drain, we prefer to lose a chunk rather than
				// block the tmux protocol reader.
				select {
				case events <- tui.PaneEvent{PaneID: out.PaneID, Data: out.Data}:
				default:
					debugBridgef("DROP pane=%s bytes=%d (buffer full)", out.PaneID, len(out.Data))
				}
			case tmuxctl.NotificationEvent:
				// %pane-exited $session session-name @window window-name %pane exit-status
				// The pane ID (the arg matching %\d+) can be at any position
				// depending on the tmux version. Scan all args rather than
				// assuming a fixed index — Args[0] is the session ID ($N), not
				// the pane ID.
				if out.Kind == "pane-exited" {
					paneID := notificationPaneID(out.Args)
					if paneID != "" {
						select {
						case events <- tui.PaneEvent{PaneID: paneID, Closed: true}:
						default:
							debugBridgef("DROP pane-exited pane=%s (buffer full)", paneID)
						}
					}
				}
			}
		}
	}()

	// The PaneEventStream contract is "call once, get back a channel".
	// We synthesize one that hands out the same shared channel so
	// the TUI's internal waitForPaneEvent loop observes the same
	// close semantics no matter how many times it's invoked.
	stream := func() <-chan tui.PaneEvent { return events }

	// resolver uses OneShot (not the Client's Send) because
	// display-message is a one-shot semantic by nature and the
	// Client is busy fan-ing out %output notifications. Using a
	// fresh subprocess here also isolates resolution failures from
	// the pane stream: a missing window returns ErrNoSuchPane
	// without breaking the active preview.
	resolver := func(ctx context.Context, windowID string) (string, error) {
		paneID, err := o.ActivePane(ctx, windowID)
		if errors.Is(err, tmuxctl.ErrNoSuchPane) {
			return "", err
		}
		return paneID, err
	}

	closer := func() {
		// Closing the Client makes its Events() channel close,
		// which lets the bridge goroutine exit, which closes
		// `events`, which tells the TUI's waiter loop to emit
		// paneStreamClosedMsg and stop arming reads. One call
		// tears everything down in order.
		_ = client.Close()
	}

	return stream, resolver, closer
}

// paneBridgeStartTimeout bounds both SessionExists and Client.Start.
// A slow tmux socket should not block the TUI from opening; we prefer
// to launch without preview and let the user see the sidebar
// immediately.
const paneBridgeStartTimeout = 3 * time.Second

// paneBridgeBufferSize bounds the backpressure window between the
// tmuxctl event reader and the TUI. A full buffer drops chunks
// rather than blocking; for a raw-bytes preview that is fine (the
// user sees the latest state on the next frame), and it keeps the
// tmux protocol reader responsive.
const paneBridgeBufferSize = 512

// detectShell returns the shell binary for the `t` binding. It reads
// $SHELL first (the user's login shell), then falls back to /bin/sh.
func detectShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

// detectEditor returns the editor binary to use for the `o` binding by
// consulting $VISUAL, then $EDITOR, then PATH (looking for nvim, vim, nano
// in that order). Returns "" if no editor is found, which disables the `o`
// binding.
func detectEditor() string {
	// $VISUAL and $EDITOR are the POSIX-standard way to specify a preferred
	// editor; most shells and tools honour this convention.
	for _, env := range []string{"VISUAL", "EDITOR"} {
		if v := os.Getenv(env); v != "" {
			// Take only the first word — some users set EDITOR to
			// "nvim -e" or similar; we want just the binary name.
			bin := strings.Fields(v)[0]
			if filepath.IsAbs(bin) {
				return bin
			}
			if p, err := exec.LookPath(bin); err == nil {
				return p
			}
		}
	}
	// Fall back to well-known editors in preference order.
	for _, name := range []string{"nvim", "vim", "nano"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// sessionLister adapts tmuxctl.OneShot.ListWindows to the
// tui.SessionLister signature. Two responsibilities:
//
//   - Filter to sm4c-managed windows (Kind == KindClaude). The
//     sidebar renders only sessions we know how to reason about;
//     any un-tagged windows a user may have created on the sm4c
//     socket by hand stay hidden.
//
//   - Project tmuxctl.Window into the TUI-local tui.Session value
//     so the TUI package never sees a tmuxctl type. This is the
//     same one-way-dependency pattern we use at every CLI↔internal
//     boundary.
func sessionLister(o tmuxctl.OneShot) tui.SessionLister {
	return func(ctx context.Context) ([]tui.Session, error) {
		wins, err := o.ListWindows(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]tui.Session, 0, len(wins))
		for _, w := range wins {
			if !w.Managed() {
				continue
			}
			out = append(out, tui.Session{
				WindowID:  w.ID,
				Name:      w.Name,
				Active:    w.Active,
				Cwd:       w.CurrentPath,
				Title:     w.PaneTitle,
				PaneCount: w.PaneCount,
			})
		}
		return out, nil
	}
}

// resolvePollInterval picks the effective session-list poll cadence
// the TUI will run at. It honors the operator's `session_poll_interval`
// TOML value when positive, and falls back to tui.DefaultPollInterval
// for the zero-value case (e.g. a config file that predates this
// field, or an in-process caller that passed 0). A negative value
// passed explicitly is preserved — the Model treats that as "fetch
// once and never poll", which is the snapshot-only contract.
func resolvePollInterval(cfg time.Duration) time.Duration {
	if cfg == 0 {
		return tui.DefaultPollInterval
	}
	return cfg
}

// interactiveStdin reports whether the process's standard input is
// a terminal. It's a package-level var (not a function) so tests
// that exercise runTUI end-to-end can flip it to true without
// touching a real fd — running `go test` detaches stdin, so any
// code path that gates on a real TTY would be unreachable without
// this seam.
//
// Production code initializes it from isInteractiveStdinReal, which
// checks os.Stdin via golang.org/x/term. We deliberately check
// os.Stdin (not cmd.InOrStdin) because Bubble Tea always reads from
// the process stdin in production.
var interactiveStdin = isInteractiveStdinReal

func isInteractiveStdinReal() bool {
	// #nosec G115 -- file descriptors on Linux/macOS fit in int
	// comfortably; this is the same cast we use in cmd/sm4c/cli/stop.go.
	return xterm.IsTerminal(int(os.Stdin.Fd()))
}

// terminalDims reports the current controlling terminal's size.
// It tries stdout first (what Bubble Tea writes to and the size
// everything downstream cares about) and falls back to stdin so
// pipelines that redirect stdout still get a sensible hint.
// Returns ok=false when neither fd is a terminal or xterm.GetSize
// fails — callers should treat that as "skip pre-sizing".
func terminalDims() (int, int, bool) {
	for _, fd := range []uintptr{os.Stdout.Fd(), os.Stdin.Fd()} {
		// #nosec G115 -- fd ints match the cast used elsewhere in cli.
		n := int(fd)
		if !xterm.IsTerminal(n) {
			continue
		}
		w, h, err := xterm.GetSize(n)
		if err != nil || w < 1 || h < 1 {
			continue
		}
		return w, h, true
	}
	return 0, 0, false
}

// notificationPaneID scans the whitespace-split args of a tmux notification
// for the first token matching the tmux pane ID format (%N where N is one or
// more digits). This is necessary because the arg position of the pane ID
// varies by notification type — for %pane-exited the layout is:
//
//	$session session-name @window window-name %pane exit-status
//
// so the pane ID is NOT at index 0 (that is the session ID, $N). Scanning all
// args handles version differences without fragile index assumptions.
func notificationPaneID(args []string) string {
	for _, a := range args {
		if len(a) < 2 || a[0] != '%' {
			continue
		}
		allDigits := true
		for _, c := range a[1:] {
			if c < '0' || c > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return a
		}
	}
	return ""
}

// asReader / asWriter coerce Cobra's io.Reader / io.Writer
// interfaces down to the concrete types internal/tui.Run accepts.
// tui.Run takes structurally-typed interfaces (not io.Reader /
// io.Writer by name) to emphasize that the package is not doing
// anything beyond Read / Write — no seek, no close, no ReadAt.
func asReader(r io.Reader) interface {
	Read(p []byte) (n int, err error)
} {
	return r
}

func asWriter(w io.Writer) interface {
	Write(p []byte) (n int, err error)
} {
	return w
}
