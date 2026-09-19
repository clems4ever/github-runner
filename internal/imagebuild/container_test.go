package imagebuild

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/clems4ever/github-runner/internal/containerimage"
	"github.com/clems4ever/github-runner/internal/model"
)

// A Docker that is not there: what it holds, and what it does when asked to
// build, are whatever a test says.
type fakeDocker struct {
	has    map[string]bool
	builds []containerimage.Spec
	fail   error
	prints string
}

func (f *fakeDocker) BuildImage(_ context.Context, spec containerimage.Spec, journal io.Writer) error {
	f.builds = append(f.builds, spec)
	if journal != nil && f.prints != "" {
		_, _ = io.WriteString(journal, f.prints)
	}
	if f.fail != nil {
		return f.fail
	}
	if f.has == nil {
		f.has = map[string]bool{}
	}
	f.has[spec.Name()] = true
	return nil
}

func (f *fakeDocker) HasImage(_ context.Context, image string) (bool, error) {
	return f.has[image], nil
}

// containerFixture is the fixture above with a Docker attached, because a
// container build goes to the daemon rather than to a booted machine.
func containerFixture(t *testing.T, docker Containers) *fixture {
	t.Helper()
	f := newFixture(t)
	f.builder.containers = docker
	return f
}

func bakingPool() model.Pool {
	return model.Pool{
		Name: "ci", Runtime: model.RuntimeContainer, Enabled: true,
		Image:    "ghcr.io/actions/actions-runner:latest",
		Packages: []string{"make"}, Recipe: "echo hello\n",
	}
}

// The rule the machine side has, on the other runtime: a pool is not ready
// until the image its runners would start from has been built, and it is ready
// once it has.
func TestAContainerPoolIsNotReadyUntilItsImageIsBuilt(t *testing.T) {
	docker := &fakeDocker{prints: "Step 1/4 : FROM ghcr.io/actions/actions-runner:latest\n"}
	f := containerFixture(t, docker)
	pool := bakingPool()

	if status := f.builder.Status(pool); status.Ready {
		t.Fatalf("a pool whose image does not exist was reported ready: %+v", status)
	}

	f.builder.Ensure(context.Background(), pool)
	f.run()

	if len(docker.builds) != 1 {
		t.Fatalf("the image was built %d times", len(docker.builds))
	}
	if got := docker.builds[0].Name(); got != Image(pool) {
		t.Errorf("built %q, and the pool asks for %q", got, Image(pool))
	}
	if status := f.builder.Status(pool); !status.Ready || status.State != StateReady {
		t.Fatalf("a pool whose image is built is not ready: %+v", status)
	}

	// Asked again, nothing is built again.
	f.builder.Ensure(context.Background(), pool)
	f.run()
	if len(docker.builds) != 1 {
		t.Errorf("the image was built again: %d builds", len(docker.builds))
	}
}

// A pool that bakes nothing keeps exactly the behaviour every container pool
// had before there was anything to build.
func TestAContainerPoolThatBakesNothingHasNothingToBuild(t *testing.T) {
	docker := &fakeDocker{}
	f := containerFixture(t, docker)
	pool := bakingPool()
	pool.Packages, pool.Recipe = nil, ""

	status := f.builder.Ensure(context.Background(), pool)
	if !status.Ready || status.State != StateNone {
		t.Fatalf("a pool with nothing to bake is waiting for something: %+v", status)
	}
	f.run()
	if len(docker.builds) != 0 {
		t.Errorf("a pool with nothing to bake built %d images", len(docker.builds))
	}
	if _, err := f.builder.Rebuild(context.Background(), pool); err == nil {
		t.Error("a pool with nothing to bake was given a build to run")
	}
}

