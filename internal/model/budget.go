package model

import (
	"encoding/json"
	"fmt"
)

// Budget is the ceiling the whole fleet is held to on this host, as opposed to
// what any one pool is promised.
//
// It exists because a pool's own limits are per runner and say nothing about
// what happens when every pool is busy at once. Commitment already adds that up
// and shows it; this is the same figure turned into something the host
// enforces, so a fleet cannot take more than it was meant to whatever the pools
// were configured to do.
//
// Zero in any field means that dimension is not capped, which is what an
// install that has never been configured has. An empty budget therefore changes
// nothing, and the daemon behaves exactly as it did before one existed.
type Budget struct {
	// CPUs is how many processors the whole fleet may use together, as a
	// ceiling on throughput rather than a set of cores: four here means the
	// runners share four processors' worth of time, on whichever cores the
	// scheduler picks.
	CPUs int `json:"cpus"`
	// CPUWeight is the fleet's share when the host is contended, and is a
	// different question from the cap above. A cap is what the fleet may take
	// when nothing else wants the machine; the weight is what it gets when
	// something does. Both can be set: the fleet is then held to four
	// processors, and yields even those to a higher-weighted neighbour.
	//
	// systemd's default is 100. Zero here means "say nothing", which leaves the
	// default rather than writing it down.
	CPUWeight int `json:"cpuWeight"`
	// MemoryMB is the fleet's memory ceiling. It is applied as pressure rather
	// than as a wall — see HardMemory, which is the wall.
	MemoryMB int `json:"memoryMb"`
	// HardMemory adds a hard limit above the ceiling, past which the kernel
	// kills something.
	//
	// It is off by default, and that is the most considered decision in this
	// type. Every runner on this host shares one budget, so a hard limit is
	// enforced by the kernel's out-of-memory killer, which picks the largest
	// process in the group — some machine, not necessarily the one that
	// overspent. A job is failed, and it is somebody else's job. The soft
	// ceiling instead makes the kernel reclaim harder as the fleet approaches
	// it: the fleet slows down rather than losing work, which for continuous
	// integration is the better trade almost every time.
	//
	// It is offered at all because "slows down" is not free either: a fleet
	// held just under its ceiling by reclaim can thrash, and an operator who
	// would rather lose one job than have the host crawl should be able to say
	// so.
	HardMemory bool `json:"hardMemory"`
}

// HardMemoryHeadroom is how far the hard limit sits above the soft one.
//
// Not equal to it: the soft ceiling only does its job if there is somewhere to
// push back from. With both at the same figure the kernel would reclaim and
// kill at the same instant, which is a hard limit with extra steps.
const HardMemoryHeadroom = 5 // per cent

// MemoryBytes is the ceiling in bytes, and zero when memory is not capped.
func (b Budget) MemoryBytes() int64 { return int64(b.MemoryMB) * 1024 * 1024 }

// HardMemoryBytes is where the kernel would start killing, and zero when
// nothing would.
func (b Budget) HardMemoryBytes() int64 {
	if !b.HardMemory || b.MemoryMB == 0 {
		return 0
	}
	return b.MemoryBytes() * (100 + HardMemoryHeadroom) / 100
}

// Capped reports whether this budget constrains anything at all. A weight is
// deliberately not a cap: it decides who yields when the host is contended,
// which is not the same as a ceiling, and a fleet with only a weight set is
// still allowed to use the whole machine.
func (b Budget) Capped() bool { return b.CPUs > 0 || b.MemoryMB > 0 }

// Configured reports whether anything at all was asked for, cap or weight. It
// is what decides whether the host needs a group to put the fleet in.
func (b Budget) Configured() bool { return b.Capped() || b.CPUWeight > 0 }

// Limits on the budget itself, so a number that could only be a mistake is
// refused where somebody is there to read why.
const (
	// MaxBudgetCPUs is the same ceiling a single pool has. A budget larger than
	// any host is not wrong so much as meaningless.
	MaxBudgetCPUs = 1024
	// MinBudgetMemoryMB is below anything that could run a job. A budget under
	// it is a typo — a gigabyte entered in the wrong unit — and enforcing it
	// would give the fleet a ceiling no machine could boot inside.
	MinBudgetMemoryMB = 512
	MaxBudgetMemoryMB = 64 * 1024 * 1024
	// The range systemd accepts for cpu.weight.
	MinCPUWeight = 1
	MaxCPUWeight = 10000
)

// Validate reports why a budget cannot be used, in terms someone can act on.
func (b Budget) Validate() error {
	if b.CPUs < 0 || b.CPUs > MaxBudgetCPUs {
		return fmt.Errorf("cpus %d: want 0 to %d, where 0 is no processor cap at all", b.CPUs, MaxBudgetCPUs)
	}
	if b.MemoryMB != 0 && (b.MemoryMB < MinBudgetMemoryMB || b.MemoryMB > MaxBudgetMemoryMB) {
		return fmt.Errorf("memory %d MiB: want %d to %d, or 0 for no memory cap at all",
			b.MemoryMB, MinBudgetMemoryMB, MaxBudgetMemoryMB)
	}
	if b.CPUWeight != 0 && (b.CPUWeight < MinCPUWeight || b.CPUWeight > MaxCPUWeight) {
		return fmt.Errorf("cpu weight %d: want %d to %d, or 0 to leave systemd's default of 100",
			b.CPUWeight, MinCPUWeight, MaxCPUWeight)
	}
	// A hard limit with nothing to be hard about is not an error worth
	// refusing, but it is worth saying: it reads as though memory were capped.
	if b.HardMemory && b.MemoryMB == 0 {
		return fmt.Errorf("the hard memory limit needs a memory ceiling to sit above")
	}
	return nil
}

// SettingFleetBudget is the settings key the budget is stored under. It lives
// here rather than beside the daemon's other settings keys because two packages
// need it — the API writes it and the reconciler reads it — and neither should
// have to depend on the other for a string.
const SettingFleetBudget = "fleet.budget"

// ParseBudget reads a budget out of the settings table, where it is one JSON
// document under one key.
//
// An empty value is an install that has never set one, and is not an error: it
// is the uncapped budget, which is what every host had before this existed.
func ParseBudget(stored string) (Budget, error) {
	if stored == "" {
		return Budget{}, nil
	}
	var b Budget
	if err := json.Unmarshal([]byte(stored), &b); err != nil {
		return Budget{}, fmt.Errorf("read the fleet budget: %w", err)
	}
	return b, nil
}

// Encode is how a budget is stored.
func (b Budget) Encode() (string, error) {
	out, err := json.Marshal(b)
	return string(out), err
}
