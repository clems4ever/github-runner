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
	// MaxRecipeBytes bounds a pool's image recipe. It is a shell script, not a
	// program: something that needs more than this wants to be fetched by the
	// recipe rather than pasted into it.
	MaxRecipeBytes = 64 * 1024
)

var (
	// Pool names end up in unit names, container names and runner names, so
	// they are restricted to what all three accept without escaping.
	poolNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$|^[a-z0-9]$`)
	labelRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	segmentRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	// What Debian policy allows in a package name. Checked because these are
	// interpolated into the cloud-init the image is built from, where a name
	// with a newline in it is not a failed install but a different document.
	packageRe = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]{1,63}$`)
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
	MaxReplicas int      `json:"maxReplicas"`
	Labels      []string `json:"labels"`
	CPUs        int      `json:"cpus"`
	MemoryMB    int      `json:"memoryMb"`
	DiskGB      int      `json:"diskGb"`
	Image       string   `json:"image"`
	// Packages are apt packages baked into this pool's image on top of the
	// ones every runner gets. A job that installs the same package every time
	// pays for it every time; this is where it stops paying.
	Packages []string `json:"packages"`
	// Recipe is a shell script run as root while the image is built, after the
	// packages are in. It is for everything apt cannot give: a toolchain at a
	// version no archive carries, a pinned linter, a warm build cache.
	//
	// It is part of the image's identity — see the executor's Recipe — so
	// editing it builds a new image and drains the runners built from the old
	// one, and leaving it alone reuses what is already on the host.
	Recipe string `json:"recipe"`
	// Layers is how much this pool trusts the repository it serves to declare
	// its own additions to the image. Off unless an operator turns it on; see
	// layer.go for what turning it on means.
	Layers       LayerPolicy `json:"layers"`
	CredentialID int64       `json:"credentialId"`
	Enabled      bool        `json:"enabled"`
	CreatedAt    time.Time   `json:"createdAt"`
	UpdatedAt    time.Time   `json:"updatedAt"`
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
// runs-on: [self-hosted, nestedvirt] — without every pool having to remember to
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
		// "nested" on its own says nothing about what is nested; a workflow
		// author reading runs-on has to guess. The label spells out the
		// capability it is really asking for.
		add("nestedvirt")
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
//	5  machines are booted with a balloon that reports free pages, so guest
//	   memory a job has finished with goes back to the host — a machine built
//	   before this holds its high-water mark until it is replaced
//
// It should need bumping less often now. The recipe is in the generation above,
// so a release that changes how runners are built says so by itself; this is
// for the changes no recipe can express.
//
// Revision 5 is the first that is not about a runner that cannot work, and it
// is here because no recipe can reach it: the QEMU command line is not part of
// the golden image, so a machine built the old way hashes identically to one
// built the new way and would never be replaced. On a fleet whose pools are
// ephemeral that resolves itself within a job or two — but "within a job or
// two" is not the same as "on an idle host", where the machines waiting for
// work are exactly the ones that would sit unfixed indefinitely.
const SpecRevision = 5

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

	// Both are how a machine's image is built, and a container has no image of
	// this kind — it names one that somebody else built. Refused rather than
	// ignored: a pool that quietly bakes nothing is a pool whose jobs install
	// the toolchain every time and nobody knows why.
	if p.Runtime != RuntimeVM {
		if len(p.Packages) > 0 {
			return fmt.Errorf("packages are for machine pools: a container pool names a prebuilt image in its image field instead")
		}
		if p.Recipe != "" {
			return fmt.Errorf("a recipe is for machine pools: a container pool names a prebuilt image in its image field instead")
		}
	}
	for _, pkg := range p.Packages {
		if !packageRe.MatchString(pkg) {
			return fmt.Errorf("package %q: use a Debian package name — lower-case letters, digits, plus, dot and dash", pkg)
		}
	}
	if len(p.Recipe) > MaxRecipeBytes {
		return fmt.Errorf("recipe is %d bytes: the limit is %d, and a recipe that long wants to fetch what it needs rather than carry it", len(p.Recipe), MaxRecipeBytes)
	}
	if err := ValidateLayerPolicy(*p); err != nil {
		return err
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
	// Sorted for the same reason the labels are: what is stored should not
	// depend on the order somebody typed it in. The image's name does not —
	// ImageSpec sorts and deduplicates before hashing — but a pool that reads
	// back differently from how it was written is confusing on its own.
	sort.Strings(p.Packages)
	// A recipe pasted from an editor that ends its lines with CRLF would
	// otherwise reach the build machine with a carriage return on the end of
	// every command, where bash treats it as part of the argument: the error
	// is "no such file or directory" naming a file that plainly exists.
	// Normalised on the way in, so what is stored is what runs and what the
	// image's name was computed from.
	p.Recipe = strings.ReplaceAll(p.Recipe, "\r\n", "\n")
	if p.Layers == "" {
		p.Layers = LayersOff
	}
}

