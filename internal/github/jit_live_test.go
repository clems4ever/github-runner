package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
)

// The unit tests answer "does the client send what this file thinks it sends".
// They cannot answer "is that what GitHub actually wants", and that is the
// question a registration path has to get right — a wrong field name here is a
// runner that never comes online, found by a job that failed at midnight.
//
// So this talks to real GitHub, and is skipped unless it is told where. It
// wants a token with Administration: read and write on the repository named in
// FLEET_LIVE_REPO, which is the permission generate-jitconfig needs:
//
//	FLEET_LIVE_TOKEN=… FLEET_LIVE_REPO=owner/name go test ./internal/github -run Live -v
//
// It cleans up after itself: every runner it registers is deregistered, in a
// deferred call, under a name nothing else would choose.
func live(t *testing.T) (*Client, Scope) {
	t.Helper()
	token, repo := os.Getenv("FLEET_LIVE_TOKEN"), os.Getenv("FLEET_LIVE_REPO")
	if token == "" || repo == "" {
		t.Skip("set FLEET_LIVE_TOKEN and FLEET_LIVE_REPO to run this against GitHub")
	}
	return New(token), Scope{Kind: model.ScopeRepository, Path: repo}
}

// name is unmistakable in a runner list somebody is reading later, and unique
// per test so two runs do not collide with each other.
func name(t *testing.T) string {
	t.Helper()
	return "fleet-live-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
}

// decoded is what the runner process is handed. Its shape is the contract:
// run.sh --jitconfig writes these three files out and reads them back.
func decoded(t *testing.T, encoded string) map[string]string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("the configuration is not base64: %v", err)
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the configuration is not the runner's JSON: %v", err)
	}
	return out
}

func TestLiveJITConfigIsWhatTheRunnerExpects(t *testing.T) {
	client, scope := live(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	runner := name(t)
	defer client.Deregister(ctx, scope, runner)

	encoded, err := client.JITConfig(ctx, scope, JIT{
		Name: runner, Labels: []string{"self-hosted", "vm", "ephemeral"},
	})
	if err != nil {
		t.Fatal(err)
	}

	config := decoded(t, encoded)
	for _, key := range []string{".runner", ".credentials", ".credentials_rsaparams"} {
		if config[key] == "" {
			t.Errorf("no %s in the configuration; the runner will not start", key)
		}
	}

	inner, err := base64.StdEncoding.DecodeString(config[".runner"])
	if err != nil {
		t.Fatal(err)
	}
	// Every value in here is a string, including the booleans and the ids:
	// GitHub sends {"Ephemeral":"True","UseV2Flow":"true","AgentId":"359"},
	// with the two booleans not even capitalised the same way. It is the
	// runner's own settings file rather than an API response, which is why it
	// looks like this. Decoding it into bools is what the first version of
	// this test did, and it failed.
	var settings map[string]any
	if err := json.Unmarshal(inner, &settings); err != nil {
		t.Fatal(err)
	}
	text := func(key string) string {
		value, _ := settings[key].(string)
		return value
	}

	if text("AgentName") != runner {
		t.Errorf("configured as %q, want %q", text("AgentName"), runner)
	}
	// The whole reason this path exists: GitHub makes it ephemeral, so nothing
	// on this side has to ask for it and nothing can forget to.
	if !strings.EqualFold(text("Ephemeral"), "true") {
		t.Errorf("Ephemeral is %q; a just-in-time runner that takes a second job is not one",
			text("Ephemeral"))
	}
	if text("WorkFolder") != "_work" {
		t.Errorf("work folder %q, want _work", text("WorkFolder"))
	}
	if !strings.Contains(text("GitHubUrl"), scope.Path) {
		t.Errorf("pointed at %q, want the %s scope", text("GitHubUrl"), scope.Path)
	}
	// The listener the runner long-polls. Worth asserting because it is the
	// one thing in the configuration that is not on api.github.com, and a
	// network that reaches GitHub but not this is a runner that registers and
	// never picks up a job.
	if !strings.Contains(text("ServerUrl"), "actions.githubusercontent.com") {
		t.Errorf("listener at %q, want an Actions service host", text("ServerUrl"))
	}
}

// The one branch a fake server can only assert the shape of. GitHub refuses a
// name it already holds — there is no --replace here — and a runner's name is
// reused on purpose, every time a machine comes back.
func TestLiveJITConfigReplacesARunnerLeftBehind(t *testing.T) {
	client, scope := live(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	runner := name(t)
	defer client.Deregister(ctx, scope, runner)

	if _, err := client.JITConfig(ctx, scope, JIT{Name: runner}); err != nil {
		t.Fatal(err)
	}
	// Nothing deregistered it: this is the machine that was killed rather than
	// stopped, coming back under the name it had.
	if _, err := client.JITConfig(ctx, scope, JIT{Name: runner}); err != nil {
		t.Fatalf("could not take back the name of a runner that was left behind: %v", err)
	}

	runners, err := client.Runners(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	var found int
	for _, r := range runners {
		if r.Name == runner {
			found++
		}
	}
	if found != 1 {
		t.Errorf("%d runners called %s, want exactly one", found, runner)
	}
}

// Labels are how a workflow reaches a pool, so an empty list is a runner
// nothing can target. GitHub rejects it outright with a 422; the client fills
// in the one label every runner carries anyway.
func TestLiveJITConfigWithoutLabelsIsStillTargetable(t *testing.T) {
	client, scope := live(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	runner := name(t)
	defer client.Deregister(ctx, scope, runner)

	if _, err := client.JITConfig(ctx, scope, JIT{Name: runner}); err != nil {
		t.Fatalf("GitHub refused a configuration with no labels: %v", err)
	}
}
