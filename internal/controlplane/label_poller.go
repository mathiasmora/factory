package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/owainlewis/factory/internal/protocol"
)

// The label poller restores label-driven admission on top of the Routines
// model. It deliberately adds no schema, no protocol field, and no HTTP route:
// it reuses admitRoutine, whose request_key column is UNIQUE, so an item that
// was already admitted is skipped by the database rather than by bookkeeping
// this file would otherwise have to own.
//
// Configuration lives in $FACTORY_DATA_HOME/labels.toml and is reread every
// cycle, so triggers can be changed without restarting the server:
//
//	poll_interval_seconds = 60
//
//	[[trigger]]
//	routine = "ClubHub: OpenCode P1 – Implementieren"
//	label   = "factory:dispatch-oc-p1"
//
//	[[trigger]]
//	routine      = "ClubHub: Epic-Kette Fortschaltung"
//	kind         = "pull_request"
//	label        = "factory:epic-chain"
//	state        = "merged"
//	merged_after = "2026-08-15T00:00:00Z"

const (
	labelPollDefaultInterval = 60 * time.Second
	labelPollMinimumInterval = 10 * time.Second
	labelPollCommandTimeout  = 30 * time.Second
	labelPollMaxItems        = 100
	labelPollMaxOutputBytes  = 4 << 20

	labelKindIssue       = "issue"
	labelKindPullRequest = "pull_request"
)

type labelTriggerConfig struct {
	Routine string `toml:"routine"`
	Kind    string `toml:"kind"`
	Label   string `toml:"label"`
	State   string `toml:"state"`
	// MergedAfter bounds a merged pull-request trigger. Without it the first
	// cycle would admit Work for every pull request ever merged with the
	// label; request_key prevents repeats but not that initial backfill.
	MergedAfter string `toml:"merged_after"`
}

func (t labelTriggerConfig) kind() string {
	if strings.TrimSpace(t.Kind) == "" {
		return labelKindIssue
	}
	return strings.ToLower(strings.TrimSpace(t.Kind))
}

func (t labelTriggerConfig) state() string {
	if strings.TrimSpace(t.State) == "" {
		return "open"
	}
	return strings.ToLower(strings.TrimSpace(t.State))
}

func (t labelTriggerConfig) mergedAfter() (time.Time, error) {
	value := strings.TrimSpace(t.MergedAfter)
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("merged_after must be an RFC3339 instant: %w", err)
	}
	return parsed.UTC(), nil
}

type labelPollerConfig struct {
	PollIntervalSeconds int                  `toml:"poll_interval_seconds"`
	Triggers            []labelTriggerConfig `toml:"trigger"`
}

func (c labelPollerConfig) interval() time.Duration {
	if c.PollIntervalSeconds <= 0 {
		return labelPollDefaultInterval
	}
	interval := time.Duration(c.PollIntervalSeconds) * time.Second
	if interval < labelPollMinimumInterval {
		return labelPollMinimumInterval
	}
	return interval
}

func labelPollerConfigPath() string {
	home := os.Getenv("FACTORY_DATA_HOME")
	if home == "" {
		configured, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = filepath.Join(configured, ".factory")
	}
	return filepath.Join(home, "labels.toml")
}

func validateLabelTrigger(index int, trigger labelTriggerConfig) error {
	position := index + 1
	if strings.TrimSpace(trigger.Routine) == "" {
		return fmt.Errorf("trigger %d: routine is required", position)
	}
	if strings.TrimSpace(trigger.Label) == "" {
		return fmt.Errorf("trigger %d: label is required", position)
	}
	switch trigger.kind() {
	case labelKindIssue:
		if state := trigger.state(); state != "open" && state != "closed" {
			return fmt.Errorf("trigger %d: issue state must be open or closed", position)
		}
		if strings.TrimSpace(trigger.MergedAfter) != "" {
			return fmt.Errorf("trigger %d: merged_after applies to pull_request triggers only", position)
		}
	case labelKindPullRequest:
		state := trigger.state()
		if state != "open" && state != "closed" && state != "merged" {
			return fmt.Errorf("trigger %d: pull request state must be open, closed, or merged", position)
		}
		// Refuse rather than silently replaying years of merged pull requests.
		if state == "merged" && strings.TrimSpace(trigger.MergedAfter) == "" {
			return fmt.Errorf(
				"trigger %d: merged pull-request triggers require merged_after so existing history is not replayed",
				position)
		}
		if _, err := trigger.mergedAfter(); err != nil {
			return fmt.Errorf("trigger %d: %w", position, err)
		}
	default:
		return fmt.Errorf("trigger %d: kind must be issue or pull_request", position)
	}
	return nil
}

