// Command sm4c is the session manager for the Claude Code CLI.
//
// sm4c is an unofficial, community-built TUI/CLI that hosts multiple
// concurrent claude sessions on an isolated tmux server. It is not
// affiliated with Anthropic. See README.md for positioning and SECURITY.md
// for the threat model.
package main

import (
	"os"

	"github.com/lilfrogdev/sm4c/cmd/sm4c/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:]))
}
