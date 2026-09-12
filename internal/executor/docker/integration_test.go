//go:build docker

// These tests run against a real Docker and a real runner image.
//
// They exist because the tests beside them cannot catch a whole class of bug.
// A fake Docker asserts the requests this package sends, which checks the code
// against its author's assumptions — and the first container pool failed on an
// assumption that was simply wrong: the agent looked for the runner in
// /home/runner/actions-runner and the official image puts it in /home/runner.
// The fake agreed with the code, because the same person wrote both.
//
// What follows is the part only a real image can answer: is the runner where we
// think it is, and can the process we start actually read what we hand it.
//
//	go test -tags docker ./internal/executor/docker/
package docker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/paths"
	"github.com/clems4ever/github-runner/internal/reconcile"
)

func requireDocker(t *testing.T) *Executor {
	t.Helper()
	// Skipping is right on a laptop without Docker and wrong in CI, where a
	// skip is a green tick for a test that did not run — which is how the two
	// bugs this file exists for shipped in the first place. CI sets this.
	mustRun := os.Getenv("REQUIRE_DOCKER") != ""

	if _, err := os.Stat(DefaultSocket); err != nil {
		if mustRun {
			t.Fatalf("REQUIRE_DOCKER is set and there is no Docker here: %v", err)
		}
		t.Skipf("no Docker on this host: %v", err)
	}

	layout := paths.Under(t.TempDir())
	if err := layout.EnsureDirs(paths.CurrentOwner()); err != nil {
		t.Fatal(err)
	}

	// The agent is bind-mounted into the container, so it has to exist and be
	// a static binary the image can run.
	binary := filepath.Join(t.TempDir(), "runner-fleet")
	build := exec.Command("go", "build", "-o", binary, "../../../cmd/runner-fleet")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the agent: %v: %s", err, out)
	}

	e := New(layout, binary)
	if err := e.Ping(context.Background()); err != nil {
		if mustRun {
			t.Fatalf("REQUIRE_DOCKER is set and Docker cannot be reached: %v", err)
		}
		t.Skipf("cannot reach Docker: %v", err)
	}
	return e
}

func logsOf(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("docker", "logs", name).CombinedOutput()
	if err != nil {
		t.Fatalf("docker logs %s: %v: %s", name, err, out)
	}
	return string(out)
}

// The bug that shipped: the agent could not find the runner in the image it was
// told to use, and exited before doing anything.
//
// Registration itself fails here — the token is not real — and that is the
// point. Reaching a failure that comes from GitHub means everything on this
// side worked: the image was right, the agent was found and ran, the runner was
// where it expected, and the token reached it.
func TestTheAgentFindsTheRunnerInTheOfficialImage(t *testing.T) {
	e := requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	spec := reconcile.Spec{
		Name: "runner-fleet-integration", Pool: "integration", Generation: "test",
		Runtime: model.RuntimeContainer, URL: "https://github.com/clems4ever/github-runner",
		ScopeKind: model.ScopeRepository, Scope: "clems4ever/github-runner",
		Labels: []string{"container"}, CPUs: 2, MemoryMB: 2048,
		Image: DefaultImage, CredentialID: 1,
		// Not a real token. GitHub will refuse it, which is exactly how far
		// this test needs to get.
		RegistrationToken: "AAAA-not-a-real-registration-token",
	}
	t.Cleanup(func() {
		_ = e.Remove(context.Background(), spec.Name)
	})

	if err := e.Create(ctx, spec); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Wait for it to say something conclusive either way.
	var logs string
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		logs = logsOf(t, spec.Name)
		if strings.Contains(logs, "registering runner") || strings.Contains(logs, "no GitHub Actions runner") {
			break
		}
		time.Sleep(3 * time.Second)
	}

	// The failure this test was written for.
	if strings.Contains(logs, "no GitHub Actions runner") {
		t.Fatalf("the agent could not find the runner in %s:\n%s", DefaultImage, logs)
	}
	if !strings.Contains(logs, "registering runner") {
		t.Fatalf("the agent never got as far as registering:\n%s", logs)
	}

	// And it got far enough that GitHub is the one refusing, which means
	// everything on this side of the wire worked.
	if !strings.Contains(logs, "config.sh") && !strings.Contains(logs, "registration failed") &&
		!strings.Contains(logs, "Invalid") && !strings.Contains(logs, "401") {
		t.Logf("registration did not fail the way expected; the log was:\n%s", logs)
	}
}

