// Package model holds what the daemon is asked to maintain: pools of runners
// and the credentials they register with.
//
// These types are the desired state. What actually exists on the host lives in
// systemd and Docker, and is read back from them rather than tracked here — a
// database row saying a runner is running is a guess, and after a daemon
// restart or a reboot it is usually a wrong one.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Runtime is what a runner runs in.
type Runtime string

const (
	// RuntimeVM boots a QEMU virtual machine per runner. A job gets a kernel
	// of its own to break.
	RuntimeVM Runtime = "vm"
	// RuntimeContainer runs the runner in a Docker container. Cheaper and
	// faster to start; a weaker boundary.
	RuntimeContainer Runtime = "container"
)

// ScopeKind is where the runner registers.
type ScopeKind string

const (
	// ScopeRepository registers on one repository.
	ScopeRepository ScopeKind = "repository"
	// ScopeOrganization registers on an organisation, where every repository
	// in it can use the runner.
	ScopeOrganization ScopeKind = "organization"
)

// Limits the UI and the API both enforce, so a pool that cannot work is
// rejected where someone is there to read the message.
const (
	MaxReplicas = 64
	MinCPUs     = 1
	MaxCPUs     = 64
	MinMemoryMB = 512
	MaxMemoryMB = 512 * 1024
	MinDiskGB   = 10
	MaxDiskGB   = 4000
)

