package containerimage_test

import (
	"strings"
	"testing"

	"github.com/clems4ever/github-runner/internal/containerimage"
)

func TestNothingToBuildIsNotABuild(t *testing.T) {
	// A pool that names a prebuilt image and nothing else has to keep working
	// exactly as it did: no build, no wait, no image belonging to this daemon.
	if (containerimage.Spec{Base: "ghcr.io/me/runner:v3"}).Wanted() {
		t.Error("a pool with no packages and no recipe was given something to build")
	}
	if !(containerimage.Spec{Packages: []string{"make"}}).Wanted() {
		t.Error("a pool with a package was given nothing to build")
	}
	if !(containerimage.Spec{Recipe: "echo hello"}).Wanted() {
		t.Error("a pool with a recipe was given nothing to build")
	}
	if (containerimage.Spec{Recipe: "   \n  "}).Wanted() {
		t.Error("whitespace counted as a recipe, which would build an image that does nothing")
	}
}

func TestTheNameIsEverythingTheImageIsBuiltFrom(t *testing.T) {
	base := containerimage.Spec{Base: "a", Packages: []string{"make"}, Recipe: "echo 1"}
	same := containerimage.Spec{Base: "a", Packages: []string{"make"}, Recipe: "echo 1"}
	if base.Name() != same.Name() {
		t.Error("two identical specs asked for two images")
	}
	for _, other := range []containerimage.Spec{
		{Base: "b", Packages: []string{"make"}, Recipe: "echo 1"},
		{Base: "a", Packages: []string{"make", "gcc"}, Recipe: "echo 1"},
		{Base: "a", Packages: []string{"make"}, Recipe: "echo 2"},
	} {
		if other.Name() == base.Name() {
			t.Errorf("%+v got the same name as %+v, so the host would reuse the old image", other, base)
		}
	}
}

func TestOrderAndDuplicatesDoNotMakeANewImage(t *testing.T) {
	// Two pools asking for the same set in a different order share a build.
	a := containerimage.Spec{Packages: []string{"gcc", "make", "gcc"}}
	b := containerimage.Spec{Packages: []string{"make", "gcc"}}
	if a.Name() != b.Name() {
		t.Error("the same packages in a different order asked for two images")
	}
}

func TestTheNameSaysWhoBuiltIt(t *testing.T) {
	name := containerimage.Spec{Packages: []string{"make"}}.Name()
	if !strings.HasPrefix(name, containerimage.Prefix+":") {
		t.Errorf("%q is not under %q, so nothing can tell it from an image somebody pulled",
			name, containerimage.Prefix)
	}
}

func TestTheDockerfileInstallsAsRootAndHandsTheJobBack(t *testing.T) {
	// An image left as root is one whose jobs run as root, which is not what
	// anybody asked this feature for.
	df := containerimage.Spec{Packages: []string{"make"}, Recipe: "echo hi"}.Dockerfile()
	if !strings.Contains(df, "USER root") {
		t.Error("the build does not become root, so apt cannot install anything")
	}
	if !strings.HasSuffix(strings.TrimSpace(df), "USER runner") {
		t.Errorf("the image does not end as the runner account:\n%s", df)
	}
	if strings.Index(df, "USER root") > strings.Index(df, "apt-get") {
		t.Error("apt runs before the build becomes root")
	}
}

func TestARecipeIsCarriedVerbatim(t *testing.T) {
	// A recipe is a script somebody wrote — quotes, newlines and all — and the
	// log of a failed build should show what they wrote. It travels as a file
	// in the build context rather than escaped into a RUN.
	recipe := "curl -fsSL 'https://example.test/x?a=1&b=2' | tar -xz\necho \"done\""
	files := containerimage.Spec{Recipe: recipe}.Files()
	got, ok := files[containerimage.RecipeFile]
	if !ok {
		t.Fatalf("the context has no %s: %v", containerimage.RecipeFile, files)
	}
	if !strings.Contains(string(got), recipe) {
		t.Errorf("the recipe was mangled on the way into the context:\n%s", got)
	}
	df := string(files["Dockerfile"])
	if !strings.Contains(df, "sh -e /tmp/"+containerimage.RecipeFile) {
		t.Errorf("a step that fails would not fail the build:\n%s", df)
	}
	// A recipe can carry a token; it must not be left in the image.
	if !strings.Contains(df, "rm -f /tmp/"+containerimage.RecipeFile) {
		t.Errorf("the recipe is left in the image:\n%s", df)
	}
}

func TestNoRecipeMeansNoFileInTheContext(t *testing.T) {
	files := containerimage.Spec{Packages: []string{"make"}}.Files()
	if _, ok := files[containerimage.RecipeFile]; ok {
		t.Error("a pool with no recipe still ships one, and the COPY would fail")
	}
	if strings.Contains(string(files["Dockerfile"]), "COPY") {
		t.Error("the Dockerfile copies a file the context does not have")
	}
}

func TestNoPackagesMeansNoApt(t *testing.T) {
	df := containerimage.Spec{Recipe: "echo hi"}.Dockerfile()
	if strings.Contains(df, "apt-get") {
		t.Errorf("a pool with no packages still runs apt:\n%s", df)
	}
}

func TestTheDefaultBaseIsTheRunnerImage(t *testing.T) {
	for _, base := range []string{"", "   ", "default"} {
		df := containerimage.Spec{Base: base, Recipe: "echo hi"}.Dockerfile()
		if !strings.Contains(df, "FROM "+containerimage.DefaultBase) {
			t.Errorf("base %q did not build on the default runner image:\n%s", base, df)
		}
	}
}