// The other half of the same pair: whatever the container is given has to be
// readable by the user the image runs as. A file mounted 0600 root-owned into
// a container running as uid 1001 is not, which is how the credential used to
// be handed over.
func TestWhatTheContainerIsGivenIsReadableByIt(t *testing.T) {
	e := requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	spec := reconcile.Spec{
		Name: "runner-fleet-readable", Pool: "integration", Generation: "test",
		Runtime: model.RuntimeContainer, URL: "https://github.com/o/r",
		ScopeKind: model.ScopeRepository, Scope: "o/r",
		CPUs: 1, MemoryMB: 1024, Image: DefaultImage, CredentialID: 1,
		RegistrationToken: "AAAA-not-a-real-registration-token",
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), spec.Name) })

	if err := e.Create(ctx, spec); err != nil {
		t.Fatalf("create: %v", err)
	}

	deadline := time.Now().Add(3 * time.Minute)
	var logs string
	for time.Now().Before(deadline) {
		logs = logsOf(t, spec.Name)
		if logs != "" {
			break
		}
		time.Sleep(2 * time.Second)
	}

	// Permission denied on anything the daemon handed over means the container
	// cannot use what it was given, whatever the requests looked like.
	if strings.Contains(logs, "permission denied") {
		t.Fatalf("the container cannot read what it was given:\n%s", logs)
	}
}

// Which jobs can run in a container pool and which need a machine is not a
// matter of taste. It is what the image has in it, and that is a fact about
// something outside this repository — so it is checked here rather than
// reasoned about in a comment.
//
// The runner image is built on dotnet runtime-deps: it carries the shared
// libraries a .NET program needs and no compiler. `go test -race` needs cgo and
// therefore a C compiler, which is why this repository's own `go` job runs on a
// machine pool and not a container one.
//
// If this test ever fails because gcc has appeared, that is good news and the
// go job can move to a container pool. Read it as a note, not as a break.
func TestTheOfficialImageHasNoCToolchain(t *testing.T) {
	requireDocker(t)

	if out, err := inTheImage("command -v git tar curl"); err != nil {
		t.Fatalf("the image is missing something checkout and the setup actions need: %v: %s", err, out)
	}

	out, err := inTheImage("command -v cc gcc")
	if err == nil {
		t.Fatalf("the image now has a C compiler:\n%s\n"+
			"That is good news: the go job can move from the vm pool to the container pool.", out)
	}
}

// inTheImage runs one command in the runner image and says what happened.
func inTheImage(command string) (string, error) {
	out, err := exec.Command("docker", "run", "--rm", "--entrypoint", "/bin/bash",
		DefaultImage, "-lc", command).CombinedOutput()
	return string(out), err
}

// buildDindImage builds the runner image that can run a daemon of its own, out
// of the Dockerfile this repository ships.
//
// Built here rather than pulled from somewhere, because the thing being
// checked is that file: the stock image carries dockerd and no iptables, and
// the way that fails — the daemon exits a few seconds in, complaining about a
// network controller — is exactly the failure nobody debugging a pool of
// restarting runners would attribute to a missing package.
func buildDindImage(t *testing.T) string {
	t.Helper()
	const tag = "runner-fleet-dind-integration:latest"
	build := exec.Command("docker", "build", "-t", tag, "../../../images/dind")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the dind runner image: %v: %s", err, out)
	}
	return tag
}

