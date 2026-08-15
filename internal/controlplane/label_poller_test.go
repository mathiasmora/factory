package controlplane

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

func testIssue(number int) labelledItem {
	return labelledItem{
		Kind: labelKindIssue, Number: number, Title: "Edge cases",
		URL: "https://example.test/" + strconv.Itoa(number), State: "open",
	}
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
	if config.intervalSeconds() != protocol.MinTriggerPollInterval {
		t.Fatalf("interval = %d, want %d", config.intervalSeconds(), protocol.MinTriggerPollInterval)
	}
	unset := labelPollerConfig{}
	if unset.intervalSeconds() != protocol.DefaultTriggerPollSeconds {
		t.Fatalf("default interval = %d, want %d", unset.intervalSeconds(), protocol.DefaultTriggerPollSeconds)
	}
}

func TestLoadLabelPollerConfigDefaultsToIssueKind(t *testing.T) {
	config, found, err := loadLabelPollerConfig(writeLabelConfig(t, `
[[trigger]]
routine = "Implement"
label = "factory:dispatch"
`))
	if err != nil || !found {
		t.Fatalf("found %v, err %v", found, err)
	}
	if kind := config.Triggers[0].kind(); kind != labelKindIssue {
		t.Fatalf("kind = %q, want %q", kind, labelKindIssue)
	}
	if state := config.Triggers[0].state(); state != "open" {
		t.Fatalf("state = %q, want open", state)
	}
}

