package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clems4ever/github-runner/internal/github"
	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/template"
)

// document is a template as it arrives: raw JSON, exactly as someone pasted it.
func document(pools string) json.RawMessage {
	return json.RawMessage(`{"version":1,"name":"two pools","pools":[` + pools + `]}`)
}

const containerPool = `{"name":"ci-container","scope":"clems4ever/github-runner","runtime":"container"}`
const vmPool = `{"name":"ci-vm","scope":"clems4ever/github-runner","runtime":"vm","cpus":4,"memoryMb":8192,"diskGb":40}`

// importBody is the request, with the fields a caller may leave out left out.
func (h *harness) importBody(doc json.RawMessage, extra map[string]any) map[string]any {
	body := map[string]any{"document": doc, "credentialId": h.credID}
	for key, value := range extra {
		body[key] = value
	}
	return body
}

type importResponse struct {
	Pools []struct {
		Name   string     `json:"name"`
		Action string     `json:"action"`
		Pool   model.Pool `json:"pool"`
	} `json:"pools"`
	DryRun bool   `json:"dryRun"`
	Name   string `json:"name"`
}

func TestImportCreatesThePoolsInTheTemplate(t *testing.T) {
	h := newHarness(t)

	resp := h.do("POST", "/api/pools/import", h.importBody(document(containerPool+","+vmPool), nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	var body importResponse
	h.decode(resp, &body)

	if len(body.Pools) != 2 {
		t.Fatalf("got %+v", body.Pools)
	}
	for _, outcome := range body.Pools {
		if outcome.Action != "create" {
			t.Errorf("%s: got %q", outcome.Name, outcome.Action)
		}
		// The credential is the one the import chose, not one the document
		// could have named.
		if outcome.Pool.CredentialID != h.credID {
			t.Errorf("%s: registered with credential %d", outcome.Name, outcome.Pool.CredentialID)
		}
	}
	// The document's own name comes back, so the UI can say what was imported.
	if body.Name != "two pools" {
		t.Errorf("the document's name was lost: %q", body.Name)
	}

	pools, err := h.store.ListPools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 2 {
		t.Fatalf("the fleet has %d pools", len(pools))
	}
	// And the daemon is asked to act on them now rather than at the next tick.
	if h.nudges != 1 {
		t.Errorf("got %d nudges, want 1", h.nudges)
	}
}

func TestADryRunPreviewsWithoutWritingOrNudging(t *testing.T) {
	h := newHarness(t)

	resp := h.do("POST", "/api/pools/import", h.importBody(document(containerPool), map[string]any{"dryRun": true}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	var body importResponse
	h.decode(resp, &body)
	if !body.DryRun || len(body.Pools) != 1 || body.Pools[0].Action != "create" {
		t.Fatalf("got %+v", body)
	}

	pools, _ := h.store.ListPools(context.Background())
	if len(pools) != 0 {
		t.Fatalf("the preview wrote %d pools", len(pools))
	}
	if h.nudges != 0 {
		t.Errorf("the preview woke the reconciler %d times", h.nudges)
	}
}

func TestImportSaysWhatIsWrongWithTheTemplate(t *testing.T) {
	h := newHarness(t)

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{
			"no template at all",
			map[string]any{"credentialId": h.credID},
			"there is no template here",
		},
		{
			"not a template",
			h.importBody(json.RawMessage(`{"pools":[]}`), nil),
			`"version": 1`,
		},
		{
			"a pool that cannot work",
			h.importBody(document(`{"name":"greedy","scope":"a/b","runtime":"vm","cpus":9000}`), nil),
			"greedy",
		},
		{
			"no credential chosen",
			map[string]any{"document": document(containerPool)},
			"credential",
		},
		{
			"a scope the template does not have",
			h.importBody(document(`{"name":"ci","runtime":"vm"}`), nil),
			"no scope",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.do("POST", "/api/pools/import", tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("got %d, want 400: %s", resp.StatusCode, readBody(t, resp))
			}
			var body map[string]string
			h.decode(resp, &body)
			if !strings.Contains(body["error"], tc.want) {
				t.Fatalf("the message does not mention %q:\n%s", tc.want, body["error"])
			}
		})
	}

	if pools, _ := h.store.ListPools(context.Background()); len(pools) != 0 {
		t.Fatalf("a refused import wrote %d pools", len(pools))
	}
}

// The same refusal a pool gets when it is created by hand, with the same way
// out: an app cannot widen its own access, so the most the daemon can do is put
// the page that fixes it one click away.
func TestImportRefusesAScopeGitHubWillNotServe(t *testing.T) {
	h := newHarness(t)
	h.checkAccess = func(ctx context.Context, id int64, scope github.Scope) error {
		return &github.Error{
			Status:   http.StatusNotFound,
			Scope:    scope.Path,
			Message:  "Not Found",
			Advice:   "the app is not installed on this repository",
			GrantURL: "https://github.com/settings/installations/42",
		}
	}

	resp := h.do("POST", "/api/pools/import", h.importBody(document(containerPool+","+vmPool), nil))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", resp.StatusCode)
	}
	var body map[string]string
	h.decode(resp, &body)
	if body["grantUrl"] != "https://github.com/settings/installations/42" {
		t.Errorf("no way out was offered: %v", body)
	}
	if pools, _ := h.store.ListPools(context.Background()); len(pools) != 0 {
		t.Fatalf("the refused import wrote %d pools", len(pools))
	}
}

// A dry run has to hit the same refusal. A preview that says "create" for a
// pool GitHub will not serve is a preview that lied.
func TestADryRunAsksGitHubToo(t *testing.T) {
	h := newHarness(t)
	h.checkAccess = func(ctx context.Context, id int64, scope github.Scope) error {
		return &github.Error{Status: http.StatusNotFound, Scope: scope.Path, Message: "Not Found"}
	}

	resp := h.do("POST", "/api/pools/import", h.importBody(document(containerPool), map[string]any{"dryRun": true}))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want the preview to refuse as well", resp.StatusCode)
	}
	resp.Body.Close()
}