// Docker in Docker, from the Dockerfile to a job container.
//
// The unit tests beside this one assert the request the executor sends, which
// is the half that can be checked against its author's assumptions. This is
// the other half, and every line of it is an assumption that was wrong once:
// that the image has what the daemon needs, that root can start it, that the
// socket it creates can be reached by the account the runner was dropped back
// to, and that a container started inside is a container that runs.
func TestDindGivesTheRunnerAWorkingDaemon(t *testing.T) {
	e := requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	spec := reconcile.Spec{
		Name: "runner-fleet-dind-integration", Pool: "integration", Generation: "test",
		Runtime: model.RuntimeContainer, Docker: model.DockerDind,
		URL:       "https://github.com/clems4ever/github-runner",
		ScopeKind: model.ScopeRepository, Scope: "clems4ever/github-runner",
		Labels: []string{"container", "dind"}, CPUs: 2, MemoryMB: 2048,
		Image: buildDindImage(t), CredentialID: 1,
		// Not a real token, for the same reason as the test above: GitHub
		// refusing it is how far this needs to get.
		RegistrationToken: "AAAA-not-a-real-registration-token",
	}
	t.Cleanup(func() {
		_ = e.Remove(context.Background(), spec.Name)
	})

	if err := e.Create(ctx, spec); err != nil {
		t.Fatalf("create: %v", err)
	}

	var logs string
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		logs = logsOf(t, spec.Name)
		if settled(logs) {
			break
		}
		time.Sleep(3 * time.Second)
	}

	if !strings.Contains(logs, "docker is ready") {
		t.Fatalf("the daemon inside the runner never came up:\n%s", logs)
	}
	// And it came up before the runner registered, which is what keeps a pool
	// from taking a job it cannot run. A runner that has not registered at all
	// is not out of order: Index gives -1 for it, and every position beats -1.
	if at := strings.Index(logs, "registering"); at >= 0 && strings.Index(logs, "docker is ready") > at {
		t.Fatalf("the runner registered before it had a daemon:\n%s", logs)
	}

	// What a job does with the daemon is the test below, on a runner that
	// stays up. This one stops here: the token is deliberately not real, so
	// GitHub refuses it, the runner exits and the container is gone within a
	// second of the line just asserted.
}

// settled reports whether the runner's log has got somewhere this test can
// judge: the daemon answered, the runner carried on without one, or starting
// it failed outright.
//
// Worth its own function because the obvious version of this loop is wrong.
// "starting the docker daemon inside this runner" is printed before dockerd
// has been asked for anything, so a loop that stops at the first mention of a
// docker daemon reads the log while the daemon is still coming up, every time,
// and then calls a daemon that was two seconds away one that never came.
func settled(logs string) bool {
	for _, reached := range []string{
		"docker is ready",
		"registering",
		"the docker daemon stopped while starting up",
		"did not answer within",
	} {
		if strings.Contains(logs, reached) {
			return true
		}
	}
	return false
}

// buildStubRunnerImage builds the dind runner image with its registration and
// its listener replaced by scripts that succeed.
//
// The runner above cannot be asked to do this. Its registration is refused —
// the token is not real, which is how every integration test here avoids
// needing a credential — and a runner whose registration is refused exits,
// taking the daemon and the container with it. Everything a job would do with
// the daemon happens after that point.
//
// Only config.sh and run.sh are replaced. The daemon, the account it is handed
// to and the socket it creates are all still the ones images/dind produces.
func buildStubRunnerImage(t *testing.T, base string) string {
	t.Helper()
	const tag = "runner-fleet-dind-stub:latest"
	dockerfile := "FROM " + base + "\n" +
		"USER root\n" +
		"RUN printf '%s\\n' '#!/bin/sh' 'set -e' 'v=$(docker version --format {{.Server.Version}})' 'echo stub-config: docker says $v' > /home/runner/config.sh \\\n" +
		" && printf '%s\\n' '#!/bin/sh' 'echo stub-runner: Listening for Jobs' 'sleep 900' > /home/runner/run.sh \\\n" +
		" && chmod 0755 /home/runner/config.sh /home/runner/run.sh \\\n" +
		" && chown runner /home/runner/config.sh /home/runner/run.sh\n" +
		"USER runner\n"

	build := exec.Command("docker", "build", "-t", tag, "-f", "-", t.TempDir())
	build.Stdin = strings.NewReader(dockerfile)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the stub runner image: %v: %s", err, out)
	}
	return tag
}