var (
	// Pool names end up in unit names, container names and runner names, so
	// they are restricted to what all three accept without escaping.
	poolNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$|^[a-z0-9]$`)
	labelRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	segmentRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// CredentialKind is how a credential proves who it is.
type CredentialKind string

const (
	// CredentialPAT is a personal access token: one string, belonging to a
	// person, expiring on whatever date they chose.
	CredentialPAT CredentialKind = "pat"
	// CredentialApp is a GitHub App: an id and a private key, which the daemon
	// exchanges for a short-lived installation token whenever it needs one.
	// Nothing expires on a calendar, the repositories it can reach are a list
	// that can be edited without touching the credential, and uninstalling the
	// app revokes everything at once.
	CredentialApp CredentialKind = "app"
)

// Credential is how the fleet proves who it is to GitHub. The secret itself is
// never in this struct in clear: Sealed is what the database holds, and only
// the daemon's keyring can open it.
type Credential struct {
	ID   int64          `json:"id"`
	Name string         `json:"name"`
	Kind CredentialKind `json:"kind"`
	// AppID and InstallationID are only meaningful for an app. An installation
	// of zero means "find it", which is the usual case: an app installed on one
	// account has one installation, and making someone look up its id is a step
	// with no decision in it.
	AppID          int64     `json:"appId,omitempty"`
	InstallationID int64     `json:"installationId,omitempty"`
	Sealed         string    `json:"-"`
	Hint           string    `json:"hint"` // enough to tell two apart, no more
	CreatedAt      time.Time `json:"createdAt"`
}

// Secret is a credential with its secret opened, for the daemon's own use. It
// never leaves the process except to be written to tmpfs for the runners.
type Secret struct {
	Kind CredentialKind
	// Token is the personal access token, or the app's PEM private key.
	Token          string
	AppID          int64
	InstallationID int64
}

// IsApp reports whether this is a GitHub App credential.
func (s Secret) IsApp() bool { return s.Kind == CredentialApp }

// Pool is a set of identical runners on one repository or organisation.
type Pool struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	ScopeKind ScopeKind `json:"scopeKind"`
	Scope     string    `json:"scope"`
	Runtime   Runtime   `json:"runtime"`
	Nested    bool      `json:"nested"`
	Ephemeral bool      `json:"ephemeral"`
	// MinReplicas is what the pool falls back to when nothing is running, and
	// is never below one: a pool with no runner at all cannot accept a job, and
	// so can never discover that it needs more.
	MinReplicas int `json:"minReplicas"`
	// MaxReplicas is the ceiling. Equal to the minimum, the pool is a fixed
	// size and never scales.
	MaxReplicas  int       `json:"maxReplicas"`
	Labels       []string  `json:"labels"`
	CPUs         int       `json:"cpus"`
	MemoryMB     int       `json:"memoryMb"`
	DiskGB       int       `json:"diskGb"`
	Image        string    `json:"image"`
	CredentialID int64     `json:"credentialId"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// URL is where the runners register.
func (p *Pool) URL() string {
	return "https://github.com/" + p.Scope
}

// RunnerName is what one runner in the pool is called, on GitHub and on the
// host. Numbered from one, because these are read by people.
func (p *Pool) RunnerName(index int) string {
	return fmt.Sprintf("%s-%d", p.Name, index)
}

// Elastic reports whether the pool scales at all. A pool whose minimum equals
// its maximum is a fixed size, which is how the previous fixed replica count
// is expressed.
func (p *Pool) Elastic() bool { return p.MaxReplicas > p.MinReplicas }

// Floor is the smallest the pool is allowed to be, and is at least one even if
// the stored value is not: an empty pool cannot accept a job, so it can never
// find out that it needs to grow.
func (p *Pool) Floor() int {
	if !p.Enabled {
		return 0
	}
	if p.MinReplicas < 1 {
		return 1
	}
	return p.MinReplicas
}

// Ceiling is the largest the pool is allowed to be.
func (p *Pool) Ceiling() int {
	if !p.Enabled {
		return 0
	}
	if p.MaxReplicas < p.Floor() {
		return p.Floor()
	}
	return p.MaxReplicas
}

// EffectiveLabels is what a runner registers with: the labels configured on
// the pool plus the ones describing what it actually is.
//
// The automatic ones exist so a workflow can ask for what it needs —
// runs-on: [self-hosted, nested] — without every pool having to remember to
// spell it out, and without the label and the reality drifting apart.
func (p *Pool) EffectiveLabels() []string {
	seen := map[string]bool{}
	var out []string
	add := func(label string) {
		if label == "" || seen[strings.ToLower(label)] {
			return
		}
		seen[strings.ToLower(label)] = true
		out = append(out, label)
	}

	switch p.Runtime {
	case RuntimeContainer:
		add("container")
	case RuntimeVM:
		add("vm")
	}
	if p.Nested {
		add("nested")
	}
	if p.Ephemeral {
		add("ephemeral")
	}
	for _, label := range p.Labels {
		add(label)
	}
	return out
}

// SpecRevision is bumped when the daemon changes how it builds a runner in a
// way that existing runners cannot be left on.
//
// The generation hash covers what an operator configured. It cannot cover what
// the daemon does with that, which is a problem the first time a release fixes
// the building rather than the configuration: every runner on the host hashes
// the same before and after, so the fleet keeps running the broken recipe until
// somebody deletes the containers by hand. That happened once — a container
// runner built before v0.3.0 looked for the runner in the wrong directory and
// was handed a credential it could not read — and this is what stops it
// happening quietly again.
//
// Raising it replaces every runner, gracefully, as each finishes its job. So it
// is raised only when leaving them alone would be worse.
//
//	1  the first shape
//	2  containers take a minted registration token instead of the credential,
//	   find the runner by looking for config.sh, and are replaced rather than
//	   restarted
//	3  machines carry a runner GitHub has not deprecated, may update it
//	   themselves, and give the job passwordless sudo — a machine built before
//	   this either cannot take work at all or fails every job that needs root
//	4  the golden image's name covers the script that builds it, so revision 3
//	   was installed on hosts that went on reusing an image built without it
//
// It should need bumping less often now. The recipe is in the generation above,
// so a release that changes how runners are built says so by itself; this is
// for the changes no recipe can express.
const SpecRevision = 4

// Generation is a hash of everything a runner is built from. The reconciler
// stamps it on each runner it creates and compares it later: a runner whose
// generation no longer matches its pool is running the wrong configuration and
// has to be replaced — gracefully, since it may be mid-job.
//
// The scaling bounds are deliberately not in it. Scaling a pool — by hand or
// by the autoscaler, many times an hour — must not replace the runners that
// are already right.
//
// The recipe is how the daemon builds this pool's runners — a golden image's
// name, a container image reference — and it is in here because twice now a
// release changed that and left every existing runner looking current. The
// spec revision below is the manual version of this, for changes a recipe
// cannot express.
func (p *Pool) Generation(credentialFingerprint, recipe string) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, part := range parts {
			h.Write([]byte(part))
			h.Write([]byte{0})
		}
	}
	write(
		fmt.Sprint(SpecRevision),
		p.Name,
		string(p.ScopeKind),
		p.Scope,
		string(p.Runtime),
		fmt.Sprint(p.Nested),
		fmt.Sprint(p.Ephemeral),
		strings.Join(p.EffectiveLabels(), ","),
		fmt.Sprint(p.CPUs),
		fmt.Sprint(p.MemoryMB),
		fmt.Sprint(p.DiskGB),
		p.Image,
		recipe,
		credentialFingerprint,
	)
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// Validate reports why a pool cannot work, in terms someone can act on.
func (p *Pool) Validate() error {
	if !poolNameRe.MatchString(p.Name) {
		return fmt.Errorf("name %q: use lower-case letters, digits and dashes, 1 to 32 characters, not starting or ending with a dash", p.Name)
	}

	switch p.ScopeKind {
	case ScopeRepository:
		owner, repo, ok := strings.Cut(p.Scope, "/")
		if !ok || !segmentRe.MatchString(owner) || !segmentRe.MatchString(repo) {
			return fmt.Errorf("scope %q: a repository is owner/name", p.Scope)
		}
	case ScopeOrganization:
		if !segmentRe.MatchString(p.Scope) {
			return fmt.Errorf("scope %q: an organisation is a single name", p.Scope)
		}
		// A personal account is not an organisation as far as runners go, and
		// the API call fails with a 404 that reads like a permissions problem.
		// Nothing here can tell the two apart, so this is caught when the
		// credential is checked against the scope instead.
	default:
		return fmt.Errorf("scope kind %q: want %q or %q", p.ScopeKind, ScopeRepository, ScopeOrganization)
	}

	switch p.Runtime {
	case RuntimeVM, RuntimeContainer:
	default:
		return fmt.Errorf("runtime %q: want %q or %q", p.Runtime, RuntimeVM, RuntimeContainer)
	}

	// One, not zero. A pool has to keep a runner able to accept work, or
	// nothing would ever reveal that the pool needs to grow. Switching a pool
	// off entirely is what "enabled" is for.
	if p.MinReplicas < 1 || p.MinReplicas > MaxReplicas {
		return fmt.Errorf("minimum replicas %d: want 1 to %d — use the enabled switch to stop a pool entirely", p.MinReplicas, MaxReplicas)
	}
	if p.MaxReplicas < p.MinReplicas || p.MaxReplicas > MaxReplicas {
		return fmt.Errorf("maximum replicas %d: want %d to %d, and never below the minimum", p.MaxReplicas, p.MinReplicas, MaxReplicas)
	}
	if p.CPUs < MinCPUs || p.CPUs > MaxCPUs {
		return fmt.Errorf("cpus %d: want %d to %d", p.CPUs, MinCPUs, MaxCPUs)
	}
	if p.MemoryMB < MinMemoryMB || p.MemoryMB > MaxMemoryMB {
		return fmt.Errorf("memory %d MiB: want %d to %d", p.MemoryMB, MinMemoryMB, MaxMemoryMB)
	}
	if p.Runtime == RuntimeVM && (p.DiskGB < MinDiskGB || p.DiskGB > MaxDiskGB) {
		return fmt.Errorf("disk %d GiB: want %d to %d", p.DiskGB, MinDiskGB, MaxDiskGB)
	}
	if p.CredentialID == 0 {
		return fmt.Errorf("a credential is required: runners register afresh on every start, which needs one that can mint registration tokens")
	}

	for _, label := range p.Labels {
		if !labelRe.MatchString(label) {
			return fmt.Errorf("label %q: use letters, digits, dot, dash and underscore, up to 63 characters", label)
		}
	}
	return nil
}

