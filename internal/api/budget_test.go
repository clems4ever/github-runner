package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/resources"
)

// settingsBudget is the budget as the settings page reads it back.
func (h *harness) settingsBudget() model.Budget {
	h.t.Helper()
	var body struct {
		Budget model.Budget `json:"budget"`
	}
	h.decode(h.do(http.MethodGet, "/api/settings", nil), &body)
	return body.Budget
}

// A daemon that has never been given a budget reports one that caps nothing,
// rather than nothing at all: the settings page has a form to fill in either
// way, and it should not have to tell an empty budget from a missing one.
func TestAFreshInstallHasAnUncappedBudget(t *testing.T) {
	h := newHarness(t)

	if got := h.settingsBudget(); got.Configured() {
		t.Fatalf("a fresh install came with a budget: %+v", got)
	}
}

func TestABudgetIsSavedAndReadBack(t *testing.T) {
	h := newHarness(t)
	want := model.Budget{CPUs: 8, CPUWeight: 50, MemoryMB: 24576, HardMemory: true}

	resp := h.do(http.MethodPut, "/api/settings/budget", want)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
	resp.Body.Close()

	if got := h.settingsBudget(); got != want {
		t.Fatalf("saved %+v, read back %+v", want, got)
	}
}

// Saving a budget asks for a reconcile pass, because that is where it reaches
// the host: the slice is written there and the pools are rationed there. An
// operator who has just lowered a ceiling should not have to wonder for half a
// minute whether it took.
func TestSavingABudgetAsksForAPass(t *testing.T) {
	h := newHarness(t)
	before := h.nudges

	resp := h.do(http.MethodPut, "/api/settings/budget", model.Budget{CPUs: 4})
	resp.Body.Close()

	if h.nudges == before {
		t.Fatal("the budget was stored and nothing was asked to apply it")
	}
}

// A budget that could only be a mistake is refused where somebody is there to
// read why, rather than becoming a ceiling no machine could boot inside.
func TestANonsenseBudgetIsRefusedWithAReason(t *testing.T) {
	h := newHarness(t)

	for name, budget := range map[string]model.Budget{
		"a gigabyte entered as MiB": {MemoryMB: 8},
		"negative processors":       {CPUs: -4},
		"a wall with no ceiling":    {HardMemory: true},
		"a weight off the scale":    {CPUWeight: 99999},
	} {
		resp := h.do(http.MethodPut, "/api/settings/budget", budget)
		body := resp.StatusCode
		resp.Body.Close()
		if body != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400", name, body)
		}
	}

	// And none of them were stored on the way to being refused.
	if got := h.settingsBudget(); got.Configured() {
		t.Fatalf("a refused budget was saved anyway: %+v", got)
	}
}

// Removing a cap has to be possible, or a budget set once could never be
// undone from the UI.
func TestABudgetCanBeRemoved(t *testing.T) {
	h := newHarness(t)
	h.do(http.MethodPut, "/api/settings/budget", model.Budget{CPUs: 4, MemoryMB: 8192}).Body.Close()

	resp := h.do(http.MethodPut, "/api/settings/budget", model.Budget{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
	resp.Body.Close()

	if got := h.settingsBudget(); got.Capped() {
		t.Fatalf("the cap outlived its removal: %+v", got)
	}
}

// The budget goes out beside the commitment, because the interesting number is
// the gap between them: what the pools would take at full stretch, and what
// they are allowed to.
func TestTheResourceReportCarriesTheBudgetBesideTheCommitment(t *testing.T) {
	h := newHarness(t)
	h.resources.ready = true
	h.resources.report = resources.Report{
		At:   time.Now().UTC(),
		Host: resources.Host{CPUs: 16, MemoryTotalBytes: 64 * 1024 * 1024 * 1024},
	}
	h.do(http.MethodPut, "/api/settings/budget", model.Budget{CPUs: 8, MemoryMB: 16384}).Body.Close()

	var body struct {
		Ready     bool             `json:"ready"`
		Budget    model.Budget     `json:"budget"`
		Committed model.Commitment `json:"committed"`
	}
	h.decode(h.do(http.MethodGet, "/api/resources", nil), &body)

	if !body.Ready {
		t.Fatal("the report is not ready")
	}
	if body.Budget.CPUs != 8 || body.Budget.MemoryMB != 16384 {
		t.Fatalf("the budget is not on the resources page: %+v", body.Budget)
	}
}

// And an uncapped host still gets a budget field, so the page has one thing to
// render rather than two shapes to tell apart.
func TestTheResourceReportSaysWhenNothingIsCapped(t *testing.T) {
	h := newHarness(t)
	h.resources.ready = true
	h.resources.report = resources.Report{At: time.Now().UTC()}

	var body map[string]any
	h.decode(h.do(http.MethodGet, "/api/resources", nil), &body)

	budget, ok := body["budget"]
	if !ok {
		t.Fatal("an uncapped host has no budget field at all")
	}
	if budget.(map[string]any)["cpus"] != float64(0) {
		t.Fatalf("got %v", budget)
	}
}

// The budget can create machines' worth of work on this host, so it is behind
// the same authentication as everything else.
func TestTheBudgetNeedsCredentials(t *testing.T) {
	h := newHarness(t)

	encoded, err := json.Marshal(model.Budget{CPUs: 1})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPut, h.server.URL+"/api/settings/budget",
		bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the budget was writable without credentials: %d", resp.StatusCode)
	}
}
