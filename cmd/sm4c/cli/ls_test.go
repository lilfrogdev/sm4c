package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lilfrogdev/sm4c/internal/tmuxctl"
)

func TestPartitionWindows(t *testing.T) {
	t.Parallel()

	in := []tmuxctl.Window{
		{ID: "@1", Kind: "claude", Name: "foo"},
		{ID: "@2", Kind: "", Name: "bash"},
		{ID: "@3", Kind: "claude", Name: "bar"},
		{ID: "@4", Kind: "other", Name: "baz"}, // not "claude" => unmanaged
	}
	managed, unmanaged := partitionWindows(in)
	if len(managed) != 2 || managed[0].ID != "@1" || managed[1].ID != "@3" {
		t.Errorf("managed = %+v", managed)
	}
	if len(unmanaged) != 2 || unmanaged[0].ID != "@2" || unmanaged[1].ID != "@4" {
		t.Errorf("unmanaged = %+v", unmanaged)
	}
}

func TestEmitLsText_NoServer(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := emitLsText(&buf, false, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "not running") {
		t.Errorf("output missing 'not running': %q", buf.String())
	}
}

func TestEmitLsText_HidesUnmanagedByDefault(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	managed := []tmuxctl.Window{{ID: "@1", Name: "foo", Kind: "claude", Active: true, Flags: "*"}}
	unmanaged := []tmuxctl.Window{{ID: "@9", Name: "rogue", SessionName: "sm4c"}}

	if err := emitLsText(&buf, true, managed, unmanaged, false); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "@1") || !strings.Contains(got, "foo") {
		t.Errorf("managed row missing: %q", got)
	}
	if strings.Contains(got, "@9") || strings.Contains(got, "rogue") {
		t.Errorf("unmanaged leaked without --all: %q", got)
	}
	if !strings.Contains(got, "1 unmanaged") {
		t.Errorf("hint about hidden windows missing: %q", got)
	}
}

func TestEmitLsText_AllRevealsUnmanaged(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	managed := []tmuxctl.Window{{ID: "@1", Name: "foo", Kind: "claude"}}
	unmanaged := []tmuxctl.Window{{ID: "@9", Name: "rogue", SessionName: "sm4c"}}

	if err := emitLsText(&buf, true, managed, unmanaged, true); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{"@1", "foo", "@9", "rogue", "Unmanaged"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestEmitLsJSON_StableShape(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	managed := []tmuxctl.Window{{ID: "@1", Name: "foo", Kind: "claude", Active: true, SessionName: "sm4c", Flags: "*"}}
	unmanaged := []tmuxctl.Window{{ID: "@9", Name: "rogue", SessionName: "sm4c"}}

	if err := emitLsJSON(&buf, true, managed, unmanaged, true); err != nil {
		t.Fatal(err)
	}

	var got lsJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if !got.ServerRunning {
		t.Error("ServerRunning should be true")
	}
	if len(got.Managed) != 1 || got.Managed[0].ID != "@1" || got.Managed[0].Kind != "claude" {
		t.Errorf("Managed mismatch: %+v", got.Managed)
	}
	if len(got.Unmanaged) != 1 || got.Unmanaged[0].ID != "@9" {
		t.Errorf("Unmanaged mismatch: %+v", got.Unmanaged)
	}
}

func TestEmitLsJSON_HidesUnmanagedWithoutAll(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	managed := []tmuxctl.Window{{ID: "@1", Kind: "claude"}}
	unmanaged := []tmuxctl.Window{{ID: "@9"}}

	if err := emitLsJSON(&buf, true, managed, unmanaged, false); err != nil {
		t.Fatal(err)
	}
	// Unmanaged is `omitempty`; the field should be absent from the JSON
	// bytes entirely when `--all` is false, so scripts don't accidentally
	// treat rogue windows as managed.
	if bytes.Contains(buf.Bytes(), []byte(`"unmanaged"`)) {
		t.Errorf("Unmanaged appeared in JSON without --all: %s", buf.String())
	}
}

func TestDashIfEmpty(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":       "-",
		"   ":    "-",
		"x":      "x",
		"  x  ":  "x",
		"claude": "claude",
	}
	for in, want := range cases {
		if got := dashIfEmpty(in); got != want {
			t.Errorf("dashIfEmpty(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestBoolLabel(t *testing.T) {
	t.Parallel()
	if boolLabel(true) != "yes" || boolLabel(false) != "no" {
		t.Error("boolLabel mismatch")
	}
}