// Two pools on one repository is one question, not two. Asking per pool would
// spend a fleet's worth of API calls on a template of any size.
func TestImportAsksGitHubOncePerScope(t *testing.T) {
	h := newHarness(t)
	var asked []string
	h.checkAccess = func(ctx context.Context, id int64, scope github.Scope) error {
		asked = append(asked, string(scope.Kind)+":"+scope.Path)
		return nil
	}

	resp := h.do("POST", "/api/pools/import", h.importBody(document(containerPool+","+vmPool), nil))
	resp.Body.Close()
	if len(asked) != 1 {
		t.Fatalf("asked GitHub %d times for one repository: %v", len(asked), asked)
	}
}

func TestImportingOverAPoolNeedsToBeAskedFor(t *testing.T) {
	h := newHarness(t)

	first := h.do("POST", "/api/pools/import", h.importBody(document(containerPool), nil))
	first.Body.Close()

	again := h.do("POST", "/api/pools/import", h.importBody(document(containerPool), nil))
	if again.StatusCode != http.StatusConflict {
		t.Fatalf("got %d, want 409: %s", again.StatusCode, readBody(t, again))
	}

	over := h.do("POST", "/api/pools/import", h.importBody(document(containerPool), map[string]any{"replaceExisting": true}))
	if over.StatusCode != http.StatusOK {
		t.Fatalf("got %d: %s", over.StatusCode, readBody(t, over))
	}
	var body importResponse
	h.decode(over, &body)
	if body.Pools[0].Action != "update" {
		t.Fatalf("got %q, want an update", body.Pools[0].Action)
	}
}

