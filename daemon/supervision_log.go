package daemon

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// FAE-OBS-001: every supervision line carries an RFC 3339 timestamp with zone,
// the instance and a component, and goes to a rotating private file under the
// instance root and to standard error. Writes never fail: a log fault must not
// stop the daemon or break a child's output pipe (Dolt writes through here).
// The standard error copy is queued and drops lines rather than block.
var supervisionStderr io.Writer = os.Stderr

const (
	supervisionLogLimit    = 64 << 20
	supervisionLogKeep     = 4
	supervisionLineMax     = 64 << 10
	supervisionStderrQueue = 4096
)

var ignoreBrokenStderr sync.Once

// logWriter is the daemon's supervision log handle (a plain file in fixtures).
type logWriter interface {
	Write([]byte) (int, error)
	Close() error
}

type supervisionLog struct {
	mu          sync.Mutex
	path        string
	instance    string
	file        *os.File
	size, limit int64
	keep        int
	copies      chan []byte
	copied      chan struct{}
	dropped     int64
	closed      bool
}

func openSupervisionLog(path, instance string, stderr io.Writer) (logWriter, error) {
	l := &supervisionLog{path: path, instance: instance, limit: supervisionLogLimit, keep: supervisionLogKeep}
	if err := l.open(); err != nil {
		return nil, err
	}
	if stderr == io.Writer(os.Stderr) {
		if r := serveLogRotator(); r != nil {
			stderr = r
		}
	}
	if stderr != nil {
		l.copies, l.copied = make(chan []byte, supervisionStderrQueue), make(chan struct{})
		go func() {
			defer close(l.copied)
			for b := range l.copies {
				_, _ = stderr.Write(b)
			}
		}()
	}
	// A closed stderr pipe must cost only the copy, never the daemon (SIGPIPE).
	ignoreBrokenStderr.Do(func() { signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE) })
	l.line("daemon", "supervision log opened pid="+strconv.Itoa(os.Getpid()))
	return l, nil
}
func (l *supervisionLog) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err == nil && (!info.Mode().IsRegular() || info.Mode().Perm() != 0600) {
		err = fmt.Errorf("unsafe supervision log %s", l.path)
	}
	if err != nil {
		f.Close()
		return err
	}
	l.file, l.size = f, info.Size()
	return nil
}

// rotate keeps keep older files (path.1 newest) and reopens path.
func (l *supervisionLog) rotate() {
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
	for i := l.keep - 1; i >= 1; i-- {
		_ = os.Rename(l.path+"."+strconv.Itoa(i), l.path+"."+strconv.Itoa(i+1))
	}
	if l.keep > 0 {
		_ = os.Rename(l.path, l.path+".1")
	} else {
		_ = os.Remove(l.path)
	}
	_ = l.open()
}
func (l *supervisionLog) line(component, text string) {
	b := make([]byte, 0, len(text)+96)
	b = time.Now().UTC().AppendFormat(b, time.RFC3339Nano)
	b = append(b, " instance="...)
	b = append(b, l.instance...)
	b = append(b, " component="...)
	b = append(b, component...)
	b = append(b, ' ')
	b = append(b, text...)
	b = append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	if l.copies != nil {
		select {
		case l.copies <- b:
		default:
			l.dropped++
		}
	}
	if l.size > 0 && l.size+int64(len(b)) > l.limit {
		l.rotate()
	}
	if l.file != nil {
		n, _ := l.file.Write(b)
		l.size += int64(n)
	}
}
func (l *supervisionLog) lines(component string, p []byte) {
	for len(p) > 0 {
		text, rest, _ := bytes.Cut(p, []byte{'\n'})
		l.line(component, string(bytes.TrimRight(text, "\r")))
		p = rest
	}
}

// Write records each line of p as the daemon component; a trailing partial
// line is a complete message (callers format whole messages).
func (l *supervisionLog) Write(p []byte) (int, error) {
	l.lines("daemon", p)
	return len(p), nil
}
func (l *supervisionLog) Close() error {
	l.mu.Lock()
	dropped := l.dropped
	l.mu.Unlock()
	l.line("daemon", "supervision log closed stderr_dropped="+strconv.FormatInt(dropped, 10))
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	var err error
	if l.file != nil {
		err = l.file.Close()
	}
	if l.copies != nil {
		close(l.copies)
	}
	l.mu.Unlock()
	if l.copied != nil {
		select { // drain briefly; a stalled stderr keeps only its goroutine
		case <-l.copied:
		case <-time.After(time.Second):
		}
	}
	return err
}

// componentWriter names the component of its lines. A stream (a child's
// stdout) buffers partial lines and splits lines over supervisionLineMax;
// otherwise each Write is a whole message, as from fmt.Fprintf.
type componentWriter struct {
	mu        sync.Mutex
	log       *supervisionLog
	component string
	stream    bool
	partial   []byte
}

func (c *componentWriter) Write(p []byte) (int, error) {
	if !c.stream {
		c.log.lines(c.component, p)
		return len(p), nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.partial = append(c.partial, p...)
	for {
		i := bytes.IndexByte(c.partial, '\n')
		if i < 0 && len(c.partial) < supervisionLineMax {
			break
		}
		if i < 0 || i > supervisionLineMax {
			i = supervisionLineMax
		}
		c.log.line(c.component, string(bytes.TrimRight(c.partial[:i], "\r")))
		if i < len(c.partial) && c.partial[i] == '\n' {
			i++
		}
		c.partial = append(c.partial[:0], c.partial[i:]...)
	}
	return len(p), nil
}

// componentLog names the component for messages written to w; componentStream
// does so for a byte stream. Writers that are not the supervision log (test
// fixtures) pass through unchanged.
func componentLog(w io.Writer, component string) io.Writer {
	if l, ok := w.(*supervisionLog); ok {
		return &componentWriter{log: l, component: component}
	}
	return w
}
func componentStream(w io.Writer, component string) io.Writer {
	if l, ok := w.(*supervisionLog); ok {
		return &componentWriter{log: l, component: component, stream: true}
	}
	return w
}

// logLine writes one component message; a daemon without a log (an
// admission fixture) drops it.
func (d *Daemon) logLine(component, format string, args ...any) {
	if d.log != nil {
		fmt.Fprintf(componentLog(d.log, component), format, args...)
	}
}

// logTask formats the FAE-OBS-001 correlation fields for a task-scoped line.
func logTask(task, stage, turn string) string {
	return fmt.Sprintf("task=%q stage=%q turn=%q", task, stage, turn)
}
