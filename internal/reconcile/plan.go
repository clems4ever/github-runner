// Package reconcile turns the fleet's desired state into actions on the host.
//
// The daemon never owns a runner process. It creates units and containers,
// asks them to stop, and deletes them once they have; systemd and Docker do
// the supervising. That is what lets the daemon be restarted, upgraded or
// crash without touching a single running job — and it is why the plan below
// works from what the host reports rather than from anything remembered.
package reconcile

import (
	"sort"

	"github.com/clems4ever/github-runner/internal/github"
	"github.com/clems4ever/github-runner/internal/model"
)

// RunnerState is what the host says about a runner it is supervising.
type RunnerState string

const (
	// StateRunning means the unit or container is up.
	StateRunning RunnerState = "running"
	// StateStopping means a graceful stop is under way. Stopping is not quick
	// by design: it waits for the job in flight, which can take an hour.
	StateStopping RunnerState = "stopping"
	// StateStopped means it exists but is not running — a failed unit, or one
	// that has finished draining.
	StateStopped RunnerState = "stopped"
)

// Spec is everything an executor needs to build one runner. It is derived from
// a pool, once per runner, so an executor never has to reach back into the
// database.
type Spec struct {
	Name         string
	Pool         string
	PoolID       int64
	Generation   string
	Runtime      model.Runtime
	URL          string
	ScopeKind    model.ScopeKind
	Scope        string
	Labels       []string
	Ephemeral    bool
	Nested       bool
	CPUs         int
	MemoryMB     int
	DiskGB       int
	Image        string
	CredentialID int64
	// How the runner authenticates. An app's agent signs its own assertion and
	// buys an installation token, so it needs the app id as well as the key —
	// which is what lets a runner come back after a reboot with the daemon
	// still starting.
	CredentialKind model.CredentialKind
	AppID          int64
	InstallationID int64
	// RegistrationToken is minted by the daemon for runtimes that must not be
	// given the credential itself. It is filled in as an action is applied, not
	// when the plan is made: these expire in an hour, and a plan is not a
	// promise that anything will happen.
	RegistrationToken string
}

// Runner is what an executor found on the host.
//
// It carries its own scope and credential, not just its pool. A runner outlives
// the pool that created it — deleting a pool drains its runners rather than
// killing them — and during that window the pool is no longer in the database
// to say where the runner is registered. Without this the daemon could not ask
// GitHub whether a job was on it, which is exactly when it most needs to know.
type Runner struct {
	Name         string
	Pool         string
	Generation   string
	Runtime      model.Runtime
	State        RunnerState
	ScopeKind    model.ScopeKind
	Scope        string
	CredentialID int64
	// Trouble is what the host says is wrong with this runner, when it says
	// anything: a unit that keeps failing, a container that keeps exiting.
	//
	// It exists because a runner can be dead and busy-looking at the same time.
	// A crash-looping unit spends most of its life in systemd's "activating"
	// state, which read as running, so the fleet showed a healthy runner that
	// had never once registered — for as long as anyone cared to look. Nothing
	// plans on this field; it is carried so that a person is told.
	Trouble string
}

// Op is a single thing to do.
type Op string

const (
	// OpCreate builds a runner that should exist and does not.
	OpCreate Op = "create"
	// OpStart brings back one that exists, is wanted, and is not running.
	// Whether that means starting it or rebuilding it is the executor's
	// business.
	OpStart Op = "start"
	// OpDrain asks a runner to stop when its job is done. It returns
	// immediately; the stop happens in the background and shows up as
	// StateStopping until it is over.
	OpDrain Op = "drain"
	// OpRemove deletes a runner that has stopped, and deregisters it.
	OpRemove Op = "remove"
)

// Action is one step of a plan.
type Action struct {
	Op      Op
	Runner  string
	Pool    string
	Runtime model.Runtime
	Spec    *Spec // set for OpCreate
	Reason  string
}

