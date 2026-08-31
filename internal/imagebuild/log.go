package imagebuild

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
)

// MaxLogBytes is how much of a build's log is served when nobody says
// otherwise.
//
// A build machine's console is a boot, an apt run and a recipe — megabytes of
// it — and a browser is not the place to read all of that. The end is what
// answers the question, and the whole file is on the host for the case where
// it is not.
const MaxLogBytes = 256 << 10

// journal is a build's log: one file, written by the builder and by the
// goroutine copying the build machine's console, so it is locked.
type journal struct {
	mu   sync.Mutex
	file *os.File
	path string
}

// journal opens the log for one build, named after it.
func (b *Builder) journal(id int64) (*journal, error) {
	if err := os.MkdirAll(b.LogsDir(), 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(b.LogsDir(), fmt.Sprintf("%d.log", id))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	return &journal{file: file, path: path}, nil
}

func (j *journal) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.file.Write(p)
}

func (j *journal) Path() string { return j.path }

func (j *journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.file.Close()
}

// say writes how a build ended at the end of its own log.
//
// The record says it too, but the record is not what somebody scrolling
// through six hundred lines of apt output is looking at — and a log that
// simply stops could mean the build failed, or that the daemon did.
func (b *Builder) say(build model.ImageBuild, err error, at time.Time) {
	if build.Log == "" {
		return
	}
	file, openErr := os.OpenFile(build.Log, os.O_APPEND|os.O_WRONLY, 0o600)
	if openErr != nil {
		return
	}
	defer file.Close()

	took := build.Took(at).Round(time.Second)
	if err != nil {
		fmt.Fprintf(file, "\n==> the build failed after %s: %v\n", took, err)
		return
	}
	fmt.Fprintf(file, "\n==> the build finished in %s\n", took)
}

// readTail returns the end of a file, made printable.
func readTail(path string, maxBytes int64) (string, error) {
	if maxBytes <= 0 {
		maxBytes = MaxLogBytes
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	offset := max(info.Size()-maxBytes, 0)
	buf := make([]byte, info.Size()-offset)
	if _, err := file.ReadAt(buf, offset); err != nil {
		return "", err
	}

	lines := strings.Split(string(buf), "\n")
	if offset > 0 && len(lines) > 1 {
		// The first line was cut in half by where the read started, and half a
		// line of somebody else's output reads like a mystery.
		lines = lines[1:]
	}
	for i, line := range lines {
		lines[i] = clean(line)
	}
	return strings.Join(lines, "\n"), nil
}

// tail is how much of a log is read to find its last line. The interesting
// part of a console is always the end of it.
const tail = 8 << 10

// lastLine returns the last thing a log said, and when it said it.
func lastLine(path string) (string, time.Time, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return "", time.Time{}, false
	}
	file, err := os.Open(path)
	if err != nil {
		return "", time.Time{}, false
	}
	defer file.Close()

	offset := max(info.Size()-tail, 0)
	buf := make([]byte, info.Size()-offset)
	if _, err := file.ReadAt(buf, offset); err != nil {
		return "", info.ModTime(), false
	}

	// A serial console is CRLF and carries the escape sequences of whatever
	// drew on it, so the last line is found after that is taken off rather
	// than before.
	lines := strings.Split(string(buf), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := clean(lines[i]); line != "" {
			return line, info.ModTime(), true
		}
	}
	return "", info.ModTime(), info.Size() > 0
}

// clean makes one console line printable: no carriage returns, no escape
// sequences, and short enough to sit on a line of a page rather than reflowing
// what is around it.
func clean(line string) string {
	line = strings.ReplaceAll(line, "\r", "")
	var b strings.Builder
	escape := false
	for _, r := range line {
		switch {
		case r == 0x1b:
			escape = true
		case escape:
			// An escape sequence ends at its first letter; everything up to
			// there is the parameters.
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				escape = false
			}
		case r == '\t':
			b.WriteRune(' ')
		case r >= 0x20:
			b.WriteRune(r)
		}
	}
	line = strings.TrimRight(b.String(), " ")
	const most = 2000
	if len(line) > most {
		line = line[:most] + "…"
	}
	return line
}

// short is one line of a log as a badge or a caption can carry it: the whole
// line is in the log, and this is the sentence beside the spinner.
func short(line string) string {
	line = strings.TrimSpace(line)
	const most = 160
	if len(line) > most {
		return line[:most] + "…"
	}
	return line
}