// Defaults fills in what the caller left empty, so the API and the UI agree on
// what an unspecified field means.
func (p *Pool) Defaults() {
	if p.Runtime == "" {
		p.Runtime = RuntimeVM
	}
	if p.MinReplicas < 1 {
		p.MinReplicas = 1
	}
	if p.MaxReplicas < p.MinReplicas {
		p.MaxReplicas = p.MinReplicas
	}
	if p.ScopeKind == "" {
		p.ScopeKind = ScopeRepository
	}
	if p.CPUs == 0 {
		p.CPUs = 2
	}
	if p.MemoryMB == 0 {
		p.MemoryMB = 4096
	}
	if p.DiskGB == 0 && p.Runtime == RuntimeVM {
		p.DiskGB = 40
	}
	if p.Image == "" {
		p.Image = DefaultImage(p.Runtime)
	}
	sort.Strings(p.Labels)
}

// Sample is one observation of a pool: how many runners it had and how many
// were working. Kept over time, these are what the activity chart draws.
type Sample struct {
	Pool    string
	Running int
	Busy    int
	Target  int
}

// ActivityPoint is one point on the activity chart: the whole fleet at a
// moment, bucketed.
type ActivityPoint struct {
	At      time.Time `json:"at"`
	Running int       `json:"running"`
	Busy    int       `json:"busy"`
}

// DefaultImage is what a pool runs when nothing else is asked for.
//
// The value is a variant key rather than a path: pools will grow per-repository
// images — a repository's toolchain baked in, so a job does not pay for the
// install every time — and when they do, this field is what selects one. The
// executors resolve a key to a golden image or a container image, so adding
// variants later does not change the pool schema.
func DefaultImage(runtime Runtime) string {
	if runtime == RuntimeContainer {
		return "default"
	}
	return "default"
}
