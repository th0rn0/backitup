package client

import (
	"bytes"
	"io"
	"log"
	"os"
)

// RunLogger captures all log output to an internal buffer (sent to the server
// as LogTail at the end of the run) while also writing to stderr so the
// operator can see progress locally.
type RunLogger struct {
	*log.Logger
	buf bytes.Buffer
}

// NewRunLogger returns a logger that writes to an internal buffer. When quiet
// is false (the default) it also writes to stderr so progress is visible
// locally. Set BACKITUP_QUIET=1 to suppress stderr output.
func NewRunLogger(quiet bool) *RunLogger {
	rl := &RunLogger{}
	var out io.Writer = os.Stderr
	if quiet {
		out = io.Discard
	}
	rl.Logger = log.New(io.MultiWriter(out, &rl.buf), "", log.LstdFlags)
	return rl
}

// String returns all captured log output accumulated so far.
func (rl *RunLogger) String() string { return rl.buf.String() }