// Plan works out what to do, and is deliberately a pure function: given the
// same desired state, host state and GitHub state it always returns the same
// actions, which is what makes the fleet's behaviour testable without a
// hypervisor, a container daemon or a network.
//
// The rules, in the order they matter:
//
//   - A runner that is wanted and absent is created.
//   - A runner whose generation no longer matches its pool is running the
//     wrong configuration. It is drained, not killed, and only removed once it
//     has stopped — a configuration change must never fail a job.
//   - A runner that is no longer wanted goes the same way.
//   - A runner that is wanted, correct and stopped is started again. That is
//     what brings a fleet back after a reboot, and what recovers a unit that
//     failed.
//   - A runner that is busy is never removed, whatever else is true.
func Plan(desired []Spec, actual []Runner, states map[string]github.State) []Action {
	wanted := make(map[string]Spec, len(desired))
	for _, spec := range desired {
		wanted[spec.Name] = spec
	}
	found := make(map[string]Runner, len(actual))
	for _, runner := range actual {
		found[runner.Name] = runner
	}

	var actions []Action

	// Sorted so a plan reads the same way twice and tests can compare it
	// whole. Maps in Go do not promise an order, and a plan that shuffles is a
	// plan nobody can review.
	for _, spec := range sortedSpecs(desired) {
		current, exists := found[spec.Name]
		if !exists {
			actions = append(actions, Action{
				Op: OpCreate, Runner: spec.Name, Pool: spec.Pool, Runtime: spec.Runtime,
				Spec: specPtr(spec), Reason: "wanted but not on this host",
			})
			continue
		}

		if current.Generation != spec.Generation {
			// The pool was reconfigured. Replacing means draining first, and
			// the create waits for a later pass once the old one is gone.
			switch current.State {
			case StateRunning:
				actions = append(actions, Action{
					Op: OpDrain, Runner: spec.Name, Pool: spec.Pool, Runtime: current.Runtime,
					Reason: "configuration changed",
				})
			case StateStopped:
				if states[spec.Name] == github.StateBusy {
					continue
				}
				actions = append(actions,
					Action{Op: OpRemove, Runner: spec.Name, Pool: spec.Pool, Runtime: current.Runtime,
						Reason: "configuration changed"},
					Action{Op: OpCreate, Runner: spec.Name, Pool: spec.Pool, Runtime: spec.Runtime,
						Spec: specPtr(spec), Reason: "replacing the previous configuration"},
				)
			}
			continue
		}

		if current.State == StateStopped {
			// The spec goes with it: a container cannot simply be started
			// again, because the token it registered with has expired, so its
			// executor rebuilds it from this.
			actions = append(actions, Action{
				Op: OpStart, Runner: spec.Name, Pool: spec.Pool, Runtime: current.Runtime,
				Spec: specPtr(spec), Reason: "wanted but not running",
			})
		}
		// Running with the right generation: nothing to do. This is the case
		// that matters most, because it is what a daemon restart sees for
		// every runner already on the host — it adopts them rather than
		// rebuilding the fleet underneath the jobs.
	}

	for _, current := range sortedRunners(actual) {
		if _, stillWanted := wanted[current.Name]; stillWanted {
			continue
		}
		reason := "no longer wanted"
		switch current.State {
		case StateRunning:
			actions = append(actions, Action{
				Op: OpDrain, Runner: current.Name, Pool: current.Pool, Runtime: current.Runtime,
				Reason: reason,
			})
		case StateStopped:
			// The last guard against failing a job: a stopped unit whose
			// runner GitHub still reports as busy is a contradiction, and the
			// safe reading of a contradiction is to wait.
			if states[current.Name] == github.StateBusy {
				continue
			}
			actions = append(actions, Action{
				Op: OpRemove, Runner: current.Name, Pool: current.Pool, Runtime: current.Runtime,
				Reason: reason,
			})
		}
	}

	return actions
}

func specPtr(s Spec) *Spec { return &s }

func sortedSpecs(in []Spec) []Spec {
	out := append([]Spec(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func sortedRunners(in []Runner) []Runner {
	out := append([]Runner(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SpecsFor turns a pool and the names the autoscaler chose into the runners it
// should have. The names are decided elsewhere, because how many a pool needs
// depends on what its runners are doing and this does not.
func SpecsFor(p model.Pool, credentialFingerprint string, names []string) []Spec {
	return SpecsForCredential(p, credentialFingerprint, names, model.Secret{Kind: model.CredentialPAT})
}

// SpecsForCredential is SpecsFor with the credential's shape, which the agent
// needs in order to authenticate without the daemon.
func SpecsForCredential(p model.Pool, credentialFingerprint string, names []string, secret model.Secret) []Spec {
	generation := p.Generation(credentialFingerprint)
	labels := p.EffectiveLabels()

	var specs []Spec
	for _, name := range names {
		specs = append(specs, Spec{
			Name:           name,
			Pool:           p.Name,
			PoolID:         p.ID,
			Generation:     generation,
			Runtime:        p.Runtime,
			URL:            p.URL(),
			ScopeKind:      p.ScopeKind,
			Scope:          p.Scope,
			Labels:         labels,
			Ephemeral:      p.Ephemeral,
			Nested:         p.Nested,
			CPUs:           p.CPUs,
			MemoryMB:       p.MemoryMB,
			DiskGB:         p.DiskGB,
			Image:          p.Image,
			CredentialID:   p.CredentialID,
			CredentialKind: secret.Kind,
			AppID:          secret.AppID,
			InstallationID: secret.InstallationID,
		})
	}
	return specs
}