// A job can run a container inside its own runner.
//
// Which is the point of the whole arrangement, and is four assumptions at
// once: that root started the daemon, that the runner was put back on the
// unprivileged account the image built it for, that the socket root created is
// reachable from that account, and that a container started through it runs.
//
// The exec names the account, rather than letting it default. A dind container
// is created with User=root — something has to start the daemon — so an exec
// that says nothing enters as root, and root can reach any socket on the
// machine. It would pass with the handover in startDocker deleted.
func TestAJobCanRunAContainerInsideItsRunner(t *testing.T) {
	e := requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	spec := reconcile.Spec{
		Name: "runner-fleet-dind-job", Pool: "integration", Generation: "test",
		Runtime: model.RuntimeContainer, Docker: model.DockerDind,
		URL:       "https://github.com/clems4ever/github-runner",
		ScopeKind: model.ScopeRepository, Scope: "clems4ever/github-runner",
		Labels: []string{"container", "dind"}, CPUs: 2, MemoryMB: 2048,
		Image: buildStubRunnerImage(t, buildDindImage(t)), CredentialID: 1,
		RegistrationToken: "AAAA-not-a-real-registration-token",
	}
	t.Cleanup(func() {
		_ = e.Remove(context.Background(), spec.Name)
	})

	if err := e.Create(ctx, spec); err != nil {
		t.Fatalf("create: %v", err)
	}

	var logs string
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		logs = logsOf(t, spec.Name)
		if strings.Contains(logs, "Listening for Jobs") || strings.Contains(logs, "registration failed") {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !strings.Contains(logs, "Listening for Jobs") {
		t.Fatalf("the runner never got as far as waiting for a job:\n%s", logs)
	}

	// Registration ran as the unprivileged account and had a daemon already.
	if !strings.Contains(logs, "stub-config: docker says") {
		t.Fatalf("the runner could not reach the daemon while registering:\n%s", logs)
	}

	out, err := exec.Command("docker", "exec", "-u", "runner", spec.Name,
		"docker", "run", "--rm", "hello-world").CombinedOutput()
	if err != nil {
		t.Fatalf("a job could not run a container inside its runner: %v: %s\nrunner log:\n%s", err, out, logs)
	}
}

// A runner whose image cannot run a daemon says so and stops, rather than
// registering and taking a job that will fail on its first docker step.
func TestDindRefusesAnImageWithoutTheDaemon(t *testing.T) {
	e := requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	spec := reconcile.Spec{
		Name: "runner-fleet-dind-unfit", Pool: "integration", Generation: "test",
		Runtime: model.RuntimeContainer, Docker: model.DockerDind,
		URL:       "https://github.com/clems4ever/github-runner",
		ScopeKind: model.ScopeRepository, Scope: "clems4ever/github-runner",
		Labels: []string{"container", "dind"}, CPUs: 2, MemoryMB: 2048,
		Image: DefaultImage, CredentialID: 1,
		RegistrationToken: "AAAA-not-a-real-registration-token",
	}
	t.Cleanup(func() {
		_ = e.Remove(context.Background(), spec.Name)
	})

	if err := e.Create(ctx, spec); err != nil {
		t.Fatalf("create: %v", err)
	}

	var logs string
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		logs = logsOf(t, spec.Name)
		if strings.Contains(logs, "docker in docker") || strings.Contains(logs, "registering") {
			break
		}
		time.Sleep(3 * time.Second)
	}

	if strings.Contains(logs, "registering") {
		t.Fatalf("a runner with no usable daemon registered anyway:\n%s", logs)
	}
	if !strings.Contains(logs, "images/dind") {
		t.Fatalf("the runner stopped without saying how to fix it:\n%s", logs)
	}
}

// The start line is not a verdict.
//
// This is the failure that shipped in the first version of the test above,
// and it costs a container build and five minutes to find out the hard way.
func TestSettledDoesNotMistakeStartingForReady(t *testing.T) {
	starting := `level=INFO msg="starting the docker daemon inside this runner"`
	if settled(starting) {
		t.Fatal("the log said the daemon was being started, and the test read that as an answer")
	}
	if !settled(starting + "\n" + `level=INFO msg="docker is ready"`) {
		t.Fatal("the daemon answered and the test kept waiting")
	}
	if !settled(`level=ERROR msg="the docker daemon stopped while starting up: exit status 1"`) {
		t.Fatal("the daemon died and the test kept waiting")
	}
}
