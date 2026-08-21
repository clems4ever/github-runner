package model

import (
	"strings"
	"testing"
)

func validPool() Pool {
	p := Pool{
		Name:         "web",
		ScopeKind:    ScopeRepository,
		Scope:        "clems4ever/runyard",
		Runtime:      RuntimeVM,
		Replicas:     3,
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
		{"negative replicas", func(p *Pool) { p.Replicas = -1 }, "replicas"},
		{"too many replicas", func(p *Pool) { p.Replicas = MaxReplicas + 1 }, "replicas"},
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

func TestDesiredRunnerNames(t *testing.T) {
	p := validPool()
	got := p.DesiredRunnerNames()
	want := []string{"web-1", "web-2", "web-3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}

	// A disabled pool wants nothing running, which is how the UI's off switch
	// drains a pool without losing its configuration.
	p.Enabled = false
	if names := p.DesiredRunnerNames(); len(names) != 0 {
		t.Fatalf("a disabled pool still wants %v", names)
	}
}

func TestEffectiveLabelsDescribeTheRunner(t *testing.T) {
	p := validPool()
	p.Runtime = RuntimeContainer
	p.Nested = true
	p.Ephemeral = true
	p.Labels = []string{"gpu", "eu-west"}

	got := strings.Join(p.EffectiveLabels(), ",")
	want := "container,nested,ephemeral,gpu,eu-west"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A workflow asking for "nested" must not reach a pool that only calls itself
// that, so the automatic labels follow the settings rather than the name.
func TestEffectiveLabelsFollowTheConfiguration(t *testing.T) {
	p := validPool()
	p.Nested = false
	for _, label := range p.EffectiveLabels() {
		if label == "nested" {
			t.Fatal("a pool without nested virtualisation labelled itself nested")
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

	// Scaling must not replace the runners that are already correct.
	t.Run("replicas do not", func(t *testing.T) {
		p := validPool()
		p.Replicas = 10
		if p.Generation("fp") != baseGen {
			t.Fatal("scaling changed the generation, which would replace every healthy runner")
		}
	})
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