// Editing what a pool bakes asks for a different image rather than quietly
// keeping the one it has — the failure the machine side learned the hard way.
func TestEditingWhatAContainerPoolBakesAsksForANewImage(t *testing.T) {
	pool := bakingPool()
	before := Image(pool)

	edited := pool
	edited.Recipe = "echo something else\n"
	if Image(edited) == before {
		t.Error("the image did not change with the recipe")
	}

	repackaged := pool
	repackaged.Packages = []string{"make", "gcc"}
	if Image(repackaged) == before {
		t.Error("the image did not change with the package list")
	}

	rebased := pool
	rebased.Image = "ghcr.io/somebody/else:latest"
	if Image(rebased) == before {
		t.Error("the image did not change with the image it is built on")
	}
}

// A build that fails leaves the pool with no runners and a log that says why,
// and is not tried again until somebody asks.
func TestAFailedContainerBuildIsReportedAndNotRetried(t *testing.T) {
	docker := &fakeDocker{fail: errors.New("the recipe exited 1"), prints: "+ false\n"}
	f := containerFixture(t, docker)
	pool := bakingPool()

	f.builder.Ensure(context.Background(), pool)
	f.run()

	status := f.builder.Status(pool)
	if status.Ready || status.State != StateFailed {
		t.Fatalf("a pool whose image failed is %+v", status)
	}
	if status.Build == nil || !strings.Contains(status.Build.Error, "recipe exited 1") {
		t.Fatalf("the failure does not say what happened: %+v", status.Build)
	}

	f.builder.Ensure(context.Background(), pool)
	f.run()
	if len(docker.builds) != 1 {
		t.Errorf("a failed build was tried again on its own: %d attempts", len(docker.builds))
	}

	if _, err := f.builder.Rebuild(context.Background(), pool); err != nil {
		t.Fatalf("asking for another attempt: %v", err)
	}
	f.run()
	if len(docker.builds) != 2 {
		t.Errorf("asking for another attempt built %d times", len(docker.builds))
	}
}

// What the builder printed is what is kept, and the Dockerfile with it: the
// recipe that failed is in the log beside the output it produced.
func TestTheContainerBuildsOutputIsTheLog(t *testing.T) {
	docker := &fakeDocker{prints: "Step 2/4 : RUN apt-get install -y make\n"}
	f := containerFixture(t, docker)
	pool := bakingPool()

	f.builder.Ensure(context.Background(), pool)
	f.run()

	build := f.builder.Status(pool).Build
	if build == nil {
		t.Fatal("a build that happened is not recorded")
	}
	log, err := f.builder.Log(context.Background(), build.ID, 1<<20)
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	for _, want := range []string{"apt-get install -y make", "FROM ", "echo hello"} {
		if !strings.Contains(log, want) {
			t.Errorf("the log does not carry %q:\n%s", want, log)
		}
	}
}

// A host with no Docker says so, rather than leaving a pool waiting for
// something nothing on this host will ever do.
func TestNoDockerIsAFailureThatNamesTheReason(t *testing.T) {
	f := containerFixture(t, nil)
	pool := bakingPool()

	f.builder.Ensure(context.Background(), pool)
	f.run()

	build := f.builder.Status(pool).Build
	if build == nil || !strings.Contains(build.Error, "no Docker") {
		t.Errorf("the failure does not name the reason: %+v", build)
	}
}

// Docker not answering is not "the image is missing": rebuilding on a socket
// that blinked would throw away a good image and take the pool down with it.
func TestADockerThatWillNotAnswerDoesNotLookLikeAMissingImage(t *testing.T) {
	f := containerFixture(t, refusingDocker{})
	pool := bakingPool()

	status := f.builder.Status(pool)
	if status.Ready {
		t.Error("a pool was reported ready on an image nothing could confirm")
	}
	if status.State != StateUnbuilt {
		t.Errorf("state %q; unbuilt is what an unanswered question looks like", status.State)
	}
}

type refusingDocker struct{}

func (refusingDocker) BuildImage(context.Context, containerimage.Spec, io.Writer) error {
	return errors.New("docker: no such host")
}
func (refusingDocker) HasImage(context.Context, string) (bool, error) {
	return false, errors.New("docker: no such host")
}
