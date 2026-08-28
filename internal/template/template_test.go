package template

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clems4ever/github-runner/internal/model"
)

const minimal = `{
  "version": 1,
  "pools": [{"name": "ci", "scope": "acme/widgets", "runtime": "container"}]
}`

func TestParseReadsADocument(t *testing.T) {
	doc, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(doc.Pools) != 1 || doc.Pools[0].Name != "ci" {
		t.Fatalf("got %+v", doc.Pools)
	}
}

// Every one of these is something a person will actually paste, and each gets
// an answer that says what to do about it.
func TestParseRefusesWhatIsNotATemplate(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"not json at all", `runner-fleet pools`, "not a pool template"},
		{"no version", `{"pools":[{"name":"ci"}]}`, `"version": 1`},
		{"a version from the future", `{"version":2,"pools":[{"name":"ci"}]}`, "version 2"},
		{"no pools", `{"version":1,"pools":[]}`, "no pools"},
		{"a pool with no name", `{"version":1,"pools":[{"runtime":"vm"}]}`, "pool 1 has no name"},
		{
			"the same pool twice",
			`{"version":1,"pools":[{"name":"ci"},{"name":"ci"}]}`,
			`"ci" appears twice`,
		},
		{
			"a misspelt field",
			`{"version":1,"pools":[{"name":"ci","ephemerel":true}]}`,
			`"ephemerel" is not a field`,
		},
		{
			"two documents in one file",
			minimal + minimal,
			"more than one document",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.input))
			if err == nil {
				t.Fatal("this was accepted, and should not have been")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the message does not say %q:\n%v", tc.want, err)
			}
		})
	}
}

// The obvious mistake is to paste what /api/pools returned, which looks almost
// like a template and carries three things that belong to one installation.
func TestPastingTheApisOwnPoolsSaysWhyItCannotWork(t *testing.T) {
	for _, field := range []string{"id", "credentialId", "createdAt", "updatedAt"} {
		t.Run(field, func(t *testing.T) {
			doc := `{"version":1,"pools":[{"name":"ci","` + field + `":1}]}`
			_, err := Parse([]byte(doc))
			if err == nil {
				t.Fatal("accepted a pool carrying " + field)
			}
			if !strings.Contains(err.Error(), "a template does not carry") {
				t.Fatalf("the message does not explain the field:\n%v", err)
			}
		})
	}
}

func TestApplyFillsInWhatTheDocumentLeavesOut(t *testing.T) {
	doc, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	pools, err := Apply(doc, Options{CredentialID: 7})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	pool := pools[0]
	if pool.CredentialID != 7 {
		t.Errorf("credential: got %d, want the one the import chose", pool.CredentialID)
	}
	// A template that says nothing means an enabled, ephemeral pool. The other
	// way round, an import would quietly deliver a fleet that does nothing.
	if !pool.Enabled {
		t.Error("a pool that says nothing about being enabled was imported switched off")
	}
	if !pool.Ephemeral {
		t.Error("a pool that says nothing about being ephemeral was imported as a reused runner")
	}
	if pool.Nested {
		t.Error("nested virtualisation was turned on without being asked for")
	}
	if pool.ScopeKind != model.ScopeRepository {
		t.Errorf("scope kind: got %q, want a repository", pool.ScopeKind)
	}
	if pool.CPUs == 0 || pool.MemoryMB == 0 || pool.Image == "" {
		t.Errorf("the model's defaults were not applied: %+v", pool)
	}
}

