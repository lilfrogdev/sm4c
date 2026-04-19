package tmuxctl

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// DefaultMaxLineSize caps the length of a single control-mode line. tmux
// does not publish a hard upper bound, but 1 MiB comfortably holds a full
// terminal screen's worth of escape-encoded bytes and keeps an adversarial
// or malfunctioning tmux from driving us OOM.
const DefaultMaxLineSize = 1 << 20

// Parser turns a tmux control-mode byte stream into a stream of Event
// values. It is stateful: begin/end blocks affect how subsequent
// non-`%`-prefixed lines are classified.
//
// Parser is NOT safe for concurrent use. Callers that want to fan events
// out to multiple consumers should wrap it with a single owning goroutine.
type Parser struct {
	r           *bufio.Reader
	maxLine     int
	inBlock     bool
	blockCmdNum uint64
}

// NewParser returns a Parser that reads from r using the default limits.
func NewParser(r io.Reader) *Parser {
	return &Parser{
		r:       bufio.NewReaderSize(r, 64*1024),
		maxLine: DefaultMaxLineSize,
	}
}

// SetMaxLine overrides the per-line byte cap. Intended for tests.
func (p *Parser) SetMaxLine(n int) { p.maxLine = n }

// InBlock reports whether the parser is currently between a BeginEvent
// and its matching EndEvent/ErrorEvent. Exposed for tests and for the
// Client's request-pairing logic.
func (p *Parser) InBlock() bool { return p.inBlock }

// Next returns the next Event from the stream. It returns io.EOF when
// the stream ends cleanly (no bytes buffered). Any other error indicates
// a protocol violation (malformed escape, oversized line, garbled
// notification header) and should be treated as fatal.
func (p *Parser) Next() (Event, error) {
	line, err := p.readLine()
	if err != nil {
		return nil, err
	}
	return p.classify(line)
}

// readLine returns a single line with the trailing CR/LF stripped. It
// returns io.EOF only when EOF is reached with no buffered bytes.
func (p *Parser) readLine() ([]byte, error) {
	line, err := p.r.ReadBytes('\n')
	if len(line) > p.maxLine {
		return nil, fmt.Errorf("tmuxctl: line exceeds max %d bytes", p.maxLine)
	}
	switch {
	case err == nil:
		line = line[:len(line)-1]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		return line, nil
	case errors.Is(err, io.EOF) && len(line) == 0:
		return nil, io.EOF
	case errors.Is(err, io.EOF):
		return line, nil
	default:
		return nil, err
	}
}

func (p *Parser) classify(line []byte) (Event, error) {
	if len(line) == 0 || line[0] != '%' {
		if p.inBlock {
			return DataEvent{Line: copyBytes(line)}, nil
		}
		return RawEvent{Line: string(line)}, nil
	}

	verb, rest := splitVerb(line[1:])
	switch string(verb) {
	case "begin":
		ts, num, fl, err := parseHeader(rest)
		if err != nil {
			return nil, fmt.Errorf("tmuxctl: %%begin: %w", err)
		}
		p.inBlock = true
		p.blockCmdNum = num
		return BeginEvent{Time: ts, CommandNum: num, Flags: fl}, nil

	case "end":
		ts, num, fl, err := parseHeader(rest)
		if err != nil {
			return nil, fmt.Errorf("tmuxctl: %%end: %w", err)
		}
		p.inBlock = false
		return EndEvent{Time: ts, CommandNum: num, Flags: fl}, nil

	case "error":
		ts, num, fl, err := parseHeader(rest)
		if err != nil {
			return nil, fmt.Errorf("tmuxctl: %%error: %w", err)
		}
		p.inBlock = false
		return ErrorEvent{Time: ts, CommandNum: num, Flags: fl}, nil

	case "output":
		return parseOutput(rest)

	case "exit":
		return ExitEvent{Reason: string(bytes.TrimSpace(rest))}, nil

	default:
		args := splitArgs(rest)
		return NotificationEvent{Kind: string(verb), Args: args}, nil
	}
}

// parseHeader parses the "<time> <command-num> <flags>" tail of a
// %begin / %end / %error line.
func parseHeader(b []byte) (ts int64, num uint64, flags uint64, err error) {
	fields := bytes.Fields(b)
	if len(fields) < 3 {
		return 0, 0, 0, fmt.Errorf("expected 3 fields, got %d (%q)", len(fields), b)
	}
	ts, err = strconv.ParseInt(string(fields[0]), 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("time: %w", err)
	}
	num, err = strconv.ParseUint(string(fields[1]), 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("command-num: %w", err)
	}
	flags, err = strconv.ParseUint(string(fields[2]), 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("flags: %w", err)
	}
	return ts, num, flags, nil
}

// parseOutput decodes `%<pane-id> <escaped-bytes>`.
func parseOutput(rest []byte) (Event, error) {
	rest = bytes.TrimLeft(rest, " ")
	if len(rest) == 0 {
		return nil, fmt.Errorf("tmuxctl: %%output: empty payload")
	}
	if rest[0] != '%' {
		return nil, fmt.Errorf("tmuxctl: %%output: expected %%<pane-id>, got %q", rest)
	}
	sp := bytes.IndexByte(rest, ' ')
	var paneID string
	var rawData []byte
	if sp < 0 {
		paneID = string(rest)
	} else {
		paneID = string(rest[:sp])
		rawData = rest[sp+1:]
	}
	if !isPaneID(paneID) {
		return nil, fmt.Errorf("tmuxctl: %%output: malformed pane id %q", paneID)
	}
	data, err := Unescape(rawData)
	if err != nil {
		return nil, fmt.Errorf("tmuxctl: %%output: %w", err)
	}
	return OutputEvent{PaneID: paneID, Data: data}, nil
}

// isPaneID validates %N pane identifiers. tmux uses %N for pane IDs, @N
// for window IDs, and $N for session IDs; only %N is legal in %output.
func isPaneID(s string) bool {
	if len(s) < 2 || s[0] != '%' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// splitVerb returns the first ASCII-alphabetic/dash word of s and the
// remainder. A verb ends at the first space or end-of-line; the caller is
// expected to trim the remainder if it cares about leading whitespace.
func splitVerb(s []byte) (verb, rest []byte) {
	i := 0
	for i < len(s) && s[i] != ' ' {
		i++
	}
	verb = s[:i]
	if i >= len(s) {
		return verb, nil
	}
	return verb, s[i+1:]
}

// splitArgs splits a notification tail on ASCII whitespace. Returns nil
// (not an empty slice) when s contains no tokens so a zero-arg
// notification round-trips cleanly.
func splitArgs(s []byte) []string {
	trimmed := strings.TrimSpace(string(s))
	if trimmed == "" {
		return nil
	}
	return strings.Fields(trimmed)
}

func copyBytes(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
