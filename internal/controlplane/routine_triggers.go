package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/owainlewis/factory/internal/protocol"
)

// normalizeRoutineTriggers validates the label triggers submitted with a
// Routine. It rejects rather than repairs, because a silently corrected label
// would poll for items the operator never asked for.
func normalizeRoutineTriggers(triggers []protocol.RoutineTrigger) ([]protocol.RoutineTrigger, error) {
	if len(triggers) > protocol.MaxRoutineTriggers {
		return nil, invalid("too_many_routine_triggers",
			fmt.Sprintf("a Routine is limited to %d triggers", protocol.MaxRoutineTriggers))
	}
	normalized := make([]protocol.RoutineTrigger, 0, len(triggers))
	seen := make(map[string]struct{}, len(triggers))
	for _, trigger := range triggers {
		value := protocol.RoutineTrigger{
			Kind:                strings.ToLower(strings.TrimSpace(trigger.Kind)),
			Label:               strings.TrimSpace(trigger.Label),
			State:               strings.ToLower(strings.TrimSpace(trigger.State)),
			PollIntervalSeconds: trigger.PollIntervalSeconds,
			MergedAfter:         trigger.MergedAfter,
		}
		if value.Kind == "" {
			value.Kind = protocol.TriggerGitHubIssue
		}
		if value.Kind != protocol.TriggerGitHubIssue && value.Kind != protocol.TriggerGitHubPullRequest {
			return nil, invalid("invalid_routine_trigger_kind",
				"trigger kind must be github_issue or github_pull_request")
		}
		if value.Label == "" || len([]rune(value.Label)) > 200 {
			return nil, invalid("invalid_routine_trigger_label",
				"a trigger label is required and limited to 200 characters")
		}
		if value.State == "" {
			value.State = "open"
		}
		switch value.State {
		case "open", "closed":
		case "merged":
			if value.Kind != protocol.TriggerGitHubPullRequest {
				return nil, invalid("invalid_routine_trigger_state",
					"only a pull-request trigger can use the merged state")
			}
			// Refuse rather than silently replaying years of merged pull
			// requests on the first poll.
			if value.MergedAfter == nil || value.MergedAfter.IsZero() {
				return nil, invalid("routine_trigger_merged_after_required",
					"a merged pull-request trigger requires merged_after so existing history is not replayed")
			}
		default:
			return nil, invalid("invalid_routine_trigger_state",
				"trigger state must be open, closed, or merged")
		}
		if value.State != "merged" {
			value.MergedAfter = nil
		} else {
			bound := value.MergedAfter.UTC()
			value.MergedAfter = &bound
		}
		if value.PollIntervalSeconds == 0 {
			value.PollIntervalSeconds = protocol.DefaultTriggerPollSeconds
		}
		if value.PollIntervalSeconds < protocol.MinTriggerPollInterval ||
			value.PollIntervalSeconds > protocol.MaxTriggerPollInterval {
			return nil, invalid("invalid_routine_trigger_interval",
				"poll_interval_seconds must be between 10 and 86400")
		}
		key := value.Kind + "\x00" + strings.ToLower(value.Label)
		if _, exists := seen[key]; exists {
			return nil, invalid("duplicate_routine_trigger",
				"each label may be used once per trigger kind")
		}
		seen[key] = struct{}{}
		value.NextPollAt = nil
		normalized = append(normalized, value)
	}
	return normalized, nil
}