// The pointers exist for exactly this: false is a real answer, and must survive
// the trip.
func TestFalseSurvivesWhenTheTemplateSaysFalse(t *testing.T) {
	doc, err := Parse([]byte(`{"version":1,"pools":[
		{"name":"ci","scope":"acme/widgets","runtime":"vm","ephemeral":false,"enabled":false,"nested":true}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	pools, err := Apply(doc, Options{CredentialID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if pools[0].Ephemeral || pools[0].Enabled || !pools[0].Nested {
		t.Fatalf("the switches were not honoured: %+v", pools[0])
	}
}

func TestApplyNeedsACredential(t *testing.T) {
	doc, _ := Parse([]byte(minimal))
	if _, err := Apply(doc, Options{}); err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("want a message about choosing a credential, got %v", err)
	}
}

func TestTheScopeOverrideReplacesEveryScope(t *testing.T) {
	doc, err := Parse([]byte(`{"version":1,"pools":[
		{"name":"a","scope":"acme/widgets","runtime":"container"},
		{"name":"b","scope":"acme/other","runtime":"container"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	pools, err := Apply(doc, Options{CredentialID: 1, Scope: "someone-else/thing", ScopeKind: model.ScopeRepository})
	if err != nil {
		t.Fatal(err)
	}
	for _, pool := range pools {
		if pool.Scope != "someone-else/thing" {
			t.Fatalf("pool %q kept the template's scope %q", pool.Name, pool.Scope)
		}
	}
}

func TestAnOrganisationOverrideChangesTheKindToo(t *testing.T) {
	doc, _ := Parse([]byte(minimal))
	pools, err := Apply(doc, Options{CredentialID: 1, Scope: "acme", ScopeKind: model.ScopeOrganization})
	if err != nil {
		t.Fatal(err)
	}
	if pools[0].ScopeKind != model.ScopeOrganization || pools[0].Scope != "acme" {
		t.Fatalf("got %q %q", pools[0].ScopeKind, pools[0].Scope)
	}
}

// A template meant to be reused is written without a scope, and then the import
// has to supply one rather than inventing it.
func TestAPoolWithNoScopeNeedsOneFromTheImport(t *testing.T) {
	doc, err := Parse([]byte(`{"version":1,"pools":[{"name":"ci","runtime":"container"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Apply(doc, Options{CredentialID: 1})
	if err == nil || !strings.Contains(err.Error(), "no scope") {
		t.Fatalf("want a message about the missing scope, got %v", err)
	}

	if _, err := Apply(doc, Options{CredentialID: 1, Scope: "acme/widgets"}); err != nil {
		t.Fatalf("the same document with a scope should import: %v", err)
	}
}

func TestApplyRefusesAPoolThatCannotWork(t *testing.T) {
	doc, err := Parse([]byte(`{"version":1,"pools":[
		{"name":"fine","scope":"acme/widgets","runtime":"container"},
		{"name":"greedy","scope":"acme/widgets","runtime":"container","cpus":9000}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	pools, err := Apply(doc, Options{CredentialID: 1})
	if err == nil {
		t.Fatal("a pool with 9000 cpus was accepted")
	}
	// Which pool, and what is wrong with it — a document can be long.
	if !strings.Contains(err.Error(), `"greedy"`) || !strings.Contains(err.Error(), "cpus") {
		t.Fatalf("the message does not locate the problem:\n%v", err)
	}
	// And nothing comes back: a caller cannot import the good half by accident.
	if pools != nil {
		t.Fatalf("got %d pools alongside the error", len(pools))
	}
}

func TestApplyRefusesAScopeThatIsNotOne(t *testing.T) {
	doc, _ := Parse([]byte(minimal))
	_, err := Apply(doc, Options{CredentialID: 1, Scope: "not a repository", ScopeKind: model.ScopeRepository})
	if err == nil || !strings.Contains(err.Error(), "owner/name") {
		t.Fatalf("want the scope explained, got %v", err)
	}
}

func TestExportCarriesNothingLocalToThisInstallation(t *testing.T) {
	raw, err := json.Marshal(Export([]model.Pool{{
		ID: 4, Name: "ci", Scope: "acme/widgets", ScopeKind: model.ScopeRepository,
		Runtime: model.RuntimeContainer, CredentialID: 9, CPUs: 2, MemoryMB: 4096,
		MinReplicas: 1, MaxReplicas: 2, Enabled: true,
	}}))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"id"`, `"credentialId"`, `"createdAt"`, `"updatedAt"`} {
		if strings.Contains(string(raw), field) {
			t.Errorf("the export carries %s, which belongs to one database:\n%s", field, raw)
		}
	}
}

// Export and import are one feature, not two: what comes out has to go back in
// unchanged, or an operator moving a fleet to a second host loses something
// without being told.
func TestExportRoundTrips(t *testing.T) {
	original := []model.Pool{
		{
			Name: "ci-container", Scope: "acme/widgets", ScopeKind: model.ScopeRepository,
			Runtime: model.RuntimeContainer, Ephemeral: true, Enabled: true,
			MinReplicas: 1, MaxReplicas: 3, CPUs: 2, MemoryMB: 4096,
			Labels: []string{"fast"}, Image: "default",
		},
		{
			Name: "ci-vm", Scope: "acme", ScopeKind: model.ScopeOrganization,
			Runtime: model.RuntimeVM, Nested: true, Ephemeral: false, Enabled: false,
			MinReplicas: 2, MaxReplicas: 2, CPUs: 4, MemoryMB: 8192, DiskGB: 60,
			Image: "default",
		},
	}

	raw, err := json.Marshal(Export(original))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Parse(raw)
	if err != nil {
		t.Fatalf("what Export wrote does not parse: %v\n%s", err, raw)
	}
	back, err := Apply(doc, Options{CredentialID: 9})
	if err != nil {
		t.Fatalf("what Export wrote does not import: %v", err)
	}

	if len(back) != len(original) {
		t.Fatalf("got %d pools back, sent %d", len(back), len(original))
	}
	for i, want := range original {
		got := back[i]
		got.CredentialID = 0 // supplied by the import, not the document
		want.Defaults()
		want.CredentialID = 0
		if got.Name != want.Name || got.Scope != want.Scope || got.ScopeKind != want.ScopeKind ||
			got.Runtime != want.Runtime || got.Nested != want.Nested || got.Ephemeral != want.Ephemeral ||
			got.Enabled != want.Enabled || got.MinReplicas != want.MinReplicas ||
			got.MaxReplicas != want.MaxReplicas || got.CPUs != want.CPUs || got.MemoryMB != want.MemoryMB ||
			got.DiskGB != want.DiskGB || got.Image != want.Image ||
			strings.Join(got.Labels, ",") != strings.Join(want.Labels, ",") {
			t.Errorf("pool %d changed on the way round:\n got %+v\nwant %+v", i, got, want)
		}
	}
}

// The templates in this repository are handed to people to import. If one of
// them stops importing, this is where that is found — not in a support message.
func TestTheShippedTemplatesImport(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "templates", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no templates are shipped; this test is pointed at the wrong place")
	}

	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := Parse(raw)
			if err != nil {
				t.Fatalf("this template does not parse: %v", err)
			}
			if doc.Description == "" {
				t.Error("a shipped template should say what it is for")
			}
			if _, err := Apply(doc, Options{CredentialID: 1}); err != nil {
				t.Fatalf("this template does not import: %v", err)
			}
			// And with a scope of the importer's own, since that is how anyone
			// but this repository would use it.
			if _, err := Apply(doc, Options{CredentialID: 1, Scope: "someone/else", ScopeKind: model.ScopeRepository}); err != nil {
				t.Fatalf("this template does not import onto another repository: %v", err)
			}
		})
	}
}

// The CI template is the answer to "which runners do these jobs need", and the
// answer has two halves: containers for the jobs that only need a toolchain,
// machines for the jobs that need a Docker daemon or a systemd of their own.
func TestTheCITemplateCoversBothKindsOfJob(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "templates", "github-runner-ci.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	pools, err := Apply(doc, Options{CredentialID: 1})
	if err != nil {
		t.Fatal(err)
	}

	byRuntime := map[model.Runtime]model.Pool{}
	for _, pool := range pools {
		byRuntime[pool.Runtime] = pool
	}
	container, ok := byRuntime[model.RuntimeContainer]
	if !ok {
		t.Fatal("no container pool: the go and ui jobs have nowhere to run")
	}
	machine, ok := byRuntime[model.RuntimeVM]
	if !ok {
		t.Fatal("no vm pool: container-runner needs its own Docker daemon and installer needs its own systemd")
	}

	// A workflow selects these with runs-on, and the runtime labels are what it
	// selects on.
	if !has(container.EffectiveLabels(), "container") || !has(machine.EffectiveLabels(), "vm") {
		t.Fatalf("the pools cannot be told apart by runs-on: %v / %v",
			container.EffectiveLabels(), machine.EffectiveLabels())
	}
	// The installer job installs a service and then deletes it again, so its
	// runner must not be reused by the next job.
	if !machine.Ephemeral {
		t.Error("the vm pool is not ephemeral, and the installer job leaves the host changed")
	}
	// Docker in a machine needs no KVM, and nested is a hole in the boundary.
	if machine.Nested {
		t.Error("the vm pool asks for nested virtualisation, which none of these jobs need")
	}
	if !machine.Elastic() && !container.Elastic() {
		t.Error("neither pool can scale, so a second job of either kind waits for the first")
	}
}

func has(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

// A template is the portable form of a pool, and a pool whose runners have the
// toolchain baked in is not portable without what bakes it.
func TestATemplateCarriesWhatAPoolBakesIn(t *testing.T) {
	recipe := "#!/usr/bin/env bash\nset -euo pipefail\ninstall-the-toolchain\n"
	pools := []model.Pool{{
		Name: "ci-vm", ScopeKind: model.ScopeRepository, Scope: "o/r", Runtime: model.RuntimeVM,
		MinReplicas: 1, MaxReplicas: 3, CPUs: 4, MemoryMB: 8192, DiskGB: 40, Image: "default",
		Packages: []string{"conntrack", "nftables"}, Recipe: recipe,
		CredentialID: 1, Enabled: true,
	}}

	doc := Export(pools)
	if strings.Join(doc.Pools[0].Packages, ",") != "conntrack,nftables" {
		t.Fatalf("the export dropped the packages: %+v", doc.Pools[0])
	}
	if doc.Pools[0].Recipe != recipe {
		t.Fatalf("the export changed the recipe: %q", doc.Pools[0].Recipe)
	}

	// Through JSON, which is the form it is actually moved in.
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var back Document
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	imported, err := Apply(back, Options{CredentialID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if imported[0].Recipe != recipe {
		t.Fatalf("the recipe came back as %q", imported[0].Recipe)
	}
	if strings.Join(imported[0].Packages, ",") != "conntrack,nftables" {
		t.Fatalf("the packages came back as %v", imported[0].Packages)
	}
}

// A pool that bakes nothing exports nothing, so the templates in this
// repository do not grow two empty fields.
func TestATemplateOmitsWhatAPoolDoesNotBake(t *testing.T) {
	doc := Export([]model.Pool{{
		Name: "ci-container", ScopeKind: model.ScopeRepository, Scope: "o/r",
		Runtime: model.RuntimeContainer, MinReplicas: 1, MaxReplicas: 1,
		CPUs: 2, MemoryMB: 4096, Image: "default", CredentialID: 1, Enabled: true,
	}})
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"packages", "recipe"} {
		if strings.Contains(string(raw), unwanted) {
			t.Errorf("%q is in a template for a pool that has none:\n%s", unwanted, raw)
		}
	}
}

// A container pool cannot bake anything, and an import that says otherwise is
// refused before anything is written rather than having the fields dropped.
func TestImportingARecipeOntoAContainerPoolIsRefused(t *testing.T) {
	doc := Document{Version: Version, Pools: []Pool{{
		Name: "ci-container", Scope: "o/r", Runtime: model.RuntimeContainer,
		Recipe: "echo hello\n",
	}}}
	if _, err := Apply(doc, Options{CredentialID: 1}); err == nil {
		t.Fatal("a container pool was imported with a recipe it cannot run")
	}
}
