package model

import "time"

// ImagePhase is where one attempt at building a pool's golden image has got
// to.
//
// The two middle phases are the two slow parts, and they are told apart
// because they fail for different reasons and look different while they work:
// a download has no console to show, and a machine provisioning itself has
// nothing but.
type ImagePhase string

const (
	// ImageQueued is a build that has been asked for and is waiting for the
	// host to be free. One image is built at a time.
	ImageQueued ImagePhase = "queued"
	// ImageFetching is the stock Ubuntu image coming down, which happens once
	// per host.
	ImageFetching ImagePhase = "fetching"
	// ImageRunning is the build machine provisioning itself, which is where a
	// pool's recipe runs.
	ImageRunning ImagePhase = "running"
	// ImageSucceeded is an image that was published.
	ImageSucceeded ImagePhase = "succeeded"
	// ImageFailed is one that was not.
	ImageFailed ImagePhase = "failed"
)

// Who asked for a build.
const (
	// ImageAutomatic is the daemon building an image a pool needs and this
	// host has never had. It happens once per image: a build that fails is not
	// tried again on its own, because a recipe that cannot work would
	// otherwise fail for ever and bury the first failure — the one worth
	// reading — under a thousand identical ones.
	ImageAutomatic = "automatic"
	// ImageRequested is somebody pressing the button, which is how a failed
	// build is tried again.
	ImageRequested = "requested"
)

// ImageBuild is one attempt at building a pool's golden image, as it is kept.
//
// Kept, and not only while it matters: the log of a build that failed last
// Tuesday is the evidence for what a recipe did, and a fleet that threw it
// away left whoever changed that recipe with nothing to read.
type ImageBuild struct {
	ID int64 `json:"id"`
	// Pool is who wanted it. Two pools that ask for the same image share one
	// build; this is the one whose need started it.
	Pool string `json:"pool"`
	// Image is the file name, which is a hash of everything the image is built
	// from — so a pool that changed its recipe asks for a different one.
	Image   string     `json:"image"`
	Phase   ImagePhase `json:"phase"`
	Trigger string     `json:"trigger"`
	// Error is why it failed, in the words that would otherwise only be in the
	// journal.
	Error string `json:"error,omitempty"`
	// Log is where the whole account of the build was written: what the
	// builder did, and everything the build machine printed.
	Log       string     `json:"-"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
}

// Unfinished reports whether this build is still going, or waiting to.
func (b ImageBuild) Unfinished() bool {
	switch b.Phase {
	case ImageQueued, ImageFetching, ImageRunning:
		return true
	}
	return false
}

// Took is how long the build ran for, or how long it has been running.
func (b ImageBuild) Took(now time.Time) time.Duration {
	if b.EndedAt != nil {
		return b.EndedAt.Sub(b.StartedAt)
	}
	return now.Sub(b.StartedAt)
}
