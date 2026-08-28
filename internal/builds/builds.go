// Package builds reads what the agents have written about image builds, so
// the fleet page can say what a pool with no runners is waiting for.
//
// The daemon does not build images — a runner's own unit does, on the host,
// before the machine exists. This is the daemon reading their notes.
package builds

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/clems4ever/github-runner/internal/agent"
)

// Silence is how long a running build may say nothing before that is worth
// reporting.
//
// Not a timeout and not a diagnosis: the agent has its own deadline and gives
// up long before this matters. It is the difference between "this is taking a
// while" and "nothing has happened for a quarter of an hour", which is the
// question somebody watching a progress display is actually asking. A recipe
// downloading a large toolchain is quiet for minutes at a time, so this is
// generous on purpose.
const Silence = 15 * time.Minute

// keepDone is how long a finished build stays interesting. A successful build
// is worth showing while the person who triggered it is still watching, and
// noise the next morning.
const keepDone = time.Hour

// Build is one image build as the UI receives it: the agent's record, plus
// what the daemon can see about it now.
type Build struct {
	Image  string           `json:"image"`
	Pool   string           `json:"pool"`
	Runner string           `json:"runner"`
	Phase  agent.BuildPhase `json:"phase"`
	// Detail is what the build is doing, in its own words: the last line its
	// console printed, or the size of the download that has not started
	// printing yet. This is the answer to "it has been four minutes and I
	// cannot tell whether it is working".
	Detail string `json:"detail,omitempty"`
	Error  string `json:"error,omitempty"`
	// Console is where the whole account of a failed build was kept.
	Console string `json:"console,omitempty"`
	// Silent says a running build has printed nothing for a long time. It does
	// not say the build is dead — only that it has stopped saying otherwise,
	// which is what the page can honestly report.
	Silent    bool       `json:"silent,omitempty"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	// Seconds is how long it has been running, or how long it took.
	Seconds int `json:"seconds"`
}

// Reader reports the builds on this host.
type Reader struct {
	imagesDir string
	// now is injectable so the tests can be about elapsed time rather than
	// about sleeping.
	now func() time.Time
}

// New reads the builds under an images directory.
func New(imagesDir string) *Reader {
	return &Reader{imagesDir: imagesDir, now: time.Now}
}

// List reports what is building and what last happened to each pool's image.
//
// One record per pool, the newest, and that is the whole retention policy. A
// failure that was fixed is superseded by the build that fixed it; a failure
// that was not is still the last thing that happened to that pool, which is
// exactly what should still be on the page.
func (r *Reader) List() ([]Build, error) {
	entries, err := os.ReadDir(agent.BuildsDir(r.imagesDir))
	if os.IsNotExist(err) {
		// No host has built anything yet. Not an error — it is the answer.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	now := r.now()
	newest := map[string]Build{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		record, err := r.read(filepath.Join(agent.BuildsDir(r.imagesDir), entry.Name()))
		if err != nil {
			// A record that cannot be read is a record, not a fleet: the page
			// is better off missing one line than failing to load.
			continue
		}
		if record.Phase == agent.BuildDone && record.EndedAt != nil &&
			now.Sub(*record.EndedAt) > keepDone {
			continue
		}
		if seen, ok := newest[record.Pool]; ok && seen.StartedAt.After(record.StartedAt) {
			continue
		}
		newest[record.Pool] = r.describe(record, now)
	}

	out := make([]Build, 0, len(newest))
	for _, build := range newest {
		out = append(out, build)
	}
	// Unfinished first, because that is the one somebody is waiting on, then
	// newest first within each group.
	sort.Slice(out, func(i, j int) bool {
		if running(out[i]) != running(out[j]) {
			return running(out[i])
		}
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out, nil
}

func running(b Build) bool {
	return b.Phase == agent.BuildFetching || b.Phase == agent.BuildRunning
}

func (r *Reader) read(path string) (agent.BuildRecord, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return agent.BuildRecord{}, err
	}
	var record agent.BuildRecord
	if err := json.Unmarshal(body, &record); err != nil {
		return agent.BuildRecord{}, err
	}
	if record.Pool == "" || record.Image == "" {
		return agent.BuildRecord{}, fmt.Errorf("%s names neither a pool nor an image", path)
	}
	return record, nil
}

// describe turns a record into what the page shows, which for a build still
// happening means asking the host what it is doing right now.
func (r *Reader) describe(record agent.BuildRecord, now time.Time) Build {
	b := Build{
		Image:     record.Image,
		Pool:      record.Pool,
		Runner:    record.Runner,
		Phase:     record.Phase,
		Error:     record.Error,
		Console:   record.Console,
		StartedAt: record.StartedAt,
		EndedAt:   record.EndedAt,
	}
	b.Seconds = int(now.Sub(record.StartedAt).Seconds())
	if record.EndedAt != nil {
		b.Seconds = int(record.EndedAt.Sub(record.StartedAt).Seconds())
	}
	if !running(b) {
		return b
	}

	// The build directory is a fixed path — one build at a time, under a lock
	// — so the console of whatever is happening now is always here.
	console := filepath.Join(r.imagesDir, "build", "console.log")
	if line, at, ok := lastLine(console); ok {
		b.Detail = line
		b.Silent = now.Sub(at) > Silence
		return b
	}

	// No console yet, which is the download: the machine has not booted, so
	// there is nothing to print. The file's size is the only progress there is.
	if info, err := os.Stat(filepath.Join(r.imagesDir, "cloud-"+agent.UbuntuRelease+".img")); err == nil {
		b.Detail = fmt.Sprintf("fetching the Ubuntu image, %d MB so far", info.Size()/(1<<20))
		b.Silent = now.Sub(info.ModTime()) > Silence
		return b
	}
	b.Silent = now.Sub(record.StartedAt) > Silence
	return b
}

// tail is how much of the console is read to find its last line. A console is
// megabytes of boot output and the interesting part is the end of it.
const tail = 8 << 10

// lastLine returns the last thing a console said, and when it said it.
func lastLine(path string) (string, time.Time, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return "", time.Time{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", time.Time{}, false
	}
	defer f.Close()

	size := info.Size()
	offset := max(size-tail, 0)
	buf := make([]byte, size-offset)
	if _, err := f.ReadAt(buf, offset); err != nil {
		return "", time.Time{}, false
	}

	// A serial console is CRLF and carries the escape sequences of whatever
	// drew on it, so the last line is found after that is taken off rather
	// than before.
	for _, line := range reverse(strings.Split(string(buf), "\n")) {
		line = clean(line)
		if line == "" {
			continue
		}
		return line, info.ModTime(), true
	}
	return "", info.ModTime(), false
}

func reverse(lines []string) []string {
	out := make([]string, 0, len(lines))
	for i := len(lines) - 1; i >= 0; i-- {
		out = append(out, lines[i])
	}
	return out
}

// clean makes one console line printable: no carriage returns, no escape
// sequences, and short enough to sit on a line of the page rather than
// reflowing the card around it.
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
	line = strings.TrimSpace(b.String())
	const most = 160
	if len(line) > most {
		line = line[:most] + "…"
	}
	return line
}