// replaceRoutineTriggers rewrites a Routine's triggers while preserving the
// poll cursor of a trigger that survived the edit. Resetting every cursor on
// save would re-poll GitHub for unrelated changes such as a renamed Routine.
func replaceRoutineTriggers(
	ctx context.Context,
	tx *sql.Tx,
	routineID string,
	triggers []protocol.RoutineTrigger,
) error {
	existing := make(map[string]sql.NullInt64)
	rows, err := tx.QueryContext(ctx, `
		SELECT kind, label, next_poll_at FROM routine_triggers WHERE routine_id = ?
	`, routineID)
	if err != nil {
		return unavailable(err)
	}
	for rows.Next() {
		var kind, label string
		var next sql.NullInt64
		if err := rows.Scan(&kind, &label, &next); err != nil {
			rows.Close()
			return unavailable(err)
		}
		existing[kind+"\x00"+strings.ToLower(label)] = next
	}
	if err := rows.Close(); err != nil {
		return unavailable(err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM routine_triggers WHERE routine_id = ?`, routineID); err != nil {
		return unavailable(err)
	}
	for position, trigger := range triggers {
		var mergedAfter any
		if trigger.MergedAfter != nil {
			mergedAfter = trigger.MergedAfter.UTC().UnixMilli()
		}
		var nextPoll any
		if previous, ok := existing[trigger.Kind+"\x00"+strings.ToLower(trigger.Label)]; ok && previous.Valid {
			nextPoll = previous.Int64
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO routine_triggers(
				routine_id, position, kind, label, item_state,
				poll_interval_seconds, merged_after, next_poll_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, routineID, position, trigger.Kind, trigger.Label, trigger.State,
			trigger.PollIntervalSeconds, mergedAfter, nextPoll)
		if err != nil {
			return unavailable(err)
		}
	}
	return nil
}

func loadRoutineTriggers(ctx context.Context, db queryer, routineID string) ([]protocol.RoutineTrigger, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT kind, label, item_state, poll_interval_seconds, merged_after, next_poll_at
		FROM routine_triggers WHERE routine_id = ? ORDER BY position
	`, routineID)
	if err != nil {
		return nil, unavailable(err)
	}
	var triggers []protocol.RoutineTrigger
	for rows.Next() {
		var trigger protocol.RoutineTrigger
		var mergedAfter, nextPoll sql.NullInt64
		if err := rows.Scan(&trigger.Kind, &trigger.Label, &trigger.State,
			&trigger.PollIntervalSeconds, &mergedAfter, &nextPoll); err != nil {
			rows.Close()
			return nil, unavailable(err)
		}
		if mergedAfter.Valid {
			value := fromMillis(mergedAfter.Int64)
			trigger.MergedAfter = &value
		}
		if nextPoll.Valid {
			value := fromMillis(nextPoll.Int64)
			trigger.NextPollAt = &value
		}
		triggers = append(triggers, trigger)
	}
	if err := rows.Close(); err != nil {
		return nil, unavailable(err)
	}
	return triggers, nil
}

// SetRoutineEnabled activates or pauses a Routine. A paused Routine starts no
// scheduled or label-triggered Work; "Run now" stays available so an operator
// can test a Routine without exposing it to its triggers.
func (s *Store) SetRoutineEnabled(
	ctx context.Context,
	id string,
	input protocol.SetRoutineEnabledRequest,
) (protocol.Routine, error) {
	if input.Enabled == nil {
		return protocol.Routine{}, invalid("routine_enabled_required", "enabled is required")
	}
	if input.ExpectedGeneration < 1 {
		return protocol.Routine{}, invalid("routine_generation_required", "expected_generation is required")
	}
	enabled := *input.Enabled
	result, err := s.db.ExecContext(ctx, `
		UPDATE routines SET enabled = ?, generation = generation + 1, updated_at = ?
		WHERE id = ? AND generation = ? AND migration_only = 0 AND read_only = 0
		  AND (? = 0 OR archived = 0)
	`, enabled, s.now().UnixMilli(), id, input.ExpectedGeneration, enabled)
	if err != nil {
		return protocol.Routine{}, unavailable(err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		var exists, readOnly, archived int
		_ = s.db.QueryRowContext(ctx, `
			SELECT COUNT(*), COALESCE(MAX(read_only), 0), COALESCE(MAX(archived), 0)
			FROM routines WHERE id = ?
		`, id).Scan(&exists, &readOnly, &archived)
		switch {
		case exists == 0:
			return protocol.Routine{}, ErrNotFound
		case readOnly != 0:
			return protocol.Routine{}, conflict("routine_read_only", "historical Routine revisions are read-only")
		case archived != 0 && enabled:
			return protocol.Routine{}, conflict("routine_archived", "restore the Routine before activating it")
		}
		return protocol.Routine{}, conflict("routine_generation_conflict", "the Routine changed; refresh and try again")
	}
	return s.Routine(ctx, id)
}

// queryer covers both *sql.DB and *sql.Tx so trigger loading works inside and
// outside a transaction.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// optionalInstant reports the zero instant as absent so a parsed-but-empty
// bound is not stored as 1970.
func optionalInstant(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}
