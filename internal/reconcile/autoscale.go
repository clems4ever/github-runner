package reconcile

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/clems4ever/github-runner/internal/github"
	"github.com/clems4ever/github-runner/internal/model"
)

// ScaleDownAfter is how long a pool has to go without a single busy runner
// before it shrinks back to its minimum.
//
// Shrinking is deliberately the slow direction. Growing costs a machine that
// was going to be needed anyway; shrinking too eagerly means booting one again
// a minute later, and a VM takes seconds to start but a golden image takes
// minutes to build the first time. Waiting also rides out the gap between one
// job finishing and the next arriving, which on a busy repository is short.
const ScaleDownAfter = 5 * time.Minute

// Scale is what the autoscaler decided, and why. The reason is shown in the UI
// and logged: a fleet that resizes itself should never leave anyone guessing
// what it is reacting to.
type Scale struct {
	Target   int    `json:"target"`
	Floor    int    `json:"floor"`
	Ceiling  int    `json:"ceiling"`
	Reason   string `json:"reason"`
	ScaledUp bool   `json:"scaledUp"`
}

// Autoscale decides how many runners a pool should have right now.
//
// The shape of the problem is that GitHub does not tell anyone how many jobs
// are queued for a set of labels — only what each runner is doing. So demand
// is inferred: if every runner in the pool is busy, the next job to arrive has
// nowhere to go, and one more runner is added. That is also why the minimum is
// never zero. A pool with no runners has nothing to observe, so it could never
// learn that it should grow; keeping one idle runner is what makes the pool
// able to answer the question at all.
//
// It is a pure function of what was observed, so every rule below is a test
// rather than a description.
func Autoscale(p model.Pool, runners []Runner, states map[string]github.State, lastBusy, now time.Time) Scale {
	floor, ceiling := p.Floor(), p.Ceiling()
	scale := Scale{Floor: floor, Ceiling: ceiling}

	if !p.Enabled {
		scale.Reason = "the pool is switched off"
		return scale
	}

	// Runners that are draining are on their way out and cannot take a job, so
	// they are not capacity and they are not counted.
	live := livingRunners(runners)
	current := len(live)

	var busy int
	for _, runner := range live {
		if states[runner.Name] == github.StateBusy {
			busy++
		}
	}
	// Anything not known to be busy is capacity, including a runner that is
	// still booting. That is deliberate: counting a starting runner as absent
	// would add another every pass until the first one finished registering.
	free := current - busy

	switch {
	case current < floor:
		scale.Target = floor
		scale.Reason = "below the minimum"

	case free == 0 && current < ceiling:
		// Every runner is working, so the next job would queue. One at a time
		// rather than a jump to the ceiling: the pool climbs while demand
		// lasts, and a single long job does not conjure a full fleet.
		scale.Target = current + 1
		scale.ScaledUp = true
		scale.Reason = "every runner is busy"

	case free == 0 && current >= ceiling:
		scale.Target = ceiling
		// Worth distinguishing: a pool that cannot grow because it is at a
		// ceiling someone chose reads differently from one that was never
		// meant to grow at all.
		if p.Elastic() {
			scale.Reason = "every runner is busy, and the pool is at its maximum"
		} else {
			scale.Reason = "fixed size, and every runner is busy"
		}

	case busy == 0 && current > floor && !lastBusy.IsZero() && now.Sub(lastBusy) >= ScaleDownAfter:
		scale.Target = floor
		scale.Reason = "quiet for " + now.Sub(lastBusy).Round(time.Minute).String()

	case busy == 0 && current > floor:
		scale.Target = current
		scale.Reason = "idle, waiting to see if the quiet lasts"

	default:
		scale.Target = current
		if p.Elastic() {
			scale.Reason = "spare capacity available"
		} else {
			scale.Reason = "fixed size"
		}
	}

	if scale.Target < floor {
		scale.Target = floor
	}
	if scale.Target > ceiling {
		scale.Target = ceiling
	}
	return scale
}

// DesiredNames picks which runners a pool should have, given how many it
// should have.
//
// Which ones matter, not just how many. Shrinking must not pick a runner with
// a job on it — draining one is safe but pointless, since it would sit there
// occupied until the job ended while an idle runner stayed up. So busy runners
// are kept first, then the ones already running, and only then are new names
// invented. Names are reused rather than climbing for ever, so a pool that has
// scaled up and down for a week still reads web-1 to web-3.
func DesiredNames(p model.Pool, runners []Runner, states map[string]github.State, target int) []string {
	if target <= 0 {
		return nil
	}

	live := livingRunners(runners)
	sort.SliceStable(live, func(i, j int) bool {
		iBusy := states[live[i].Name] == github.StateBusy
		jBusy := states[live[j].Name] == github.StateBusy
		if iBusy != jBusy {
			return iBusy
		}
		iRunning := live[i].State == StateRunning
		jRunning := live[j].State == StateRunning
		if iRunning != jRunning {
			return iRunning
		}
		return runnerIndex(p.Name, live[i].Name) < runnerIndex(p.Name, live[j].Name)
	})

	taken := map[string]bool{}
	names := make([]string, 0, target)
	for _, runner := range live {
		if len(names) == target {
			break
		}
		names = append(names, runner.Name)
		taken[runner.Name] = true
	}

	// Short of the target: fill from the lowest unused index, so a pool that
	// lost its middle runner gets that name back rather than growing a tail.
	for index := 1; len(names) < target; index++ {
		name := p.RunnerName(index)
		if taken[name] {
			continue
		}
		names = append(names, name)
		taken[name] = true
	}

	sort.Slice(names, func(i, j int) bool {
		return runnerIndex(p.Name, names[i]) < runnerIndex(p.Name, names[j])
	})
	return names
}

func livingRunners(runners []Runner) []Runner {
	out := make([]Runner, 0, len(runners))
	for _, runner := range runners {
		if runner.State != StateStopping {
			out = append(out, runner)
		}
	}
	return out
}

// runnerIndex reads the number off a runner's name. A name that does not carry
// one sorts last rather than causing trouble.
func runnerIndex(pool, name string) int {
	suffix, ok := strings.CutPrefix(name, pool+"-")
	if !ok {
		return 1 << 30
	}
	index, err := strconv.Atoi(suffix)
	if err != nil {
		return 1 << 30
	}
	return index
}
