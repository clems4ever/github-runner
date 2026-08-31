package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/clems4ever/github-runner/internal/agent"
	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/paths"
	"github.com/clems4ever/github-runner/internal/secrets"
	"github.com/clems4ever/github-runner/internal/store"
)

func main() {
	layout := paths.Under(os.Args[1])
	must(layout.EnsureDirs(paths.CurrentOwner()))
	ring, err := secrets.LoadOrCreateKey(layout.MasterKey())
	must(err)
	db, err := store.Open(layout.Database(), ring)
	must(err)
	defer db.Close()
	ctx := context.Background()

	cred, err := db.CreateCredential(ctx, model.Credential{Name: "runyard", Kind: model.CredentialPAT}, "github_pat_11ABCDEFG0demoonlynotarealtoken")
	must(err)

	recipe := "GO_VERSION=1.25.0\ninstall -d /opt/hostedtoolcache/go/${GO_VERSION}\n"
	pools := []model.Pool{
		{Name: "runyard-org-vm-nested", ScopeKind: model.ScopeOrganization, Scope: "runyard",
			Runtime: model.RuntimeVM, Nested: true, Ephemeral: true, MinReplicas: 1, MaxReplicas: 4,
			Labels: []string{"gpu"}, CPUs: 4, MemoryMB: 8192, DiskGB: 60, CredentialID: cred.ID,
			Enabled: true, Recipe: "install-the-toolchain --version 3\n"},
		{Name: "web", ScopeKind: model.ScopeRepository, Scope: "clems4ever/runyard",
			Runtime: model.RuntimeVM, Ephemeral: true, MinReplicas: 1, MaxReplicas: 3,
			CPUs: 2, MemoryMB: 4096, DiskGB: 40, CredentialID: cred.ID, Enabled: true, Recipe: recipe},
		{Name: "docs", ScopeKind: model.ScopeRepository, Scope: "clems4ever/docs",
			Runtime: model.RuntimeContainer, MinReplicas: 1, MaxReplicas: 2,
			CPUs: 2, MemoryMB: 2048, CredentialID: cred.ID, Enabled: true},
		{Name: "arm-experiment", ScopeKind: model.ScopeRepository, Scope: "clems4ever/runyard",
			Runtime: model.RuntimeVM, Ephemeral: true, MinReplicas: 1, MaxReplicas: 1,
			CPUs: 2, MemoryMB: 4096, DiskGB: 40, CredentialID: cred.ID, Enabled: false,
			Recipe: "curl -fsSL https://example.invalid/toolchain | sh\n"},
	}
	for i := range pools {
		pools[i].Defaults()
		created, err := db.CreatePool(ctx, pools[i])
		must(err)
		pools[i] = created
	}

	image := func(p model.Pool) string {
		return agent.ImageSpec{Variant: p.Image, Packages: p.Packages, Recipe: p.Recipe}.Name()
	}

	// The recipe web had when its build failed, before it was fixed. A
	// different recipe is a different image, which is why the history shows
	// two names.
	oldRecipe := "GO_VERSION=1.25.0\ninstall -d /opt/hostedtoolcache/go/${GO_VERSION}\ncurl -fsSL https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.bz2 | tar -xz -C /tmp\n"
	oldWeb := pools[1]
	oldWeb.Recipe = oldRecipe

	// web: built, after one failure that was fixed.
	logsDir := filepath.Join(layout.ImagesDir(), "logs")
	must(os.MkdirAll(logsDir, 0o700))
	write := func(pool model.Pool, phase model.ImagePhase, trigger, errMsg, body string, started time.Time, took time.Duration) {
		b, err := db.StartImageBuild(ctx, model.ImageBuild{
			Pool: pool.Name, Image: image(pool), Phase: model.ImageQueued,
			Trigger: trigger, StartedAt: started,
		})
		must(err)
		ended := started.Add(took)
		b.Phase, b.Error, b.EndedAt = phase, errMsg, &ended
		b.Log = filepath.Join(logsDir, fmt.Sprintf("%d.log", b.ID))
		must(os.WriteFile(b.Log, []byte(body), 0o600))
		must(db.UpdateImageBuild(ctx, b))
	}

	now := time.Now()
	failedLog := body(image(oldWeb), len(oldRecipe), failedTail)
	okLog := body(image(pools[1]), len(pools[1].Recipe), okTail)
	nestedLog := body(image(pools[0]), len(pools[0].Recipe), nestedTail)
	write(oldWeb, model.ImageFailed, model.ImageAutomatic,
		"the build machine stopped without reporting that it finished; the console above is what it said (a recipe that exits non-zero fails the build)",
		failedLog, now.Add(-95*time.Minute), 2*time.Minute+49*time.Second)
	write(pools[1], model.ImageSucceeded, model.ImageAutomatic, "", okLog,
		now.Add(-72*time.Minute), 8*time.Minute+31*time.Second)
	// The image the successful build published.
	must(os.WriteFile(filepath.Join(layout.ImagesDir(), image(pools[1])), []byte("golden"), 0o600))

	// runyard-org-vm-nested: the recipe does not work, and nothing will retry it.
	write(pools[0], model.ImageFailed, model.ImageAutomatic,
		"the build machine stopped without reporting that it finished; the console above is what it said (a recipe that exits non-zero fails the build)",
		nestedLog, now.Add(-14*time.Minute), 2*time.Minute+49*time.Second)

	fmt.Println("seeded", layout.Etc)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// body is the head every build's log has, followed by what this one did.
func body(image string, recipe int, tail string) string {
	return fmt.Sprintf(`==> building %s
==> %d packages, %d bytes of recipe
==> preparing a 20 GB disk from the cloud image
==> booting the build machine; everything below is its console
%s`, image, len(agent.ImageSpec{}.EffectivePackages()), recipe, tail)
}

const failedTail = `[    0.000000] Linux version 6.8.0-31-generic (buildd@lcy02-amd64-080)
cloud-init[721]: Cloud-init v. 24.1.3 running 'modules:config'
Get:14 http://archive.ubuntu.com/ubuntu noble/main amd64 build-essential
Setting up build-essential (12.10ubuntu1) ...
runner-fleet: running the pool recipe
+ GO_VERSION=1.25.0
+ install -d /opt/hostedtoolcache/go/1.25.0
+ curl -fsSL https://go.dev/dl/go1.25.0.linux-amd64.tar.bz2
curl: (22) The requested URL returned error: 404
runner-fleet: the image build failed with status 22
==> the build failed after 2m49s: the build machine stopped without reporting that it finished; the console above is what it said (a recipe that exits non-zero fails the build)
`

const okTail = `cloud-init[721]: Cloud-init v. 24.1.3 running 'modules:config'
Setting up build-essential (12.10ubuntu1) ...
runner-fleet: installing the actions runner 2.336.0
runner-fleet: running the pool recipe
+ GO_VERSION=1.25.0
+ install -d /opt/hostedtoolcache/go/1.25.0
+ mv /tmp/go /opt/hostedtoolcache/go/1.25.0/x64
runner-fleet: image ready
==> the build finished in 8m31s
`

const nestedTail = `cloud-init[721]: Cloud-init v. 24.1.3 running 'modules:final'
runner-fleet: running the pool recipe
+ install-the-toolchain --version 3
/usr/local/bin/recipe.sh: line 3: install-the-toolchain: command not found
runner-fleet: the image build failed with status 127
==> the build failed after 2m49s: the build machine stopped without reporting that it finished; the console above is what it said (a recipe that exits non-zero fails the build)
`