func TestLoadLabelPollerConfigRejectsInvalidTriggers(t *testing.T) {
	for name, body := range map[string]string{
		"missing routine": "[[trigger]]\nlabel = \"factory:dispatch\"\n",
		"missing label":   "[[trigger]]\nroutine = \"Implement\"\n",
		"unknown kind": "[[trigger]]\nroutine = \"Implement\"\nlabel = \"x\"\n" +
			"kind = \"discussion\"\n",
		"invalid issue state": "[[trigger]]\nroutine = \"Implement\"\nlabel = \"x\"\n" +
			"state = \"merged\"\n",
		"invalid pull request state": "[[trigger]]\nroutine = \"Implement\"\nlabel = \"x\"\n" +
			"kind = \"pull_request\"\nstate = \"draft\"\n",
		// A merged trigger without a bound would replay every historical
		// pull request on its first cycle.
		"merged without bound": "[[trigger]]\nroutine = \"Implement\"\nlabel = \"x\"\n" +
			"kind = \"pull_request\"\nstate = \"merged\"\n",
		"unparsable merged_after": "[[trigger]]\nroutine = \"Implement\"\nlabel = \"x\"\n" +
			"kind = \"pull_request\"\nstate = \"merged\"\nmerged_after = \"yesterday\"\n",
		"merged_after on issue trigger": "[[trigger]]\nroutine = \"Implement\"\nlabel = \"x\"\n" +
			"merged_after = \"2026-08-15T00:00:00Z\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := loadLabelPollerConfig(writeLabelConfig(t, body)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestLoadLabelPollerConfigAcceptsMergedPullRequestTrigger(t *testing.T) {
	config, found, err := loadLabelPollerConfig(writeLabelConfig(t, `
[[trigger]]
routine = "Advance the chain"
kind = "pull_request"
label = "factory:epic-chain"
state = "merged"
merged_after = "2026-08-15T00:00:00Z"
`))
	if err != nil || !found {
		t.Fatalf("found %v, err %v", found, err)
	}
	bound, err := config.Triggers[0].mergedAfter()
	if err != nil {
		t.Fatal(err)
	}
	if !bound.Equal(time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("merged_after = %s", bound)
	}
}

func TestMergedAfterBound(t *testing.T) {
	bound := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	for name, testCase := range map[string]struct {
		mergedAt string
		include  bool
	}{
		"after the bound":  {"2026-08-16T10:00:00Z", true},
		"exactly at bound": {"2026-08-15T00:00:00Z", true},
		"before the bound": {"2026-08-14T23:59:59Z", false},
		// An open pull request reports no merge instant and must not be
		// treated as merged at the zero time, which would precede any bound.
		"never merged": {"", false},
	} {
		t.Run(name, func(t *testing.T) {
			include, err := mergedAfterBound(testCase.mergedAt, bound)
			if err != nil {
				t.Fatal(err)
			}
			if include != testCase.include {
				t.Fatalf("include = %v, want %v", include, testCase.include)
			}
		})
	}
	if _, err := mergedAfterBound("not a time", bound); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestResolveLabelPromptAppendsTrustedOccurrence(t *testing.T) {
	prompt := resolveLabelPrompt("Implement the ticket.", "factory:dispatch",
		"github.com/owainlewis/factory", testIssue(582))
	if !strings.HasPrefix(prompt, "Implement the ticket.") {
		t.Fatalf("operator prompt was not preserved: %q", prompt)
	}
	_, encoded, found := strings.Cut(prompt, "Trusted label occurrence:\n\n")
	if !found {
		t.Fatalf("trusted occurrence missing: %q", prompt)
	}
	var occurrence struct {
		Type       string `json:"type"`
		Kind       string `json:"kind"`
		Repository string `json:"repository"`
		Number     int    `json:"number"`
		Title      string `json:"title"`
		MergedAt   string `json:"merged_at"`
	}
	if err := json.Unmarshal([]byte(encoded), &occurrence); err != nil {
		t.Fatal(err)
	}
	if occurrence.Type != "label" || occurrence.Kind != labelKindIssue || occurrence.Number != 582 ||
		occurrence.Repository != "github.com/owainlewis/factory" || occurrence.Title != "Edge cases" {
		t.Fatalf("occurrence = %#v", occurrence)
	}
	if occurrence.MergedAt != "" {
		t.Fatalf("issue occurrence carried a merge instant: %q", occurrence.MergedAt)
	}
}

func TestResolveLabelPromptDescribesPullRequest(t *testing.T) {
	prompt := resolveLabelPrompt("Advance the chain.", "factory:epic-chain",
		"github.com/owainlewis/factory", labelledItem{
			Kind: labelKindPullRequest, Number: 91, Title: "Sub-issue 3",
			URL: "https://example.test/pull/91", State: "merged",
			BaseBranch: "epic/42", MergedAt: "2026-08-16T10:00:00Z",
		})
	if !strings.Contains(prompt, "GitHub pull request") {
		t.Fatalf("prompt does not name the subject: %q", prompt)
	}
	_, encoded, _ := strings.Cut(prompt, "Trusted label occurrence:\n\n")
	var occurrence struct {
		Kind       string `json:"kind"`
		BaseBranch string `json:"base_branch"`
		MergedAt   string `json:"merged_at"`
	}
	if err := json.Unmarshal([]byte(encoded), &occurrence); err != nil {
		t.Fatal(err)
	}
	if occurrence.Kind != labelKindPullRequest || occurrence.BaseBranch != "epic/42" ||
		occurrence.MergedAt != "2026-08-16T10:00:00Z" {
		t.Fatalf("occurrence = %#v", occurrence)
	}
}

func newLabelTestRoutine(t *testing.T, store *Store, name string) (string, protocol.RoutineSnapshot) {
	t.Helper()
	worker := registerTestWorker(t, store, workerA, 1, protocol.RepositoryRegistration{
		Key: "factory", RemoteIdentity: "github.com/owainlewis/factory",
	})
	created, err := store.CreateRoutine(context.Background(), protocol.SaveRoutineRequest{
		Name: name, Prompt: "Implement the ticket.", Runtime: protocol.RuntimeCodex,
		TimeoutSeconds: 3600, ConcurrencyLimit: 10,
		RepositoryIDs: []string{worker.Repositories[0].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.labelTriggerRoutine(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	return created.ID, snapshot
}

// The poller relies on the UNIQUE request_key rather than its own bookkeeping,
// so a repeated cycle over an unchanged item must not create a second Work.
func TestAdmitLabelledItemIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	routineID, snapshot := newLabelTestRoutine(t, store, "Implement ticket")
	issue := testIssue(582)
	created, err := store.admitLabelledItem(context.Background(), routineID, snapshot,
		"factory:dispatch", snapshot.Repositories[0], issue)
	if err != nil || !created {
		t.Fatalf("first admission: created %v, err %v", created, err)
	}
	created, err = store.admitLabelledItem(context.Background(), routineID, snapshot,
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

// Issues and pull requests share a number space, so an issue and a pull request
// with the same number must not collide on one request key.
func TestAdmitLabelledItemSeparatesIssuesFromPullRequests(t *testing.T) {
	store := newTestStore(t)
	routineID, snapshot := newLabelTestRoutine(t, store, "Implement ticket")
	if _, err := store.admitLabelledItem(context.Background(), routineID, snapshot,
		"factory:chain", snapshot.Repositories[0], testIssue(91)); err != nil {
		t.Fatal(err)
	}
	created, err := store.admitLabelledItem(context.Background(), routineID, snapshot,
		"factory:chain", snapshot.Repositories[0], labelledItem{
			Kind: labelKindPullRequest, Number: 91, Title: "Sub-issue 3",
			URL: "https://example.test/pull/91", State: "merged",
		})
	if err != nil || !created {
		t.Fatalf("pull request admission: created %v, err %v", created, err)
	}
	work, err := store.WorkPage(context.Background(), 50, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(work.Work) != 2 {
		t.Fatalf("expected two Work records, got %d", len(work.Work))
	}
}

// The item context must reach the agent, otherwise a label-triggered run cannot
// tell which issue it is for.
func TestAdmitLabelledItemCarriesContextIntoResolvedPrompt(t *testing.T) {
	store := newTestStore(t)
	routineID, snapshot := newLabelTestRoutine(t, store, "Implement ticket")
	if _, err := store.admitLabelledItem(context.Background(), routineID, snapshot,
		"factory:dispatch", snapshot.Repositories[0], testIssue(582)); err != nil {
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
		t.Fatalf("resolved prompt lost the item: %q", detail.Targets[0].ResolvedPrompt)
	}
	// The stored Routine itself must stay untouched by the frozen snapshot.
	reloaded, err := store.Routine(context.Background(), routineID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Prompt != "Implement the ticket." {
		t.Fatalf("routine prompt was mutated: %q", reloaded.Prompt)
	}
}

func TestLabelTriggerRoutineRejectsArchivedRoutine(t *testing.T) {
	store := newTestStore(t)
	routineID, _ := newLabelTestRoutine(t, store, "Archived routine")
	routine, err := store.Routine(context.Background(), routineID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetRoutineArchived(context.Background(), routineID,
		protocol.SetRoutineArchivedRequest{
			Archived: boolPointer(true), ExpectedGeneration: routine.Generation,
		}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.labelTriggerRoutine(context.Background(), routineID); err == nil {
		t.Fatal("expected archived Routine to be refused")
	}
}

// An operator's labels.toml must survive the move to database triggers, and
// must not be reapplied afterwards: re-importing would resurrect triggers that
// were since deleted in the UI.
func TestImportLabelPollerConfigRunsOnce(t *testing.T) {
	store := newTestStore(t)
	routineID, _ := newLabelTestRoutine(t, store, "Advance the chain")
	home := t.TempDir()
	t.Setenv("FACTORY_DATA_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "labels.toml"), []byte(`
poll_interval_seconds = 120

[[trigger]]
routine      = "Advance the chain"
kind         = "pull_request"
label        = "factory:epic-chain"
state        = "merged"
merged_after = "2026-08-15T00:00:00Z"

[[trigger]]
routine = "No such Routine"
label   = "factory:orphan"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	store.importLabelPollerConfig(context.Background(), slog.New(slog.DiscardHandler))
	routine, err := store.Routine(context.Background(), routineID)
	if err != nil {
		t.Fatal(err)
	}
	if len(routine.Triggers) != 1 {
		t.Fatalf("triggers = %#v", routine.Triggers)
	}
	trigger := routine.Triggers[0]
	if trigger.Kind != protocol.TriggerGitHubPullRequest || trigger.State != "merged" ||
		trigger.Label != "factory:epic-chain" || trigger.PollIntervalSeconds != 120 {
		t.Fatalf("trigger = %#v", trigger)
	}
	if trigger.MergedAfter == nil || !trigger.MergedAfter.Equal(time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("merged_after = %v", trigger.MergedAfter)
	}
	// A trigger the operator deleted after the import must stay deleted.
	if _, err := store.db.ExecContext(context.Background(), `DELETE FROM routine_triggers`); err != nil {
		t.Fatal(err)
	}
	store.importLabelPollerConfig(context.Background(), slog.New(slog.DiscardHandler))
	if routine, err = store.Routine(context.Background(), routineID); err != nil {
		t.Fatal(err)
	}
	if len(routine.Triggers) != 0 {
		t.Fatalf("the import ran twice: %#v", routine.Triggers)
	}
}

// A paused Routine must starve its triggers, otherwise deactivating it in the
// UI would leave label admission running behind the operator's back.
func TestLabelTriggerRoutineRejectsDisabledRoutine(t *testing.T) {
	store := newTestStore(t)
	routineID, _ := newLabelTestRoutine(t, store, "Paused routine")
	routine, err := store.Routine(context.Background(), routineID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetRoutineEnabled(context.Background(), routineID,
		protocol.SetRoutineEnabledRequest{
			Enabled: boolPointer(false), ExpectedGeneration: routine.Generation,
		}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.labelTriggerRoutine(context.Background(), routineID); err == nil {
		t.Fatal("expected paused Routine to be refused")
	}
}

func TestDueLabelTriggersSkipsPausedRoutineAndRespectsInterval(t *testing.T) {
	store := newTestStore(t)
	routineID, _ := newLabelTestRoutine(t, store, "Trigger routine")
	routine, err := store.Routine(context.Background(), routineID)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.UpdateRoutine(context.Background(), routineID, protocol.SaveRoutineRequest{
		Name: routine.Name, Prompt: routine.Prompt, Runtime: routine.Runtime,
		TimeoutSeconds: routine.TimeoutSeconds, ConcurrencyLimit: routine.ConcurrencyLimit,
		RepositoryIDs:      []string{routine.Repositories[0].ID},
		Triggers:           []protocol.RoutineTrigger{{Kind: protocol.TriggerGitHubIssue, Label: "factory:dispatch"}},
		ExpectedGeneration: routine.Generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	due, err := store.dueLabelTriggers(context.Background())
	if err != nil || len(due) != 1 {
		t.Fatalf("due = %#v, err %v", due, err)
	}
	if due[0].pollIntervalSeconds != protocol.DefaultTriggerPollSeconds {
		t.Fatalf("interval = %d", due[0].pollIntervalSeconds)
	}
	// Advancing the cursor takes the trigger out of the due set until its own
	// interval elapses.
	if err := store.advanceLabelTriggerCursor(context.Background(), due[0]); err != nil {
		t.Fatal(err)
	}
	if due, err = store.dueLabelTriggers(context.Background()); err != nil || len(due) != 0 {
		t.Fatalf("due after advancing = %#v, err %v", due, err)
	}
	if _, err := store.SetRoutineEnabled(context.Background(), routineID,
		protocol.SetRoutineEnabledRequest{
			Enabled: boolPointer(false), ExpectedGeneration: saved.Generation,
		}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(),
		`UPDATE routine_triggers SET next_poll_at = NULL`); err != nil {
		t.Fatal(err)
	}
	if due, err = store.dueLabelTriggers(context.Background()); err != nil || len(due) != 0 {
		t.Fatalf("paused Routine still due: %#v, err %v", due, err)
	}
}
