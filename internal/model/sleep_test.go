package model

import (
	"strings"
	"testing"
)

// A sleeping pool is a repository pool that goes to zero. The whole point: an
// idle host with three such pools is an idle host, not three machines waiting
// for a push that comes twice a week.
func TestASleepingPoolMayReachZero(t *testing.T) {
	p := validPool()
	p.Sleeps = true
	p.MinReplicas, p.MaxReplicas = 0, 4
	p.Defaults()

	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.Floor() != 0 {
		t.Fatalf("floor is %d on a pool that is allowed to sleep", p.Floor())
	}
	if p.Ceiling() != 4 {
		t.Fatalf("ceiling is %d, so there is nothing to wake up to", p.Ceiling())
	}
}

// Sleeping is not all-or-nothing. A pool that keeps one runner warm and grows
// to eight is the same setting with a different floor, and the floor is still
// whatever was asked for.
func TestASleepingPoolKeepsAFloorItWasGiven(t *testing.T) {
	p := validPool()
	p.Sleeps = true
	p.MinReplicas, p.MaxReplicas = 1, 8
	p.Defaults()

	if p.Floor() != 1 {
		t.Fatalf("floor is %d, want the one it was configured with", p.Floor())
	}
}

// Said rather than quietly ignored. GitHub lists runs per repository, so an
// organisation pool at zero would have nothing to ask and nothing to observe —
// it would simply never come back.
func TestAnOrganisationPoolCannotSleep(t *testing.T) {
	p := validPool()
	p.ScopeKind, p.Scope = ScopeOrganization, "runyard-ai"
	p.Sleeps = true
	p.MinReplicas, p.MaxReplicas = 0, 4

	err := p.Validate()
	if err == nil {
		t.Fatal("accepted an organisation pool that sleeps")
	}
	if !strings.Contains(err.Error(), "per repository") {
		t.Fatalf("refused with %q, which does not say why", err)
	}
}

// The setting is stored, so a pool that had it and was moved to an
// organisation would carry it. Sleeping() is what everything else asks, and it
// answers for what the pool actually is rather than for what it was told.
func TestSleepingIgnoresASettingThePoolCannotHonour(t *testing.T) {
	p := validPool()
	p.ScopeKind, p.Scope = ScopeOrganization, "runyard-ai"
	p.Sleeps = true
	p.MinReplicas = 0

	if p.Sleeping() {
		t.Fatal("an organisation pool reported itself as sleeping")
	}
	if p.Floor() != 1 {
		t.Fatalf("floor is %d; an organisation pool that cannot wake must not go to zero", p.Floor())
	}
}

// A pool that sleeps and may never start anything is not a pool. Corrected
// rather than refused: the maximum a caller left at zero is an omission, and
// the minimum is the number they were thinking about.
func TestASleepingPoolIsGivenSomewhereToWakeUpTo(t *testing.T) {
	p := Pool{
		Name: "web", ScopeKind: ScopeRepository, Scope: "clems4ever/runyard",
		Runtime: RuntimeVM, Sleeps: true, CredentialID: 1, Enabled: true,
	}
	p.Defaults()

	if p.MaxReplicas < 1 {
		t.Fatalf("maximum is %d, so nothing could ever start", p.MaxReplicas)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}

// A pool that does not sleep still gets the old correction, because a minimum
// of zero on such a pool is a pool that can never grow.
func TestAPoolThatDoesNotSleepIsStillHeldAtOne(t *testing.T) {
	p := Pool{
		Name: "web", ScopeKind: ScopeRepository, Scope: "clems4ever/runyard",
		Runtime: RuntimeVM, CredentialID: 1, Enabled: true,
	}
	p.Defaults()

	if p.MinReplicas != 1 {
		t.Fatalf("minimum is %d on a pool that does not sleep", p.MinReplicas)
	}
}
