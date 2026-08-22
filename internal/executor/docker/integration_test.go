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
	if err := layout.EnsureDirs(); err != nil {
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
