package layers

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/github"
	"github.com/clems4ever/github-runner/internal/model"
)

// The resolver's tests all read from a fake whose answer the test wrote. That
// is the right way to test the decisions — pending, refused, cached, built —
// but it leaves the first step of the chain untested against the thing it
// actually talks to: a real repository, read with a real credential, through
// the same construction the daemon uses.
//
// The branch this proves is the one almost every pass takes. A pool with
// layers on asks its repository about a file that most repositories will never
// have, every five minutes, for ever. If that came back as anything but "there
// is nothing here", the pool would carry a permanent note, and the note would
// look exactly like a credential that had stopped working.
//
//	FLEET_LIVE_TOKEN=… FLEET_LIVE_REPO=owner/name go test ./internal/layers -run Live -v
//
// It only reads. What is *not* covered here is a repository that does have a
// definition — that would mean committing one to somebody's default branch,
// which is not a test's to do. The rest of that path is covered where it can
// be: repospec's parser against the bytes, this package's tests against the
// decisions, and internal/agent's live test against a layer really built and
// really booted.
func liveReader(t *testing.T) (ReaderFor, model.Pool) {
	t.Helper()
	token, repo := os.Getenv("FLEET_LIVE_TOKEN"), os.Getenv("FLEET_LIVE_REPO")
	if token == "" || repo == "" {
		t.Skip("set FLEET_LIVE_TOKEN and FLEET_LIVE_REPO to run this against GitHub")
	}
	// Built the way serve.go builds it, from a model.Secret, so that this
	// covers the wiring and not only the client.
	reader := func(secret model.Secret) (Reader, error) {
		return github.NewFromSecret(github.Secret{
			IsAppCredential: secret.IsApp(),
			Token:           secret.Token,
			AppID:           secret.AppID,
			InstallationID:  secret.InstallationID,
		})
	}
	return reader, model.Pool{
		Name: "live", Runtime: model.RuntimeVM,
		ScopeKind: model.ScopeRepository, Scope: repo,
		Layers: model.LayersApprove,
	}
}

// liveFleet is the fake store and builder, with the token the credential.
type liveFleet struct {
	*fleet
	token string
}

func (f *liveFleet) Secret(context.Context, int64) (model.Secret, error) {
	return model.Secret{Kind: model.CredentialPAT, Token: f.token}, nil
}

func TestLiveARepositoryWithNoDefinitionIsSilent(t *testing.T) {
	reader, pool := liveReader(t)
	store := &liveFleet{fleet: newFleet(t), token: os.Getenv("FLEET_LIVE_TOKEN")}

	resolver := New(store, reader, store, nil).WithClock(func() time.Time { return store.clock })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	image, note := resolver.For(ctx, pool)
	if image != "" {
		t.Fatalf("chose image %q for a repository that asked for nothing", image)
	}
	if note != "" {
		t.Fatalf("said %q about a repository that asked for nothing", note)
	}
	if len(store.rows) != 0 {
		t.Fatalf("recorded %d definitions from a repository that has none", len(store.rows))
	}
	if len(store.ensured) != 0 {
		t.Fatalf("asked to build %v for a repository that asked for nothing", store.ensured)
	}
}

// The cost this is allowed to have. A pass is every thirty seconds and this is
// a request to GitHub per pool; unbounded, a fleet of twenty repository pools
// would spend forty requests a minute of somebody's rate limit on a file that
// changes on the order of weeks.
func TestLiveTheRepositoryIsReadOnceNotEveryPass(t *testing.T) {
	reader, pool := liveReader(t)
	store := &liveFleet{fleet: newFleet(t), token: os.Getenv("FLEET_LIVE_TOKEN")}

	// Counted by wrapping the real reader rather than by replacing it: the
	// question is how often the daemon reaches GitHub, so the thing being
	// counted has to be the thing that reaches it.
	reads := 0
	counted := func(secret model.Secret) (Reader, error) {
		inner, err := reader(secret)
		if err != nil {
			return nil, err
		}
		return countingReader{inner: inner, reads: &reads}, nil
	}

	resolver := New(store, counted, store, nil).WithClock(func() time.Time { return store.clock })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for range 5 {
		resolver.For(ctx, pool)
	}
	if reads != 1 {
		t.Fatalf("reached GitHub %d times in five passes", reads)
	}

	// Past the interval, and it looks again — a repository that adds a
	// definition must not wait for a restart.
	store.clock = store.clock.Add(Interval + time.Minute)
	resolver.For(ctx, pool)
	if reads != 2 {
		t.Fatalf("reached GitHub %d times, want a second read once the answer was stale", reads)
	}
}

type countingReader struct {
	inner Reader
	reads *int
}

func (c countingReader) DefaultBranchFile(ctx context.Context, scope github.Scope, path string) ([]byte, error) {
	*c.reads++
	return c.inner.DefaultBranchFile(ctx, scope, path)
}

// A credential GitHub refuses is the failure an operator will actually hit, and
// what matters is that it does not take the pool down: the runners keep being
// built from the pool's own image, and the note says which repository could not
// be read.
func TestLiveARefusedCredentialLeavesThePoolRunning(t *testing.T) {
	reader, pool := liveReader(t)
	store := &liveFleet{fleet: newFleet(t), token: "ghp_" + strings.Repeat("0", 36)}

	resolver := New(store, reader, store, nil).WithClock(func() time.Time { return store.clock })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	image, note := resolver.For(ctx, pool)
	if image != "" {
		t.Fatalf("chose image %q on a credential GitHub refused", image)
	}
	if !strings.Contains(note, pool.Scope) {
		t.Fatalf("said %q, which does not name the repository that could not be read", note)
	}
}
