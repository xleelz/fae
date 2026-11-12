package secretshape

import (
	"bytes"
	"io"
)

// writerFlood is the most text a Writer holds without a line break before it
// writes all but a hold's worth of it.
const writerFlood = 1 << 20

// Writer copies a provider's raw output to its log with every credential
// Spans finds, and every one of values, replaced by Mark (FAE-SEC-003). It
// writes whole lines: it holds the text after the last line break until a
// later Write completes the line or Flush, except that a line longer than
// writerFlood is written all but the text a credential starting there could
// still need. A value is found only as its own bytes.
type Writer struct {
	dst    io.Writer
	values [][]byte
	buf    []byte
	err    error
}

// NewWriter is a Writer to dst for values; an empty value is ignored.
func NewWriter(dst io.Writer, values [][]byte) *Writer {
	w := &Writer{dst: dst}
	for _, v := range values {
		if len(v) > 0 {
			w.values = append(w.values, bytes.Clone(v))
		}
	}
	return w
}

// Write takes p whole; a failed write to dst is returned, now and by every
// later call.
func (w *Writer) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	w.buf = append(w.buf, p...)
	if i := bytes.LastIndexByte(w.buf, '\n'); i >= 0 {
		w.emit(i+1, false)
	} else if len(w.buf) > writerFlood {
		w.emit(len(w.buf)-w.hold(), false)
	}
	if w.err != nil {
		return 0, w.err
	}
	return len(p), nil
}

// Flush writes the text held back.
func (w *Writer) Flush() error {
	if w.err == nil && len(w.buf) > 0 {
		w.emit(len(w.buf), true)
	}
	return w.err
}

// hold is the most text one credential or value found spans.
func (w *Writer) hold() int {
	hold := Max
	for _, v := range w.values {
		hold = max(hold, len(v))
	}
	return hold
}

// emit writes buf up to n, redacted. Unless final, a credential found across
// n is held back whole.
func (w *Writer) emit(n int, final bool) {
	spans := w.spans(w.buf)
	if !final {
		for _, sp := range spans {
			if sp[0] < n && sp[1] > n {
				n = sp[0]
			}
		}
	}
	out := make([]byte, 0, n)
	at := 0
	for _, sp := range spans {
		if sp[1] > n {
			break
		}
		out = append(append(out, w.buf[at:sp[0]]...), Mark...)
		at = sp[1]
	}
	if out = append(out, w.buf[at:n]...); len(out) > 0 {
		if _, w.err = w.dst.Write(out); w.err != nil {
			return
		}
	}
	w.buf = w.buf[:copy(w.buf, w.buf[n:])]
}

// spans are b's credentials and values, in order and apart.
func (w *Writer) spans(b []byte) [][2]int {
	spans := Spans(b)
	for _, v := range w.values {
		var found [][2]int
		for at := 0; ; {
			i := bytes.Index(b[at:], v)
			if i < 0 {
				break
			}
			found = append(found, [2]int{at + i, at + i + len(v)})
			at += i + len(v)
		}
		spans = Merge(spans, found)
	}
	return spans
}
