package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// A golden image build is the one thing this daemon does that takes minutes
// and shows nothing while it does it.
//
// The build happens in the runner's own unit, on the host, before the machine
// it is for exists. So the fleet page has nothing to say about it: the pool is
// short of a runner, the runner that would explain why has not booted, and the
// only account of what is happening is a journal nobody is reading. A build
// that FAILS is worse — the pool sits at zero, the daemon retries it on the
// next pass, and the UI reports a pool that will not come up without ever
// saying that its image did not build.
//
// So the agent writes down what it is doing, where the daemon can read it.

// BuildPhase is where a build has got to.
type BuildPhase string

const (
	// BuildFetching is the stock Ubuntu image coming down. It happens once per
	// host and it is the slowest part of a first build, with nothing else to
	// see: no machine has booted, so there is no console yet.
	BuildFetching BuildPhase = "fetching"
	// BuildRunning is the machine running its provisioning. This is where a
	// recipe runs, and where a console exists to say what it is doing.
	BuildRunning BuildPhase = "running"
	// BuildDone is an image that was published.
	BuildDone BuildPhase = "done"
	// BuildFailed is one that was not, and the record is kept until the next
	// build of the same image replaces it. A failure that vanishes when the
	// next reconcile pass starts over is a failure nobody can read.
	BuildFailed BuildPhase = "failed"
)

// BuildRecord is one image build, as the agent writes it and the daemon reads
// it. It is the whole protocol between them: a file per image, replaced
// atomically, holding no more than what somebody looking at the fleet needs
// to be told.
type BuildRecord struct {
	// Image is the golden image's file name, which is the hash of everything
	// it is built from — so two builds of the same thing share a record and a
	// pool that changed its recipe gets a new one.
	Image string `json:"image"`
	// Pool and Runner are who asked. A build serves whoever needs it first,
	// and other pools wanting the same image wait on the same lock rather than
	// starting one of their own.
	Pool   string     `json:"pool"`
	Runner string     `json:"runner"`
	Phase  BuildPhase `json:"phase"`
	// Error is why it failed, in the words the agent would otherwise have put
	// only in the journal.
	Error string `json:"error,omitempty"`
	// Console is where the whole account of a failed build was kept, since the
	// directory it happened in is deleted with the build.
	Console   string     `json:"console,omitempty"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
}

// BuildFor names who a build is for, so the fleet page can say whose image is
// building rather than naming a hash.
//
// Deliberately not part of ImageSpec: the image's name is a hash of the spec,
// and which runner happened to ask for it first has no business changing what
// the image is. Folding these two fields in there would give every runner its
// own image and rebuild the fleet one machine at a time.
type BuildFor struct {
	Pool   string
	Runner string
}

// BuildsDir is where the records live, beside the images they describe.
func BuildsDir(imagesDir string) string { return filepath.Join(imagesDir, "builds") }

// buildJournal writes one build's record as it goes.
//
// Every method is best effort: a build that cannot write its record must still
// build the image. Reporting is worth an image; it is not worth a fleet.
type buildJournal struct {
	path   string
	record BuildRecord
}

// startBuild opens a journal for a build about to happen.
func startBuild(imagesDir, image, pool, runner string, now time.Time) *buildJournal {
	j := &buildJournal{
		path: filepath.Join(BuildsDir(imagesDir), image+".json"),
		record: BuildRecord{
			Image:     image,
			Pool:      pool,
			Runner:    runner,
			Phase:     BuildFetching,
			StartedAt: now,
		},
	}
	j.write()
	return j
}

// phase records that the build has moved on.
func (j *buildJournal) phase(p BuildPhase) {
	if j == nil {
		return
	}
	j.record.Phase = p
	j.write()
}

// finish records how it ended. An err of nil is a published image.
func (j *buildJournal) finish(err error, console string, now time.Time) {
	if j == nil {
		return
	}
	j.record.Phase = BuildDone
	if err != nil {
		j.record.Phase = BuildFailed
		j.record.Error = err.Error()
	}
	j.record.Console = console
	j.record.EndedAt = &now
	j.write()
}

// write replaces the record atomically, so a daemon reading it while it is
// written never sees half a document.
func (j *buildJournal) write() {
	if j == nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(j.path), 0o700); err != nil {
		return
	}
	body, err := json.Marshal(j.record)
	if err != nil {
		return
	}
	tmp := j.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, j.path)
}
