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
// it reuses admitRoutine, whose request_key column is UNIQUE, so an issue that
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
//	state   = "open"

const (
	labelPollDefaultInterval = 60 * time.Second
	labelPollMinimumInterval = 10 * time.Second
	labelPollCommandTimeout  = 30 * time.Second
	labelPollMaxIssues       = 100
	labelPollMaxOutputBytes  = 4 << 20
)

type labelTriggerConfig struct {
	Routine string `toml:"routine"`
	Label   string `toml:"label"`
	State   string `toml:"state"`
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
		if strings.TrimSpace(trigger.Routine) == "" {
			return config, false, fmt.Errorf("trigger %d: routine is required", index+1)
		}
		if strings.TrimSpace(trigger.Label) == "" {
			return config, false, fmt.Errorf("trigger %d: label is required", index+1)
		}
		state := strings.ToLower(strings.TrimSpace(trigger.State))
		if state != "" && state != "open" && state != "closed" {
			return config, false, fmt.Errorf("trigger %d: state must be open or closed", index+1)
		}
	}
	return config, true, nil
}

// RunLabelPoller admits Work for issues carrying a configured label. It mirrors
// RunRoutineScheduler: it never returns an error, because a poller that stops
// on a transient GitHub failure would silently disable every trigger.
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
				"routine", trigger.Routine, "label", trigger.Label, "error", err)
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
	state := strings.ToLower(strings.TrimSpace(trigger.State))
	if state == "" {
		state = "open"
	}
	for _, repository := range snapshot.Repositories {
		issues, err := listLabelledIssues(ctx, repository.RemoteIdentity, trigger.Label, state)
		if err != nil {
			return err
		}
		for _, issue := range issues {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			admitted, err := s.admitLabelledIssue(ctx, routineID, snapshot, trigger.Label, repository, issue)
			if err != nil {
				logger.Error("label_admission_failed",
					"routine", trigger.Routine, "label", trigger.Label,
					"repository", repository.RemoteIdentity, "issue", issue.Number,
					"error", err)
				continue
			}
			if admitted {
				logger.Info("label_work_admitted",
					"routine", trigger.Routine, "label", trigger.Label,
					"repository", repository.RemoteIdentity, "issue", issue.Number)
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

// admitLabelledIssue reports whether new Work was created. A repeated call for
// an issue that was already admitted returns false without error, because the
// UNIQUE request_key makes admitRoutine idempotent.
func (s *Store) admitLabelledIssue(
	ctx context.Context,
	routineID string,
	snapshot protocol.RoutineSnapshot,
	label string,
	repository protocol.RoutineRepository,
	issue labelledIssue,
) (bool, error) {
	frozen := snapshot
	frozen.Prompt = resolveLabelPrompt(snapshot.Prompt, label, repository.RemoteIdentity, issue)
	requestKey := fmt.Sprintf("label:%s:%s:issue:%d", routineID, label, issue.Number)
	if len(requestKey) > 200 {
		return false, fmt.Errorf("request key for issue #%d exceeds 200 bytes", issue.Number)
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
func resolveLabelPrompt(prompt, label, repository string, issue labelledIssue) string {
	occurrence, err := json.Marshal(struct {
		Type       string   `json:"type"`
		Label      string   `json:"label"`
		Repository string   `json:"repository"`
		Number     int      `json:"number"`
		Title      string   `json:"title"`
		URL        string   `json:"url"`
		State      string   `json:"state"`
		Labels     []string `json:"labels"`
	}{"label", label, repository, issue.Number, issue.Title, issue.URL, issue.State, issue.Labels})
	if err != nil {
		// Marshalling these fields cannot fail; degrade to the bare prompt
		// rather than dropping the occurrence.
		return prompt
	}
	return prompt +
		"\n\nLabel instruction:\n\n" +
		"Execute this Routine for the GitHub issue below. Revalidate the issue before acting on it." +
		"\n\nTrusted label occurrence:\n\n" + string(occurrence)
}

type labelledIssue struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	URL    string   `json:"url"`
	State  string   `json:"state"`
	Labels []string `json:"-"`
}

func listLabelledIssues(ctx context.Context, repository, label, state string) ([]labelledIssue, error) {
	project := strings.TrimPrefix(repository, "github.com/")
	arguments := []string{
		"issue", "list", "--repo", project, "--state", state,
		"--label", label, "--limit", strconv.Itoa(labelPollMaxIssues),
		"--json", "number,title,url,labels,state",
	}
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
			return nil, fmt.Errorf("gh issue list for %s timed out", project)
		case errors.Is(ctx.Err(), context.Canceled):
			return nil, ctx.Err()
		case errors.As(err, &exitError):
			message := strings.TrimSpace(string(exitError.Stderr))
			if message == "" {
				message = err.Error()
			}
			return nil, fmt.Errorf("gh issue list for %s failed: %s", project, message)
		default:
			return nil, fmt.Errorf("gh issue list for %s failed: %w", project, err)
		}
	}
	if len(stdout) > labelPollMaxOutputBytes {
		return nil, fmt.Errorf("gh returned more than %d bytes for %s", labelPollMaxOutputBytes, project)
	}
	var values []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		URL    string `json:"url"`
		State  string `json:"state"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(stdout, &values); err != nil {
		return nil, fmt.Errorf("gh returned unexpected JSON for %s: %w", project, err)
	}
	issues := make([]labelledIssue, 0, len(values))
	for _, value := range values {
		if value.Number <= 0 {
			return nil, fmt.Errorf("gh returned an invalid issue number for %s", project)
		}
		labels := make([]string, 0, len(value.Labels))
		for _, entry := range value.Labels {
			labels = append(labels, entry.Name)
		}
		issues = append(issues, labelledIssue{
			Number: value.Number,
			Title:  strings.TrimSpace(value.Title),
			URL:    strings.TrimSpace(value.URL),
			State:  strings.ToLower(strings.TrimSpace(value.State)),
			Labels: labels,
		})
	}
	return issues, nil
}
