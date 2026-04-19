package tmuxctl

import (
	"bytes"
	"testing"
)

func TestLimitWriter_UnderCap(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	w := limitWriter(&b, 16)
	n, err := w.Write([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("n=%d want 5", n)
	}
	if b.String() != "hello" {
		t.Errorf("buf=%q want hello", b.String())
	}
}

func TestLimitWriter_OverCap(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	w := limitWriter(&b, 4)

	n, err := w.Write([]byte("hello world"))
	if err != nil {
		t.Fatal(err)
	}
	// Caller sees the full length (no short-write surprise), but the
	// underlying buffer is capped.
	if n != 11 {
		t.Errorf("n=%d want 11", n)
	}
	if b.String() != "hell" {
		t.Errorf("buf=%q want 'hell'", b.String())
	}

	// Subsequent writes are dropped silently.
	n, err = w.Write([]byte("more"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("post-cap n=%d want 4", n)
	}
	if b.String() != "hell" {
		t.Errorf("post-cap buf=%q want 'hell'", b.String())
	}
}

func TestLimitWriter_Exact(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	w := limitWriter(&b, 5)
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if b.String() != "hello" {
		t.Errorf("buf=%q want hello", b.String())
	}
}
