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

// NewRunLogger returns a logger that writes to both stderr and an internal buffer.
func NewRunLogger() *RunLogger {
	rl := &RunLogger{}
	rl.Logger = log.New(io.MultiWriter(os.Stderr, &rl.buf), "", log.LstdFlags)
	return rl
}

// String returns all captured log output accumulated so far.
func (rl *RunLogger) String() string { return rl.buf.String() }
