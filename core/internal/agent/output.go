package agent

import (
	"bytes"
	"errors"
)

// Bounds of what the agent keeps of a Docker command's output. The child is
// always drained to its end; only the retained bytes are bounded.
const (
	// maxCommandOutput bounds standard output of commands other than logs
	// (compose config, ps): more is an error, not a truncation.
	maxCommandOutput = 16 << 20
	// maxStderr is how much of the end of standard error is kept.
	maxStderr = 64 << 10
	// logMargin is read beyond the log limit, so the cut can fall on a line
	// boundary.
	logMargin = 64 << 10
)

var errOutputTooLarge = errors.New("command output too large")

// tailBuffer keeps the last limit bytes written to it, in a ring.
type tailBuffer struct {
	buf   []byte
	start int // index of the oldest byte once full
	full  bool
	total int64
}

func newTailBuffer(limit int) *tailBuffer { return &tailBuffer{buf: make([]byte, 0, limit)} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	t.total += int64(n)
	limit := cap(t.buf)
	if len(p) >= limit {
		t.buf = append(t.buf[:0], p[len(p)-limit:]...)
		t.start, t.full = 0, true
		return n, nil
	}
	if !t.full {
		room := limit - len(t.buf)
		if len(p) <= room {
			t.buf = append(t.buf, p...)
			return n, nil
		}
		t.buf = append(t.buf, p[:room]...)
		p = p[room:]
		t.full = true
	}
	for len(p) > 0 {
		c := copy(t.buf[t.start:], p)
		p = p[c:]
		t.start = (t.start + c) % limit
	}
	return n, nil
}

// Bytes returns the retained bytes in order.
func (t *tailBuffer) Bytes() []byte {
	if !t.full {
		return bytes.Clone(t.buf)
	}
	out := make([]byte, 0, len(t.buf))
	out = append(out, t.buf[t.start:]...)
	return append(out, t.buf[:t.start]...)
}

// Truncated reports whether earlier bytes were dropped.
func (t *tailBuffer) Truncated() bool { return t.total > int64(len(t.buf)) }

// text returns the retained bytes. If earlier bytes were dropped, the first
// (cut) line is dropped too: a cut can fall inside a secret, and a fragment
// of a secret would escape redaction. Secret values never contain newlines.
func (t *tailBuffer) text() ([]byte, bool) {
	b := t.Bytes()
	if !t.Truncated() {
		return b, false
	}
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		return b[i+1:], true
	}
	return nil, true
}

// capBuffer keeps up to limit bytes and discards (but counts) the rest.
type capBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); room < len(p) {
		c.overflow = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

// truncatedMarker starts a log answer whose beginning was cut.
const truncatedMarker = "[earlier output truncated]\n"

// boundLogs redacts log output and bounds it to agentapi.MaxLogBytes,
// cutting only at line boundaries.
func boundLogs(out []byte, truncated bool, secrets []string, limit int) []byte {
	out = redact(out, secrets)
	if len(out) > limit-len(truncatedMarker) {
		out = out[len(out)-(limit-len(truncatedMarker)):]
		if i := bytes.IndexByte(out, '\n'); i >= 0 {
			out = out[i+1:]
		} else {
			out = nil
		}
		truncated = true
	}
	if truncated {
		out = append([]byte(truncatedMarker), out...)
	}
	return out
}
