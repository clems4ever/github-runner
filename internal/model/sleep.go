package model

import "fmt"

// A pool that never goes below one runner keeps a machine up all night for the
// repository that pushes twice a week. On a host with several such pools that
// is most of the host, spent on nothing — which is the complaint this exists
// to answer.
//
// The reason the floor was one was not thrift. It was that GitHub does not
// announce queued jobs: the fleet infers demand from what its own runners are
// doing, and a pool with no runners has nothing to observe. Keeping one idle
// runner was what made the pool able to notice it needed a second.
//
// A pool that may sleep replaces that observer with a question asked directly
// — see github.QueuedJobs. It costs two requests a pass while nothing is
// waiting, and it can only be asked of a repository, which is why sleeping is
// a repository pool's option and not an organisation pool's.

// SleepAllowed reports whether this pool could sleep at all.
//
// Not a matter of policy. Waking up means reading the queue, GitHub lists runs
// per repository, and an organisation's queue would mean crawling every
// repository in it on every pass. An organisation pool with no runners would
// have no way to find out that anything wanted it.
func (p *Pool) SleepAllowed() bool { return p.ScopeKind == ScopeRepository }

// Sleeping reports whether this pool is one that goes to zero when nothing is
// queued.
func (p *Pool) Sleeping() bool { return p.Sleeps && p.SleepAllowed() }

// ValidateSleep checks a pool's sleep setting against what the pool is.
func ValidateSleep(p Pool) error {
	if !p.Sleeps {
		return nil
	}
	if !p.SleepAllowed() {
		// Said rather than quietly ignored: a pool configured for something it
		// cannot do is a pool somebody is waiting on.
		return fmt.Errorf("an organisation pool cannot sleep: " +
			"GitHub lists queued jobs per repository, so a pool with no runners " +
			"would have no way to find out that anything wanted it")
	}
	if p.MaxReplicas < 1 {
		return fmt.Errorf("maximum replicas %d: a pool that sleeps needs somewhere to wake up to", p.MaxReplicas)
	}
	return nil
}
