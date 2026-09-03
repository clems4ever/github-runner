// Package repospec reads what a repository says its jobs need.
//
// The operator owns the fleet: how big a machine is, which credential it uses,
// how many there may be. What the operator cannot know is what any particular
// repository's jobs need installed — that changes with the repository, and
// asking somebody with access to the host to edit a pool every time a project
// picks up a new dependency is how a fleet ends up with one enormous image
// that has everything in it.
//
// So a repository declares its own layer, in a file, next to the workflows
// that need it. The daemon reads that file and builds a thin image on top of
// the pool's, and the jobs from that repository boot from it.
//
// The whole of this package is a trust boundary. A recipe is a root shell on
// the build machine, and a package list is written into a cloud-config that
// the build machine executes, so everything in here is written for input that
// is trying to get out of it:
//
//   - Only the default branch is ever read, which is not decided here — see
//     Fetch. A pull request that edits this file has to be merged before it
//     changes anything, so opening one is not a way to run code on the host.
//   - Package names are matched against Debian's own grammar rather than
//     escaped, because they are interpolated into YAML and a name containing a
//     newline would otherwise add keys to the cloud-config.
//   - A definition is identified by a digest of exactly what it will execute,
//     so that an operator approves a specific package list and a specific
//     script — and an edit to either has to be approved again.
package repospec

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// Path is where a repository declares what its jobs need. It sits beside the
// workflows because that is what it is about, and because .github is already
// the directory a repository's own tooling lives in.
const Path = ".github/runner-fleet.yml"

// Limits. None of these is a considered maximum of anything; they are the
// point past which a file has stopped being a declaration of what a job needs
// and is doing something else.
const (
	// MaxBytes is the largest file that will be read at all.
	MaxBytes = 64 << 10
	// MaxPackages is how many apt packages one repository may add.
	MaxPackages = 200
	// MaxRecipeBytes is the largest provisioning script.
	MaxRecipeBytes = 32 << 10
)

// Spec is what a repository asked for.
type Spec struct {
	// Packages are apt packages added to the pool's image for this repository.
	Packages []string `yaml:"packages"`
	// Recipe is a shell script run as root while this repository's layer is
	// built, after the packages are in.
	Recipe string `yaml:"recipe"`
}

// Empty reports whether the file asked for nothing. A repository that has the
// file but has commented everything out gets the pool's image unchanged rather
// than an empty layer on top of it.
func (s Spec) Empty() bool { return len(s.Packages) == 0 && strings.TrimSpace(s.Recipe) == "" }

// packageName is Debian's own grammar for a package name, with the version
// suffix apt accepts on the command line.
//
// Matched rather than escaped. These names are interpolated into the build
// machine's cloud-config as YAML list items, so a name containing a newline
// would not be a broken package — it would be additional cloud-config, chosen
// by whoever can open a pull request. The leading character is constrained
// separately by the grammar itself, which is what keeps a "package" called
// -oAPT::Something from arriving as an apt option.
var packageName = regexp.MustCompile(`^[a-z0-9][a-z0-9+.\-]{1,127}(=[A-Za-z0-9+.:~\-]{1,64})?$`)

// Parse reads a definition and refuses anything it is not certain of.
//
// Unknown fields are an error rather than being ignored. The alternative is a
// repository writing `packagess:` and getting a silent no-op, which it would
// then debug by looking at the daemon.
func Parse(data []byte) (Spec, error) {
	if len(data) > MaxBytes {
		return Spec{}, fmt.Errorf("%s is %d bytes, and no more than %d are read",
			Path, len(data), MaxBytes)
	}

	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)

	var spec Spec
	if err := decoder.Decode(&spec); err != nil {
		// An empty file decodes to EOF, and is a repository that has the file
		// and asks for nothing. That is not a mistake.
		if err.Error() == "EOF" {
			return Spec{}, nil
		}
		return Spec{}, fmt.Errorf("%s: %w", Path, err)
	}

	if err := spec.validate(); err != nil {
		return Spec{}, fmt.Errorf("%s: %w", Path, err)
	}
	return spec, nil
}

func (s Spec) validate() error {
	if len(s.Packages) > MaxPackages {
		return fmt.Errorf("%d packages: no more than %d", len(s.Packages), MaxPackages)
	}
	for _, pkg := range s.Packages {
		if !packageName.MatchString(pkg) {
			// Quoted, because the reason it was refused is usually something
			// that does not survive being printed plainly.
			return fmt.Errorf("%q is not a package name", pkg)
		}
	}
	if len(s.Recipe) > MaxRecipeBytes {
		return fmt.Errorf("the recipe is %d bytes, and no more than %d are run",
			len(s.Recipe), MaxRecipeBytes)
	}
	return nil
}

// Digest identifies a definition by what it will do, and is what an operator
// approves.
//
// It is taken over the effective package list and the recipe rather than over
// the file, so that a comment, a reordering or a change of quoting does not
// ask somebody to approve the same thing twice — and so that two ways of
// writing the same request cannot be approved separately. Everything that ends
// up executed is in here; nothing else is.
func (s Spec) Digest() string {
	h := sha256.New()
	for _, pkg := range s.EffectivePackages() {
		h.Write([]byte(pkg))
		h.Write([]byte{0})
	}
	h.Write([]byte(s.Recipe))
	return hex.EncodeToString(h.Sum(nil))
}

// EffectivePackages is the sorted, deduplicated list, which is the order they
// are installed in and the order the digest is taken over.
func (s Spec) EffectivePackages() []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(s.Packages))
	for _, pkg := range s.Packages {
		if pkg == "" || seen[pkg] {
			continue
		}
		seen[pkg] = true
		out = append(out, pkg)
	}
	sort.Strings(out)
	return out
}

// Short is the digest as it is shown to a person: enough to compare two of
// them by eye, and never used to decide anything.
func Short(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12]
}
