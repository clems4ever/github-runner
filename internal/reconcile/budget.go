package reconcile

import (
	"fmt"
	"sort"

	"github.com/clems4ever/github-runner/internal/model"
)

// Ration holds the fleet to its budget by deciding how much of the growth the
// autoscaler asked for the host can actually pay for.
//
// The slice on the host is the enforcement, and this is what keeps it from
// being reached. They are not the same job and neither replaces the other: a
// control group makes a fleet that overspends slower, and this makes it not
// overspend. Without the group a runaway pool would take the host; without
// this, a host at its ceiling would keep creating machines that make every
// machine already on it worse, for ever, because nothing in the autoscaler has
// any idea the ceiling exists.
//
// Two rules decide everything below.
//
// The first is that a pool's minimum is a promise, and the budget does not get
// to break it. A pool with no runner cannot accept a job, so it can never
// discover that it needs one — a budget that scaled pools to nothing would not
// slow the fleet down, it would switch it off, and nothing would ever switch it
// back on. Minimums are therefore paid first and paid whatever they cost, even
// when they cost more than the budget. An operator who has promised more
// minimums than the host can afford is told so, and the group on the host is
// what contains the result in the meantime.
//
// The second is that what is left over is shared out a runner at a time, in
// name order, round by round. A pool asking for six does not empty the budget
// before a pool asking for two is looked at; both get one, then both get
// another, until the money runs out. It is not the cleverest rule available —
// it knows nothing about which pool is busiest — but it is stable and it is
// explicable, and an operator who wants a particular pool to win can say so
// with the minimums, which are paid first.
//
// Container pools are counted by neither rule. They are not in the slice, so
// charging them against a ceiling they are not subject to would make the figure
// mean two different things at once.
func Ration(pools []model.Pool, scaling map[string]Scale, budget model.Budget) map[string]Scale {
	out := make(map[string]Scale, len(scaling))
	for name, scale := range scaling {
		out[name] = scale
	}
	if !budget.Capped() {
		return out
	}

	// Name order, so that two hosts with the same pools and the same budget
	// make the same decision, and so a test can assert on which pool got the
	// last runner.
	rationed := make([]model.Pool, 0, len(pools))
	for _, pool := range pools {
		if pool.Runtime != model.RuntimeVM {
			continue
		}
		if _, ok := scaling[pool.Name]; !ok {
			continue // its credential could not be read, and it was not sized
		}
		rationed = append(rationed, pool)
	}
	sort.Slice(rationed, func(i, j int) bool { return rationed[i].Name < rationed[j].Name })

	spent := purse{}
	granted := make(map[string]int, len(rationed))
	wanted := make(map[string]int, len(rationed))

	// The minimums, first and unconditionally.
	for _, pool := range rationed {
		want := scaling[pool.Name].Target
		floor := pool.Floor()
		if floor > want {
			floor = want
		}
		granted[pool.Name] = floor
		wanted[pool.Name] = want
		spent.add(pool, floor)
	}
	// Whether the minimums alone have already overrun, which changes what a
	// pool held at its minimum should be told: there is a difference between a
	// budget that is fully spent and one that was never large enough.
	overcommitted := !spent.fits(budget, 0, 0)

	// Then the growth, a runner at a time, round by round.
	for {
		granting := false
		for _, pool := range rationed {
			if granted[pool.Name] >= wanted[pool.Name] {
				continue
			}
			if !spent.fits(budget, pool.CPUs, pool.MemoryMB) {
				continue
			}
			granted[pool.Name]++
			spent.add(pool, 1)
			granting = true
		}
		if !granting {
			break
		}
	}

	for _, pool := range rationed {
		scale := out[pool.Name]
		// A budget too small for the minimums is worth saying on every pool,
		// even the ones nothing was taken from. That budget is not holding the
		// fleet to anything — it is being overrun by the minimums, and the group
		// on the host is the only thing containing the result. A pool sitting
		// quietly at its minimum is exactly where nobody would otherwise look.
		if granted[pool.Name] >= scale.Target && !overcommitted {
			continue
		}
		scale.Target = granted[pool.Name]
		// Not a scale-up any more, whatever the autoscaler decided. This is the
		// flag the daemon comes back in three seconds for, and coming back in
		// three seconds to be refused again is a busy loop that changes nothing.
		scale.ScaledUp = false
		scale.Reason = budgetReason(overcommitted, budget)
		out[pool.Name] = scale
	}
	return out
}

func budgetReason(overcommitted bool, budget model.Budget) string {
	if overcommitted {
		return fmt.Sprintf("the pools' minimums already exceed the fleet budget of %s,"+
			" so nothing can grow — raise the budget or lower a minimum", describe(budget))
	}
	return fmt.Sprintf("the fleet budget of %s is spent", describe(budget))
}

// describe says what the budget is, in the terms it was set in, so the reason
// on the pools page can be read without opening the settings page.
func describe(budget model.Budget) string {
	switch {
	case budget.CPUs > 0 && budget.MemoryMB > 0:
		return fmt.Sprintf("%d cpus and %d MiB", budget.CPUs, budget.MemoryMB)
	case budget.CPUs > 0:
		return fmt.Sprintf("%d cpus", budget.CPUs)
	default:
		return fmt.Sprintf("%d MiB", budget.MemoryMB)
	}
}

// purse is what the fleet has committed so far, in the two units a budget is
// written in.
type purse struct {
	cpus     int
	memoryMB int
}

func (p *purse) add(pool model.Pool, runners int) {
	p.cpus += runners * pool.CPUs
	p.memoryMB += runners * pool.MemoryMB
}

// fits reports whether one more runner of the given size stays inside the
// budget. A dimension the budget does not name is not a constraint — a budget
// that caps memory and not processors is a memory budget, not a refusal.
func (p *purse) fits(budget model.Budget, cpus, memoryMB int) bool {
	if budget.CPUs > 0 && p.cpus+cpus > budget.CPUs {
		return false
	}
	if budget.MemoryMB > 0 && p.memoryMB+memoryMB > budget.MemoryMB {
		return false
	}
	return true
}