// Sample is one observation of a pool: how many runners it had and how many
// were working. Kept over time, these are what the activity chart draws.
type Sample struct {
	Pool string
	// Scope is the repository or organisation the pool was targeting when the
	// observation was taken, kept alongside the pool name so the history can
	// be read by scope after the pool itself is gone.
	Scope   string
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

// HostSample is one observation of the machine the fleet runs on. Kept over
// time, these are what the resource history draws.
//
// The totals are stored alongside the used figures rather than being assumed
// constant: memory and disk both change under a host that is resized or has a
// volume grown, and a chart drawn against yesterday's total would be wrong for
// yesterday.
type HostSample struct {
	CPUPercent       float64
	MemoryUsedBytes  int64
	MemoryTotalBytes int64
	DiskUsedBytes    int64
	DiskTotalBytes   int64
}

// HostPoint is one point on the resource history chart.
//
// Percentages rather than bytes, so that three quantities measured in three
// different units share one axis and can be read against each other. The bytes
// are in the live report, where a number is being read rather than a shape.
type HostPoint struct {
	At            time.Time `json:"at"`
	CPUPercent    float64   `json:"cpuPercent"`
	MemoryPercent float64   `json:"memoryPercent"`
	DiskPercent   float64   `json:"diskPercent"`
}

// JobSample is what one reconcile pass saw of a pool's work.
//
// Nothing here is a stopwatch on a job. The daemon does not watch jobs; it asks
// GitHub once a pass what each runner is doing and adds up what it was told,
// so both figures are observations bounded by how often it asks.
type JobSample struct {
	Pool string
	// Started counts the runners that have a job on them now and did not at
	// the previous pass. A job that begins and ends between two passes is
	// never seen at all.
	Started int
	// BusySeconds is runner-time rather than wall-clock: two runners busy for
	// a minute is two minutes. That is the figure a pool is sized against,
	// since it is what the pool would have had to be bigger to absorb.
	BusySeconds float64
}

// PoolJobs is one pool's total over a window.
type PoolJobs struct {
	Pool    string  `json:"pool"`
	Jobs    int     `json:"jobs"`
	Seconds float64 `json:"seconds"`
}

// JobDay is one pool's total for one UTC day.
//
// A day rather than something finer, because this history is kept for a
// quarter and the question it exists to answer — is this pool too small — is
// argued with weeks of evidence. The activity chart is where a single burst is
// looked at minute by minute, over a window of hours.
type JobDay struct {
	Day     string  `json:"day"`
	Pool    string  `json:"pool"`
	Jobs    int     `json:"jobs"`
	Seconds float64 `json:"seconds"`
}

// Commitment is what the pools have promised the host, if every one of them
// grew to its ceiling at once.
//
// It is a configured number, not a measured one, and the difference is the
// point of showing it: a fleet can be idle and still be oversubscribed three
// times over, and the moment that matters is the moment every pool is busy.
type Commitment struct {
	// Runners is how many would exist at full stretch.
	Runners int `json:"runners"`
	// CPUs is the sum of what they would be given. Machines are handed a fixed
	// number of processors and containers a quota, so both count.
	CPUs        int   `json:"cpus"`
	MemoryBytes int64 `json:"memoryBytes"`
	// DiskBytes counts machines only. A container writes into the host's
	// filesystem without reserving anything, so there is no figure to add.
	DiskBytes int64 `json:"diskBytes"`
}

// Commit adds up what a set of pools would take at their ceiling. Pools that
// are switched off are left out: they have a ceiling of zero runners.
func Commit(pools []Pool) Commitment {
	var total Commitment
	for i := range pools {
		pool := pools[i]
		ceiling := pool.Ceiling()
		total.Runners += ceiling
		total.CPUs += ceiling * pool.CPUs
		total.MemoryBytes += int64(ceiling) * int64(pool.MemoryMB) * 1024 * 1024
		if pool.Runtime == RuntimeVM {
			total.DiskBytes += int64(ceiling) * int64(pool.DiskGB) * 1024 * 1024 * 1024
		}
	}
	return total
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
