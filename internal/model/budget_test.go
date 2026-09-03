package model

import (
	"strings"
	"testing"
)

// The budget every host has before anyone sets one. It has to change nothing at
// all, or installing an upgrade would quietly cap a fleet that was working.
func TestAnUnsetBudgetCapsNothing(t *testing.T) {
	var budget Budget

	if budget.Capped() {
		t.Error("a budget nobody set is enforcing something")
	}
	if budget.Configured() {
		t.Error("a budget nobody set counts as configured")
	}
	if err := budget.Validate(); err != nil {
		t.Errorf("the empty budget is invalid: %v", err)
	}
	if budget.MemoryBytes() != 0 || budget.HardMemoryBytes() != 0 {
		t.Error("the empty budget has limits in it")
	}
}

// Nothing stored is nothing set, not a parse failure. This is what every
// install has on the first pass after the upgrade that introduced budgets.
func TestNothingStoredIsTheEmptyBudget(t *testing.T) {
	budget, err := ParseBudget("")
	if err != nil {
		t.Fatalf("an install that has never set a budget is an error: %v", err)
	}
	if budget != (Budget{}) {
		t.Fatalf("got %+v", budget)
	}
}

func TestABudgetSurvivesTheSettingsTable(t *testing.T) {
	want := Budget{CPUs: 8, CPUWeight: 50, MemoryMB: 24576, HardMemory: true}

	encoded, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseBudget(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("stored %+v, read back %+v", want, got)
	}
}

// A settings row that is not a budget must not be silently read as one: a
// half-parsed budget would be an arbitrary cap on a real fleet.
func TestNonsenseInTheSettingsTableIsAnError(t *testing.T) {
	if _, err := ParseBudget("{{{"); err == nil {
		t.Fatal("anything that parses as JSON-ish was accepted as a budget")
	}
}

// A weight decides who yields when the host is contended. It is not a ceiling,
// and calling it one would have the rationing refuse to grow a fleet that is
// allowed to use the whole machine.
func TestAWeightIsNotACap(t *testing.T) {
	budget := Budget{CPUWeight: 20}
	if budget.Capped() {
		t.Error("a weight was mistaken for a cap")
	}
	if !budget.Configured() {
		t.Error("a weight is something to write on the slice, so it is configured")
	}
}

// Either dimension alone is a budget. A host with plenty of memory and few
// cores is the ordinary reason to set one.
func TestEitherDimensionAloneIsACap(t *testing.T) {
	for _, budget := range []Budget{{CPUs: 4}, {MemoryMB: 4096}} {
		if !budget.Capped() {
			t.Errorf("%+v does not count as a cap", budget)
		}
	}
}

// The hard limit sits above the soft one rather than on it. With both at the
// same figure the kernel would reclaim and kill at the same instant, which is a
// hard limit with extra steps and none of the pressure that was the point.
func TestTheHardLimitLeavesTheSoftOneRoomToWork(t *testing.T) {
	budget := Budget{MemoryMB: 10000, HardMemory: true}

	high := budget.MemoryBytes()
	max := budget.HardMemoryBytes()
	if max <= high {
		t.Fatalf("the wall is at %d and the pressure starts at %d, so the pressure never happens", max, high)
	}
	if want := high * (100 + HardMemoryHeadroom) / 100; max != want {
		t.Fatalf("hard limit is %d, want %d", max, want)
	}
}

// Off by default, and the reason is worth keeping in a test: the kernel's
// out-of-memory killer picks the largest machine in the group, which is not the
// one that overspent. The default costs minutes; this costs somebody's job.
func TestTheKillerIsOffUnlessAskedFor(t *testing.T) {
	budget := Budget{MemoryMB: 8192}
	if budget.HardMemoryBytes() != 0 {
		t.Fatal("a memory budget brought the out-of-memory killer with it")
	}
}

// A wall with nothing to stand on is a setting that reads as a memory cap and
// is not one.
func TestAHardLimitWithoutACeilingIsRefused(t *testing.T) {
	err := Budget{HardMemory: true}.Validate()
	if err == nil {
		t.Fatal("a hard limit above no limit at all was accepted")
	}
	if !strings.Contains(err.Error(), "ceiling") {
		t.Errorf("the message does not say what is missing: %v", err)
	}
}

func TestABudgetThatCouldOnlyBeAMistakeIsRefused(t *testing.T) {
	for name, budget := range map[string]Budget{
		"negative processors":  {CPUs: -1},
		"more than any host":   {CPUs: MaxBudgetCPUs + 1},
		"a gigabyte as MiB":    {MemoryMB: 8},
		"more memory than any": {MemoryMB: MaxBudgetMemoryMB + 1},
		"a weight of zero-ish": {CPUWeight: -5},
		"a disk under one VM":  {DiskGB: 20},
		"more disk than any":   {DiskGB: MaxBudgetDiskGB + 1},
		"a weight off the end": {CPUWeight: MaxCPUWeight + 1},
	} {
		if err := budget.Validate(); err == nil {
			t.Errorf("%s was accepted: %+v", name, budget)
		}
	}
}

// The message has to say what to do about it, because it is shown in a form
// somebody is filling in.
func TestTheMessageSaysWhatWouldBeAcceptable(t *testing.T) {
	err := Budget{MemoryMB: 8}.Validate()
	if err == nil {
		t.Fatal("half a megabyte of fleet was accepted")
	}
	for _, want := range []string{"512", "0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not mention %q: %v", want, err)
		}
	}
}

// Zero is how each dimension is switched off, and has to pass validation or
// there would be no way to remove a cap once set.
func TestZeroIsHowACapIsRemoved(t *testing.T) {
	for _, budget := range []Budget{
		{CPUs: 0, MemoryMB: 4096},
		{CPUs: 4, MemoryMB: 0},
		{DiskGB: 0, CPUs: 4},
		{DiskGB: 200},
		{},
	} {
		if err := budget.Validate(); err != nil {
			t.Errorf("%+v: %v", budget, err)
		}
	}
}
