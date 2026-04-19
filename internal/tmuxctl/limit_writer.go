package tmuxctl

import "io"

// limitWriter wraps w and silently drops any write beyond n bytes. The
// returned writer is safe for use by os/exec, which writes stderr from
// a separate goroutine. We intentionally swallow the overflow (rather
// than returning io.ErrShortWrite) because the only callers here are
// error-reporting paths; making the parent exec.Cmd.Run fail just
// because tmux printed too much to stderr would turn a routine error
// into a panic.
//
// limitWriter is not concurrency-safe on its own; os/exec serializes
// writes from the subprocess pipe goroutine, so a single limitWriter
// wrapping a caller's buffer is safe in the context this package uses.
func limitWriter(w io.Writer, n int64) io.Writer {
	return &boundedWriter{w: w, remaining: n}
}

type boundedWriter struct {
	w         io.Writer
	remaining int64
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	if b.remaining <= 0 {
		return len(p), nil
	}
	if int64(len(p)) <= b.remaining {
		n, err := b.w.Write(p)
		b.remaining -= int64(n)
		return n, err
	}
	n, err := b.w.Write(p[:b.remaining])
	b.remaining -= int64(n)
	if err != nil {
		return n, err
	}
	return len(p), nil
}
