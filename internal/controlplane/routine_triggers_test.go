package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/owainlewis/factory/internal/protocol"
)

func mergedBound() *time.Time {
	value := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	return &value
}

func TestNormalizeRoutineTriggersAppliesDefaults(t *testing.T) {
	normalized, err := normalizeRoutineTriggers([]protocol.RoutineTrigger{{Label: " factory:dispatch "}})
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized) != 1 {
		t.Fatalf("normalized = %#v", normalized)
	}
	trigger := normalized[0]
	if trigger.Kind != protocol.TriggerGitHubIssue || trigger.State != "open" ||
		trigger.Label != "factory:dispatch" || trigger.PollIntervalSeconds != protocol.DefaultTriggerPollSeconds {
		t.Fatalf("trigger = %#v", trigger)
	}
}

func TestNormalizeRoutineTriggersRejectsInvalidInput(t *testing.T) {
	for name, triggers := range map[string][]protocol.RoutineTrigger{
		"missing label":  {{Kind: protocol.TriggerGitHubIssue}},
		"unknown kind":   {{Kind: "github_discussion", Label: "x"}},
		"unknown state":  {{Kind: protocol.TriggerGitHubIssue, Label: "x", State: "draft"}},
		"merged issue":   {{Kind: protocol.TriggerGitHubIssue, Label: "x", State: "merged", MergedAfter: mergedBound()}},
		"interval floor": {{Kind: protocol.TriggerGitHubIssue, Label: "x", PollIntervalSeconds: 9}},
		"interval ceiling": {{
			Kind: protocol.TriggerGitHubIssue, Label: "x", PollIntervalSeconds: protocol.MaxTriggerPollInterval + 1,
		}},
		// A merged trigger without a bound replays the repository's history.
		"merged without bound": {{Kind: protocol.TriggerGitHubPullRequest, Label: "x", State: "merged"}},
		"duplicate label": {
			{Kind: protocol.TriggerGitHubIssue, Label: "factory:dispatch"},
			{Kind: protocol.TriggerGitHubIssue, Label: "Factory:Dispatch"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeRoutineTriggers(triggers); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestNormalizeRoutineTriggersRejectsMoreThanTheLimit(t *testing.T) {
	triggers := make([]protocol.RoutineTrigger, protocol.MaxRoutineTriggers+1)
	for index := range triggers {
		triggers[index] = protocol.RoutineTrigger{Kind: protocol.TriggerGitHubIssue, Label: string(rune('a' + index))}
	}
	if _, err := normalizeRoutineTriggers(triggers); err == nil {
		t.Fatal("expected the trigger limit to be enforced")
	}
}

func newTriggerTestRoutine(t *testing.T, store *Store) protocol.Routine {
	t.Helper()
	worker := registerTestWorker(t, store, workerA, 1, protocol.RepositoryRegistration{
		Key: "factory", RemoteIdentity: "github.com/owainlewis/factory",
	})
	routine, err := store.CreateRoutine(context.Background(), protocol.SaveRoutineRequest{
		Name: "Trigger routine", Prompt: "Implement the ticket.", Runtime: protocol.RuntimeCodex,
		TimeoutSeconds: 3600, ConcurrencyLimit: 10,
		RepositoryIDs: []string{worker.Repositories[0].ID},
		Triggers: []protocol.RoutineTrigger{
			{Kind: protocol.TriggerGitHubIssue, Label: "factory:dispatch", PollIntervalSeconds: 120},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return routine
}

func TestCreateRoutineStoresTriggersAndDefaultsToActive(t *testing.T) {
	store := newTestStore(t)
	routine := newTriggerTestRoutine(t, store)
	if !routine.Enabled {
		t.Fatal("a new Routine should be active")
	}
	if len(routine.Triggers) != 1 || routine.Triggers[0].Label != "factory:dispatch" ||
		routine.Triggers[0].PollIntervalSeconds != 120 {
		t.Fatalf("triggers = %#v", routine.Triggers)
	}
}

// A trigger polls the Routine's repositories, so it cannot be saved without one.
func TestCreateRoutineRequiresRepositoryForTrigger(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.CreateRoutine(context.Background(), protocol.SaveRoutineRequest{
		Name: "No repositories", Prompt: "Implement.", Runtime: protocol.RuntimeCodex,
		TimeoutSeconds: 3600, ConcurrencyLimit: 10,
		Triggers: []protocol.RoutineTrigger{{Kind: protocol.TriggerGitHubIssue, Label: "factory:dispatch"}},
	}); err == nil {
		t.Fatal("expected a repository to be required")
	}
}

// Editing unrelated fields must not restart a trigger's poll cycle.
func TestUpdateRoutinePreservesSurvivingTriggerCursor(t *testing.T) {
	store := newTestStore(t)
	routine := newTriggerTestRoutine(t, store)
	due, err := store.dueLabelTriggers(context.Background())
	if err != nil || len(due) != 1 {
		t.Fatalf("due = %#v, err %v", due, err)
	}
	if err := store.advanceLabelTriggerCursor(context.Background(), due[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateRoutine(context.Background(), routine.ID, protocol.SaveRoutineRequest{
		Name: "Renamed routine", Prompt: routine.Prompt, Runtime: routine.Runtime,
		TimeoutSeconds: routine.TimeoutSeconds, ConcurrencyLimit: routine.ConcurrencyLimit,
		RepositoryIDs:      []string{routine.Repositories[0].ID},
		Triggers:           routine.Triggers,
		ExpectedGeneration: routine.Generation,
	}); err != nil {
		t.Fatal(err)
	}
	after, err := store.dueLabelTriggers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("the surviving trigger lost its cursor and polls immediately: %#v", after)
	}
}

func TestSetRoutineEnabledIsGenerationChecked(t *testing.T) {
	store := newTestStore(t)
	routine := newTriggerTestRoutine(t, store)
	if _, err := store.SetRoutineEnabled(context.Background(), routine.ID,
		protocol.SetRoutineEnabledRequest{
			Enabled: boolPointer(false), ExpectedGeneration: routine.Generation + 1,
		}); err == nil {
		t.Fatal("expected a generation conflict")
	}
	paused, err := store.SetRoutineEnabled(context.Background(), routine.ID,
		protocol.SetRoutineEnabledRequest{
			Enabled: boolPointer(false), ExpectedGeneration: routine.Generation,
		})
	if err != nil {
		t.Fatal(err)
	}
	if paused.Enabled {
		t.Fatal("Routine stayed active")
	}
}

// Archiving must not leave a Routine that reports itself as active.
func TestArchivingDeactivatesRoutine(t *testing.T) {
	store := newTestStore(t)
	routine := newTriggerTestRoutine(t, store)
	archived, err := store.SetRoutineArchived(context.Background(), routine.ID,
		protocol.SetRoutineArchivedRequest{
			Archived: boolPointer(true), ExpectedGeneration: routine.Generation,
		})
	if err != nil {
		t.Fatal(err)
	}
	if archived.Enabled {
		t.Fatal("an archived Routine reported itself as active")
	}
	if _, err := store.SetRoutineEnabled(context.Background(), routine.ID,
		protocol.SetRoutineEnabledRequest{
			Enabled: boolPointer(true), ExpectedGeneration: archived.Generation,
		}); err == nil {
		t.Fatal("expected activating an archived Routine to be refused")
	}
}