// loadLabelPollerConfig reports found=false when no configuration exists, which
// is the normal state for an installation that does not use label triggers.
func loadLabelPollerConfig(path string) (labelPollerConfig, bool, error) {
	var config labelPollerConfig
	if path == "" {
		return config, false, nil
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return config, false, nil
	}
	if err != nil {
		return config, false, err
	}
	if err := toml.Unmarshal(body, &config); err != nil {
		return config, false, fmt.Errorf("parse %s: %w", path, err)
	}
	for index, trigger := range config.Triggers {
		if err := validateLabelTrigger(index, trigger); err != nil {
			return config, false, err
		}
	}
	return config, true, nil
}

// RunLabelPoller admits Work for issues and pull requests carrying a configured
// label. It mirrors RunRoutineScheduler: it never returns an error, because a
// poller that stops on a transient GitHub failure would silently disable every
// trigger.
func (s *Store) RunLabelPoller(ctx context.Context, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	path := labelPollerConfigPath()
	interval := labelPollDefaultInterval
	var lastConfigError string
	for {
		config, found, err := loadLabelPollerConfig(path)
		switch {
		case err != nil:
			// Log a given configuration error once rather than every cycle.
			if message := err.Error(); message != lastConfigError {
				logger.Error("label_poller_config_invalid", "path", path, "error", message)
				lastConfigError = message
			}
		case found:
			if lastConfigError != "" {
				logger.Info("label_poller_config_recovered", "path", path)
				lastConfigError = ""
			}
			interval = config.interval()
			s.admitLabelTriggers(ctx, logger, config)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (s *Store) admitLabelTriggers(ctx context.Context, logger *slog.Logger, config labelPollerConfig) {
	for _, trigger := range config.Triggers {
		if ctx.Err() != nil {
			return
		}
		if err := s.admitLabelTrigger(ctx, logger, trigger); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			logger.Error("label_trigger_failed",
				"routine", trigger.Routine, "kind", trigger.kind(),
				"label", trigger.Label, "error", err)
		}
	}
}

func (s *Store) admitLabelTrigger(ctx context.Context, logger *slog.Logger, trigger labelTriggerConfig) error {
	routineID, snapshot, err := s.labelTriggerRoutine(ctx, trigger.Routine)
	if err != nil {
		return err
	}
	if len(snapshot.Repositories) == 0 {
		return fmt.Errorf("routine %q has no repositories", trigger.Routine)
	}
	mergedAfter, err := trigger.mergedAfter()
	if err != nil {
		return err
	}
	for _, repository := range snapshot.Repositories {
		items, err := listLabelledItems(ctx, trigger.kind(), repository.RemoteIdentity,
			trigger.Label, trigger.state(), mergedAfter)
		if err != nil {
			return err
		}
		for _, item := range items {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			admitted, err := s.admitLabelledItem(ctx, routineID, snapshot, trigger.Label, repository, item)
			if err != nil {
				logger.Error("label_admission_failed",
					"routine", trigger.Routine, "kind", item.Kind, "label", trigger.Label,
					"repository", repository.RemoteIdentity, "number", item.Number,
					"error", err)
				continue
			}
			if admitted {
				logger.Info("label_work_admitted",
					"routine", trigger.Routine, "kind", item.Kind, "label", trigger.Label,
					"repository", repository.RemoteIdentity, "number", item.Number)
			}
		}
	}
	return nil
}

// labelTriggerRoutine resolves a Routine by its operator-facing name and
// refuses states that must not start new Work.
func (s *Store) labelTriggerRoutine(ctx context.Context, name string) (string, protocol.RoutineSnapshot, error) {
	var snapshot protocol.RoutineSnapshot
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", snapshot, err
	}
	defer tx.Rollback()
	var id string
	var archived, migrationOnly, readOnly int
	err = tx.QueryRowContext(ctx, `
		SELECT id, archived, migration_only, read_only FROM routines WHERE name_key = ?
	`, normalizeTitleKey(name)).Scan(&id, &archived, &migrationOnly, &readOnly)
	if errors.Is(err, sql.ErrNoRows) {
		return "", snapshot, fmt.Errorf("routine %q was not found", name)
	}
	if err != nil {
		return "", snapshot, err
	}
	if archived == 1 || migrationOnly == 1 || readOnly == 1 {
		return "", snapshot, fmt.Errorf("routine %q is archived or read-only", name)
	}
	// Deliberately not loadCurrentRoutineSnapshot: that helper serves the
	// scheduler, which only ever loads Routines that have a schedule, so it
	// scans cron and timezone as plain strings. Label triggers target
	// unscheduled Routines, where both columns are NULL. The schedule fields
	// stay empty because label admission never resolves a schedule prompt.
	err = tx.QueryRowContext(ctx, `
		SELECT id, name, prompt, runtime, timeout_seconds, concurrency_limit, generation
		FROM routines WHERE id = ?
	`, id).Scan(&snapshot.ID, &snapshot.Name, &snapshot.Prompt, &snapshot.Runtime,
		&snapshot.TimeoutSeconds, &snapshot.ConcurrencyLimit, &snapshot.Generation)
	if err != nil {
		return "", snapshot, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT repository.id, repository.remote_identity
		FROM routine_repositories selected
		JOIN repositories repository ON repository.id = selected.repository_id
		WHERE selected.routine_id = ? ORDER BY selected.position
	`, id)
	if err != nil {
		return "", snapshot, err
	}
	for rows.Next() {
		var repository protocol.RoutineRepository
		if err := rows.Scan(&repository.ID, &repository.RemoteIdentity); err != nil {
			rows.Close()
			return "", snapshot, err
		}
		snapshot.Repositories = append(snapshot.Repositories, repository)
	}
	if err := rows.Close(); err != nil {
		return "", snapshot, err
	}
	if err := tx.Commit(); err != nil {
		return "", snapshot, err
	}
	return id, snapshot, nil
}

// admitLabelledItem reports whether new Work was created. A repeated call for
// an item that was already admitted returns false without error, because the
// UNIQUE request_key makes admitRoutine idempotent.
func (s *Store) admitLabelledItem(
	ctx context.Context,
	routineID string,
	snapshot protocol.RoutineSnapshot,
	label string,
	repository protocol.RoutineRepository,
	item labelledItem,
) (bool, error) {
	frozen := snapshot
	frozen.Prompt = resolveLabelPrompt(snapshot.Prompt, label, repository.RemoteIdentity, item)
	// Issues and pull requests share a number space per repository, so the kind
	// belongs in the key.
	requestKey := fmt.Sprintf("label:%s:%s:%s:%d", routineID, label, item.Kind, item.Number)
	if len(requestKey) > 200 {
		return false, fmt.Errorf("request key for %s #%d exceeds 200 bytes", item.Kind, item.Number)
	}
	_, created, err := s.admitRoutine(ctx, routineID, "manual", requestKey, nil, &frozen)
	if err != nil {
		return false, err
	}
	return created, nil
}

// resolveLabelPrompt mirrors protocol.ResolveRoutineSchedulePrompt: the trusted
// occurrence is appended as JSON so the agent can tell operator prompt text
// apart from Factory-supplied context.
func resolveLabelPrompt(prompt, label, repository string, item labelledItem) string {
	occurrence, err := json.Marshal(struct {
		Type       string   `json:"type"`
		Kind       string   `json:"kind"`
		Label      string   `json:"label"`
		Repository string   `json:"repository"`
		Number     int      `json:"number"`
		Title      string   `json:"title"`
		URL        string   `json:"url"`
		State      string   `json:"state"`
		Labels     []string `json:"labels"`
		BaseBranch string   `json:"base_branch,omitempty"`
		MergedAt   string   `json:"merged_at,omitempty"`
	}{"label", item.Kind, label, repository, item.Number, item.Title, item.URL,
		item.State, item.Labels, item.BaseBranch, item.MergedAt})
	if err != nil {
		// Marshalling these fields cannot fail; degrade to the bare prompt
		// rather than dropping the occurrence.
		return prompt
	}
	subject := "GitHub issue"
	if item.Kind == labelKindPullRequest {
		subject = "GitHub pull request"
	}
	return prompt +
		"\n\nLabel instruction:\n\n" +
		"Execute this Routine for the " + subject + " below. Revalidate it before acting on it." +
		"\n\nTrusted label occurrence:\n\n" + string(occurrence)
}

type labelledItem struct {
	Kind       string
	Number     int
	Title      string
	URL        string
	State      string
	Labels     []string
	BaseBranch string
	MergedAt   string
}

func listLabelledItems(
	ctx context.Context,
	kind, repository, label, state string,
	mergedAfter time.Time,
) ([]labelledItem, error) {
	project := strings.TrimPrefix(repository, "github.com/")
	var arguments []string
	switch kind {
	case labelKindPullRequest:
		arguments = []string{
			"pr", "list", "--repo", project, "--state", state,
			"--label", label, "--limit", strconv.Itoa(labelPollMaxItems),
			"--json", "number,title,url,state,labels,baseRefName,mergedAt",
		}
	default:
		arguments = []string{
			"issue", "list", "--repo", project, "--state", state,
			"--label", label, "--limit", strconv.Itoa(labelPollMaxItems),
			"--json", "number,title,url,state,labels",
		}
	}
	stdout, err := runLabelPollCommand(ctx, project, arguments)
	if err != nil {
		return nil, err
	}
	var values []struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		URL         string `json:"url"`
		State       string `json:"state"`
		BaseRefName string `json:"baseRefName"`
		MergedAt    string `json:"mergedAt"`
		Labels      []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(stdout, &values); err != nil {
		return nil, fmt.Errorf("gh returned unexpected JSON for %s: %w", project, err)
	}
	items := make([]labelledItem, 0, len(values))
	for _, value := range values {
		if value.Number <= 0 {
			return nil, fmt.Errorf("gh returned an invalid number for %s", project)
		}
		if kind == labelKindPullRequest && !mergedAfter.IsZero() {
			include, err := mergedAfterBound(value.MergedAt, mergedAfter)
			if err != nil {
				return nil, fmt.Errorf("gh returned an unreadable mergedAt for %s #%d: %w",
					project, value.Number, err)
			}
			if !include {
				continue
			}
		}
		labels := make([]string, 0, len(value.Labels))
		for _, entry := range value.Labels {
			labels = append(labels, entry.Name)
		}
		items = append(items, labelledItem{
			Kind:       normalizeLabelKind(kind),
			Number:     value.Number,
			Title:      strings.TrimSpace(value.Title),
			URL:        strings.TrimSpace(value.URL),
			State:      strings.ToLower(strings.TrimSpace(value.State)),
			Labels:     labels,
			BaseBranch: strings.TrimSpace(value.BaseRefName),
			MergedAt:   strings.TrimSpace(value.MergedAt),
		})
	}
	return items, nil
}

func normalizeLabelKind(kind string) string {
	if kind == labelKindPullRequest {
		return labelKindPullRequest
	}
	return labelKindIssue
}

// mergedAfterBound reports whether a pull request merged at the reported
// instant falls inside the configured bound. An unmerged entry is excluded
// rather than treated as merged at the zero time.
func mergedAfterBound(mergedAt string, bound time.Time) (bool, error) {
	value := strings.TrimSpace(mergedAt)
	if value == "" {
		return false, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return false, err
	}
	return !parsed.UTC().Before(bound), nil
}

func runLabelPollCommand(ctx context.Context, project string, arguments []string) ([]byte, error) {
	commandContext, cancel := context.WithTimeout(ctx, labelPollCommandTimeout)
	defer cancel()
	command := exec.CommandContext(commandContext, "gh", arguments...)
	stdout, err := command.Output()
	if err != nil {
		var exitError *exec.ExitError
		switch {
		case errors.Is(err, exec.ErrNotFound):
			return nil, errors.New("GitHub CLI (gh) was not found on PATH")
		case errors.Is(commandContext.Err(), context.DeadlineExceeded):
			return nil, fmt.Errorf("gh %s for %s timed out", arguments[0], project)
		case errors.Is(ctx.Err(), context.Canceled):
			return nil, ctx.Err()
		case errors.As(err, &exitError):
			message := strings.TrimSpace(string(exitError.Stderr))
			if message == "" {
				message = err.Error()
			}
			return nil, fmt.Errorf("gh %s for %s failed: %s", arguments[0], project, message)
		default:
			return nil, fmt.Errorf("gh %s for %s failed: %w", arguments[0], project, err)
		}
	}
	if len(stdout) > labelPollMaxOutputBytes {
		return nil, fmt.Errorf("gh returned more than %d bytes for %s", labelPollMaxOutputBytes, project)
	}
	return stdout, nil
}
