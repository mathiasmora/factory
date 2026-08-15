package controlplane

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/owainlewis/factory/internal/protocol"
)

func writeLabelConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "labels.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadLabelPollerConfigReportsMissingFileWithoutError(t *testing.T) {
	config, found, err := loadLabelPollerConfig(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil || found || len(config.Triggers) != 0 {
		t.Fatalf("config = %#v, found %v, err %v", config, found, err)
	}
}

func TestLoadLabelPollerConfigClampsInterval(t *testing.T) {
	path := writeLabelConfig(t, `
poll_interval_seconds = 1

[[trigger]]
routine = "Implement"
label = "factory:dispatch"
`)
	config, found, err := loadLabelPollerConfig(path)
	if err != nil || !found {
		t.Fatalf("found %v, err %v", found, err)
	}
	if config.interval() != labelPollMinimumInterval {
		t.Fatalf("interval = %s, want %s", config.interval(), labelPollMinimumInterval)
	}
	unset := labelPollerConfig{}
	if unset.interval() != labelPollDefaultInterval {
		t.Fatalf("default interval = %s, want %s", unset.interval(), labelPollDefaultInterval)
	}
}

func TestLoadLabelPollerConfigRejectsIncompleteTriggers(t *testing.T) {
	for name, body := range map[string]string{
		"missing routine": "[[trigger]]\nlabel = \"factory:dispatch\"\n",
		"missing label":   "[[trigger]]\nroutine = \"Implement\"\n",
		"invalid state":   "[[trigger]]\nroutine = \"Implement\"\nlabel = \"x\"\nstate = \"merged\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := loadLabelPollerConfig(writeLabelConfig(t, body)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestResolveLabelPromptAppendsTrustedOccurrence(t *testing.T) {
	prompt := resolveLabelPrompt("Implement the ticket.", "factory:dispatch",
		"github.com/owainlewis/factory",
		labelledIssue{Number: 582, Title: "Edge cases", URL: "https://example.test/582",
			State: "open", Labels: []string{"factory:dispatch"}})
	if !strings.HasPrefix(prompt, "Implement the ticket.") {
		t.Fatalf("operator prompt was not preserved: %q", prompt)
	}
	_, encoded, found := strings.Cut(prompt, "Trusted label occurrence:\n\n")
	if !found {
		t.Fatalf("trusted occurrence missing: %q", prompt)
	}
	var occurrence struct {
		Type       string `json:"type"`
		Repository string `json:"repository"`
		Number     int    `json:"number"`
		Title      string `json:"title"`
	}
	if err := json.Unmarshal([]byte(encoded), &occurrence); err != nil {
		t.Fatal(err)
	}
	if occurrence.Type != "label" || occurrence.Number != 582 ||
		occurrence.Repository != "github.com/owainlewis/factory" || occurrence.Title != "Edge cases" {
		t.Fatalf("occurrence = %#v", occurrence)
	}
}

// The poller relies on the UNIQUE request_key rather than its own bookkeeping,
// so a repeated cycle over an unchanged issue must not create a second Work.
func TestAdmitLabelledIssueIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	worker := registerTestWorker(t, store, workerA, 1, protocol.RepositoryRegistration{
		Key: "factory", RemoteIdentity: "github.com/owainlewis/factory",
	})
	routine, err := store.CreateRoutine(context.Background(), protocol.SaveRoutineRequest{
		Name: "Implement ticket", Prompt: "Implement the ticket.", Runtime: protocol.RuntimeCodex,
		TimeoutSeconds: 3600, ConcurrencyLimit: 10,
		RepositoryIDs: []string{worker.Repositories[0].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	routineID, snapshot, err := store.labelTriggerRoutine(context.Background(), "implement TICKET")
	if err != nil {
		t.Fatalf("routine lookup by normalized name failed: %v", err)
	}
	if routineID != routine.ID || len(snapshot.Repositories) != 1 {
		t.Fatalf("routineID = %q, snapshot = %#v", routineID, snapshot)
	}
	issue := labelledIssue{Number: 582, Title: "Edge cases", URL: "https://example.test/582", State: "open"}
	created, err := store.admitLabelledIssue(context.Background(), routineID, snapshot,
		"factory:dispatch", snapshot.Repositories[0], issue)
	if err != nil || !created {
		t.Fatalf("first admission: created %v, err %v", created, err)
	}
	created, err = store.admitLabelledIssue(context.Background(), routineID, snapshot,
		"factory:dispatch", snapshot.Repositories[0], issue)
	if err != nil || created {
		t.Fatalf("second admission: created %v, err %v", created, err)
	}
	work, err := store.WorkPage(context.Background(), 50, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(work.Work) != 1 {
		t.Fatalf("expected exactly one Work record, got %d", len(work.Work))
	}
}

// The issue context must reach the agent, otherwise a label-triggered run
// cannot tell which issue it is for.
func TestAdmitLabelledIssueCarriesIssueIntoResolvedPrompt(t *testing.T) {
	store := newTestStore(t)
	worker := registerTestWorker(t, store, workerA, 1, protocol.RepositoryRegistration{
		Key: "factory", RemoteIdentity: "github.com/owainlewis/factory",
	})
	routine, err := store.CreateRoutine(context.Background(), protocol.SaveRoutineRequest{
		Name: "Implement ticket", Prompt: "Implement the ticket.", Runtime: protocol.RuntimeCodex,
		RepositoryIDs: []string{worker.Repositories[0].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, snapshot, err := store.labelTriggerRoutine(context.Background(), routine.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.admitLabelledIssue(context.Background(), routine.ID, snapshot,
		"factory:dispatch", snapshot.Repositories[0],
		labelledIssue{Number: 582, Title: "Edge cases", URL: "https://example.test/582", State: "open"},
	); err != nil {
		t.Fatal(err)
	}
	list, err := store.WorkPage(context.Background(), 50, "")
	if err != nil {
		t.Fatal(err)
	}
	detail, err := store.Work(context.Background(), list.Work[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Targets) != 1 {
		t.Fatalf("targets = %#v", detail.Targets)
	}
	if !strings.Contains(detail.Targets[0].ResolvedPrompt, `"number":582`) {
		t.Fatalf("resolved prompt lost the issue: %q", detail.Targets[0].ResolvedPrompt)
	}
	// The stored Routine itself must stay untouched by the frozen snapshot.
	reloaded, err := store.Routine(context.Background(), routine.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Prompt != "Implement the ticket." {
		t.Fatalf("routine prompt was mutated: %q", reloaded.Prompt)
	}
}

func TestLabelTriggerRoutineRejectsArchivedRoutine(t *testing.T) {
	store := newTestStore(t)
	worker := registerTestWorker(t, store, workerA, 1, protocol.RepositoryRegistration{
		Key: "factory", RemoteIdentity: "github.com/owainlewis/factory",
	})
	routine, err := store.CreateRoutine(context.Background(), protocol.SaveRoutineRequest{
		Name: "Archived routine", Prompt: "Implement the ticket.", Runtime: protocol.RuntimeCodex,
		RepositoryIDs: []string{worker.Repositories[0].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetRoutineArchived(context.Background(), routine.ID,
		protocol.SetRoutineArchivedRequest{Archived: boolPointer(true), ExpectedGeneration: routine.Generation}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.labelTriggerRoutine(context.Background(), routine.Name); err == nil {
		t.Fatal("expected archived Routine to be refused")
	}
}
