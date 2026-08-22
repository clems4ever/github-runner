package resources

import (
	"runtime"
	"sync"
	"time"
)

// Rate turns a cumulative processor-time counter into a percentage.
//
// Both runtimes report the same shape of number — nanoseconds of CPU consumed
// since the thing started — and neither reports a rate, so the arithmetic is
// the same for a container and for a machine and lives here once. The previous
// reading per runner is the state that makes it possible, which is why this is
// an object rather than a function.
type Rate struct {
	// cores is what the percentage is a fraction of, so that a runner's figure
	// and the host's are on the same scale and can be read against each other.
	// Four saturated cores out of eight is 50 in both places.
	cores int

	mu   sync.Mutex
	last map[string]reading
}

type reading struct {
	consumed uint64
	at       time.Time
}

// NewRate builds a tracker against this host's processor count.
func NewRate() *Rate {
	cores := runtime.NumCPU()
	if cores < 1 {
		cores = 1
	}
	return &Rate{cores: cores, last: map[string]reading{}}
}

// Percent records a reading and returns the share of the host that runner has
// been using since the previous one, or nil when there was no previous one.
func (r *Rate) Percent(name string, consumed uint64) *float64 {
	return r.percentAt(name, consumed, time.Now())
}

func (r *Rate) percentAt(name string, consumed uint64, at time.Time) *float64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	previous, seen := r.last[name]
	r.last[name] = reading{consumed: consumed, at: at}

	// First sight of this runner. A rate needs two readings and there is one,
	// so nothing is reported rather than a zero that would read as idle.
	if !seen {
		return nil
	}
	elapsed := at.Sub(previous.at)
	// A counter that went backwards is a name reused by a new runner — the
	// fleet rebuilds a machine under the same name every time it starts one —
	// and the window before it existed is not this runner's to account for.
	if elapsed <= 0 || consumed < previous.consumed {
		return nil
	}

	percent := clampPercent(float64(consumed-previous.consumed) /
		(float64(elapsed.Nanoseconds()) * float64(r.cores)) * 100)
	return &percent
}

// Keep drops every runner but these, so a fleet that has churned through a
// thousand ephemeral machines does not carry a thousand readings.
func (r *Rate) Keep(names []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	live := make(map[string]bool, len(names))
	for _, name := range names {
		live[name] = true
	}
	for name := range r.last {
		if !live[name] {
			delete(r.last, name)
		}
	}
}