func TestTheScopeOverrideIsApplied(t *testing.T) {
	h := newHarness(t)

	resp := h.do("POST", "/api/pools/import", h.importBody(document(containerPool), map[string]any{
		"scope": "someone-else/thing", "scopeKind": "repository",
	}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()

	pools, _ := h.store.ListPools(context.Background())
	if pools[0].Scope != "someone-else/thing" {
		t.Fatalf("the pool is on %q", pools[0].Scope)
	}
}

// "replace" is not "replaceExisting", and an import that quietly ignored it
// would be an import someone believes overwrote their pools.
func TestImportRejectsARequestFieldItDoesNotKnow(t *testing.T) {
	h := newHarness(t)
	resp := h.do("POST", "/api/pools/import", map[string]any{
		"document": document(containerPool), "credentialId": h.credID, "replace": true,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d for a field the daemon does not know, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

// ---------------------------------------------------------------------------
// Export
// ---------------------------------------------------------------------------

func TestExportIsATemplateThatImportsSomewhereElse(t *testing.T) {
	source := newHarness(t)
	created := source.do("POST", "/api/pools", source.samplePool())
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("got %d", created.StatusCode)
	}
	created.Body.Close()

	resp := source.do("GET", "/api/pools/export", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export: %d", resp.StatusCode)
	}
	// It is meant to be saved, so it says so.
	if !strings.Contains(resp.Header.Get("Content-Disposition"), ".json") {
		t.Errorf("the export does not offer itself as a file: %q", resp.Header.Get("Content-Disposition"))
	}
	exported := readBody(t, resp)

	// A different daemon, a different credential: what came out of one goes
	// into the other, which is the whole point of the format.
	destination := newHarness(t)
	into := destination.do("POST", "/api/pools/import", destination.importBody(json.RawMessage(exported), nil))
	if into.StatusCode != http.StatusOK {
		t.Fatalf("what the export wrote does not import: %d: %s", into.StatusCode, readBody(t, into))
	}
	var body importResponse
	destination.decode(into, &body)

	pools, _ := destination.store.ListPools(context.Background())
	if len(pools) != 1 || pools[0].Name != "web" {
		t.Fatalf("got %+v", pools)
	}
	if pools[0].CredentialID != destination.credID {
		t.Errorf("the imported pool points at credential %d, not this host's", pools[0].CredentialID)
	}
	if pools[0].MinReplicas != 2 || pools[0].MaxReplicas != 4 {
		t.Errorf("the scaling bounds did not survive the trip: %+v", pools[0])
	}
	if strings.Join(pools[0].Labels, ",") != "gpu" {
		t.Errorf("the labels did not survive the trip: %v", pools[0].Labels)
	}
}

func TestExportingAnEmptyFleetIsHonestAboutIt(t *testing.T) {
	h := newHarness(t)
	resp := h.do("GET", "/api/pools/export", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	// It is a valid document with nothing in it, and importing it says so
	// rather than reporting a successful import of nothing.
	if _, err := template.Parse([]byte(body)); err == nil {
		t.Fatal("an empty export claims to be importable")
	}
}

// The template checked into this repository is what someone is told to import.
// It goes in through the real route, on a real database.
func TestTheShippedTemplateImportsThroughTheAPI(t *testing.T) {
	h := newHarness(t)
	raw, err := os.ReadFile(filepath.Join("..", "..", "templates", "github-runner-ci.json"))
	if err != nil {
		t.Fatal(err)
	}

	resp := h.do("POST", "/api/pools/import", h.importBody(json.RawMessage(raw), nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	var body importResponse
	h.decode(resp, &body)
	if len(body.Pools) != 2 {
		t.Fatalf("got %d pools from the CI template", len(body.Pools))
	}

	pools, _ := h.store.ListPools(context.Background())
	runtimes := map[model.Runtime]bool{}
	for _, pool := range pools {
		runtimes[pool.Runtime] = true
		if !pool.Enabled {
			t.Errorf("%s was imported switched off", pool.Name)
		}
	}
	if !runtimes[model.RuntimeContainer] || !runtimes[model.RuntimeVM] {
		t.Fatalf("the imported fleet cannot serve both kinds of job: %v", runtimes)
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
