package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestConfirmStop_AcceptsExactStop(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	in := strings.NewReader("stop\n")
	if err := confirmStop(&out, in); err != nil {
		t.Fatalf("confirmStop: %v", err)
	}
	if !strings.Contains(out.String(), "Type 'stop'") {
		t.Errorf("prompt missing: %q", out.String())
	}
}

func TestConfirmStop_RejectsAnythingElse(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"y", "yes", "STOP", "  stop  \n", "", " "} {
		var out bytes.Buffer
		r := strings.NewReader(in)
		err := confirmStop(&out, r)
		switch in {
		case "  stop  \n":
			// confirmStop TrimSpaces, so this should succeed.
			if err != nil {
				t.Errorf("confirmStop(%q): %v; want nil", in, err)
			}
		default:
			if err == nil {
				t.Errorf("confirmStop(%q) accepted; want errStopAborted", in)
			}
		}
	}
}

func TestStdinIsTTY_NonFile(t *testing.T) {
	t.Parallel()

	// A bytes.Reader is not an *os.File, so stdinIsTTY must return
	// false. This matters because runStop decides whether to require
	// --force based on this answer — a false-positive would let a
	// script destroy a live server without any friction.
	in := bytes.NewReader([]byte("stop\n"))
	if stdinIsTTY(in) {
		t.Error("stdinIsTTY(bytes.Reader) returned true")
	}
}
