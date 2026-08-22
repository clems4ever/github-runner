package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

func validPool() Pool {
	p := Pool{
		Name:        "web",
		ScopeKind:   ScopeRepository,
		Scope:       "clems4ever/runyard",
		Runtime:     RuntimeVM,
		MinReplicas: 3, MaxReplicas: 3,
		CredentialID: 1,
		Enabled:      true,
	}
	p.Defaults()
	return p
}

func TestValidateAcceptsAWorkablePool(t *testing.T) {
	p := validPool()
	if err := p.Validate(); err != nil {
		t.Fatalf("a valid pool was rejected: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Pool)
		want   string
	}{
		{"empty name", func(p *Pool) { p.Name = "" }, "name"},
		{"upper case name", func(p *Pool) { p.Name = "Web" }, "name"},
		{"name with a slash", func(p *Pool) { p.Name = "web/1" }, "name"},
		{"trailing dash", func(p *Pool) { p.Name = "web-" }, "name"},
		{"repository without owner", func(p *Pool) { p.Scope = "runyard" }, "repository is owner/name"},
		{"organisation with a slash", func(p *Pool) { p.ScopeKind = ScopeOrganization; p.Scope = "o/r" }, "single name"},
		{"unknown scope kind", func(p *Pool) { p.ScopeKind = "user" }, "scope kind"},
		{"unknown runtime", func(p *Pool) { p.Runtime = "podman" }, "runtime"},
		{"a minimum of zero", func(p *Pool) { p.MinReplicas = 0 }, "minimum replicas"},
		{"more than the host will take", func(p *Pool) { p.MaxReplicas = MaxReplicas + 1 }, "maximum replicas"},
		{"a maximum below the minimum", func(p *Pool) { p.MinReplicas, p.MaxReplicas = 4, 2 }, "never below the minimum"},
		{"no cpus", func(p *Pool) { p.CPUs = 0 }, "cpus"},
		{"absurd memory", func(p *Pool) { p.MemoryMB = 64 }, "memory"},
		{"absurd disk", func(p *Pool) { p.DiskGB = 1 }, "disk"},
		{"no credential", func(p *Pool) { p.CredentialID = 0 }, "credential"},
		{"label with a space", func(p *Pool) { p.Labels = []string{"big machine"} }, "label"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validPool()
			tt.mutate(&p)
			err := p.Validate()
			if err == nil {
				t.Fatal("accepted a pool that cannot work")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("the message does not say what is wrong: got %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// A container pool has no disk of its own, so the disk limits do not apply.
func TestValidateIgnoresDiskForContainers(t *testing.T) {
	p := validPool()
	p.Runtime = RuntimeContainer
	p.DiskGB = 0
	if err := p.Validate(); err != nil {
		t.Fatalf("a container pool needs no disk size: %v", err)
	}
}

func TestBounds(t *testing.T) {
	p := validPool()
	if p.RunnerName(2) != "web-2" {
		t.Fatalf("got %q", p.RunnerName(2))
	}

	// A disabled pool wants nothing running, which is how the UI's off switch
	// drains a pool without losing its configuration.
	p.Enabled = false
	if p.Floor() != 0 || p.Ceiling() != 0 {
		t.Fatalf("a disabled pool still wants %d to %d runners", p.Floor(), p.Ceiling())
	}
}

// A pool must never be allowed to reach zero while it is enabled: with no
// runner there is nothing to accept a job, and so nothing to reveal that the
// pool needs to grow.
func TestFloorIsNeverZeroWhileEnabled(t *testing.T) {
	p := validPool()
	p.MinReplicas = 0
	if p.Floor() != 1 {
		t.Fatalf("floor is %d", p.Floor())
	}
}

func TestElastic(t *testing.T) {
	p := validPool()
	p.MinReplicas, p.MaxReplicas = 2, 2
	if p.Elastic() {
		t.Fatal("a pool with equal bounds is a fixed size")
	}
	p.MaxReplicas = 3
	if !p.Elastic() {
		t.Fatal("a pool with room above its minimum is elastic")
	}
}

func TestEffectiveLabelsDescribeTheRunner(t *testing.T) {
	p := validPool()
	p.Runtime = RuntimeContainer
	p.Nested = true
	p.Ephemeral = true
	p.Labels = []string{"gpu", "eu-west"}

	got := strings.Join(p.EffectiveLabels(), ",")
	want := "container,nestedvirt,ephemeral,gpu,eu-west"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A workflow asking for "nestedvirt" must not reach a pool that only calls
// itself that, so the automatic labels follow the settings rather than the name.
func TestEffectiveLabelsFollowTheConfiguration(t *testing.T) {
	p := validPool()
	p.Nested = false
	for _, label := range p.EffectiveLabels() {
		if label == "nestedvirt" {
			t.Fatal("a pool without nested virtualisation labelled itself nestedvirt")
		}
	}
}

// The label was "nested" until it was found to say nothing about what is
// nested. Nobody should reintroduce it as a second spelling: two labels for one
// capability is how a workflow ends up on a runner that cannot do the work.
func TestNestedIsNoLongerALabel(t *testing.T) {
	p := validPool()
	p.Nested = true
	for _, label := range p.EffectiveLabels() {
		if label == "nested" {
			t.Fatal("the old label is back; workflows must target nestedvirt")
		}
	}
}

func TestEffectiveLabelsDeduplicate(t *testing.T) {
	p := validPool()
	p.Runtime = RuntimeVM
	p.Labels = []string{"vm", "VM", "custom"}
	got := strings.Join(p.EffectiveLabels(), ",")
	if got != "vm,custom" {
		t.Fatalf("got %q, want the automatic label kept once", got)
	}
}

func TestGenerationChangesWithTheConfiguration(t *testing.T) {
	base := validPool()
	baseGen := base.Generation("fp")

	tests := []struct {
		name   string
		mutate func(*Pool)
	}{
		{"labels", func(p *Pool) { p.Labels = []string{"gpu"} }},
		{"nested", func(p *Pool) { p.Nested = true }},
		{"ephemeral", func(p *Pool) { p.Ephemeral = true }},
		{"runtime", func(p *Pool) { p.Runtime = RuntimeContainer }},
		{"scope", func(p *Pool) { p.Scope = "clems4ever/other" }},
		{"cpus", func(p *Pool) { p.CPUs = 8 }},
		{"memory", func(p *Pool) { p.MemoryMB = 8192 }},
		{"disk", func(p *Pool) { p.DiskGB = 100 }},
		{"image", func(p *Pool) { p.Image = "runyard-toolchain" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validPool()
			tt.mutate(&p)
			if p.Generation("fp") == baseGen {
				t.Fatalf("changing %s left the generation alone, so runners would keep the old configuration", tt.name)
			}
		})
	}

	t.Run("credential", func(t *testing.T) {
		if base.Generation("other-fingerprint") == baseGen {
			t.Fatal("replacing the credential left the generation alone")
		}
	})

	// Scaling must not replace the runners that are already correct — and the
	// autoscaler moves these numbers many times an hour.
	t.Run("the scaling bounds do not", func(t *testing.T) {
		p := validPool()
		p.MinReplicas, p.MaxReplicas = 2, 10
		if p.Generation("fp") != baseGen {
			t.Fatal("changing the scaling bounds changed the generation, which would replace every healthy runner")
		}
	})
}

// An upgrade that changes how runners are built has to replace the ones
// already on the host. The configuration hash cannot notice that on its own —
// the pool is identical before and after — so the revision is part of it.
func TestGenerationCoversHowRunnersAreBuilt(t *testing.T) {
	p := validPool()
	generation := p.Generation("fp")

	// What the hash would be if the revision had not moved: if these are ever
	// equal, an upgrade that fixes the recipe leaves every runner on the old
	// one, and somebody has to go and delete them by hand.
	previous := poolGenerationAtRevision(p, "fp", SpecRevision-1)
	if generation == previous {
		t.Fatal("the build revision is not part of the generation")
	}
}

// poolGenerationAtRevision recomputes a pool's generation as an earlier
// revision of the daemon would have.
func poolGenerationAtRevision(p Pool, fingerprint string, revision int) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, part := range parts {
			h.Write([]byte(part))
			h.Write([]byte{0})
		}
	}
	write(
		fmt.Sprint(revision),
		p.Name, string(p.ScopeKind), p.Scope, string(p.Runtime),
		fmt.Sprint(p.Nested), fmt.Sprint(p.Ephemeral),
		strings.Join(p.EffectiveLabels(), ","),
		fmt.Sprint(p.CPUs), fmt.Sprint(p.MemoryMB), fmt.Sprint(p.DiskGB),
		p.Image, fingerprint,
	)
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func TestGenerationIsStable(t *testing.T) {
	p := validPool()
	if p.Generation("fp") != p.Generation("fp") {
		t.Fatal("the same pool hashed differently twice")
	}
	if len(p.Generation("fp")) != 12 {
		t.Fatalf("want a short readable hash, got %q", p.Generation("fp"))
	}
}

func TestDefaults(t *testing.T) {
	p := Pool{Name: "x", Scope: "o/r", CredentialID: 1}
	p.Defaults()

	if p.Runtime != RuntimeVM {
		t.Errorf("runtime defaulted to %q", p.Runtime)
	}
	if p.ScopeKind != ScopeRepository {
		t.Errorf("scope kind defaulted to %q", p.ScopeKind)
	}
	if p.CPUs != 2 || p.MemoryMB != 4096 || p.DiskGB != 40 {
		t.Errorf("sizing defaulted to %d/%d/%d", p.CPUs, p.MemoryMB, p.DiskGB)
	}
	if p.Image == "" {
		t.Error("no default image, so the executor would have nothing to resolve")
	}
}

func TestURL(t *testing.T) {
	p := validPool()
	if got := p.URL(); got != "https://github.com/clems4ever/runyard" {
		t.Fatalf("got %q", got)
	}
	p.ScopeKind, p.Scope = ScopeOrganization, "runyard-ai"
	if got := p.URL(); got != "https://github.com/runyard-ai" {
		t.Fatalf("got %q", got)
	}
}
