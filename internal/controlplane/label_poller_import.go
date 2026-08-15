package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"

	"github.com/owainlewis/factory/internal/protocol"
)

// importLabelPollerConfig moves an operator's labels.toml into routine_triggers
// exactly once. Re-importing on every start would resurrect triggers the
// operator has since deleted in the UI, so the completed import is recorded
// even when the file contributed no usable trigger.
func (s *Store) importLabelPollerConfig(ctx context.Context, logger *slog.Logger) {
	path := labelPollerConfigPath()
	if path == "" {
		return
	}
	var alreadyImported int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM routine_trigger_imports WHERE path = ?`, path).Scan(&alreadyImported); err != nil {
		logger.Error("label_import_state_unreadable", "path", path, "error", err)
		return
	}
	if alreadyImported > 0 {
		return
	}
	config, found, err := loadLabelPollerConfig(path)
	if err != nil {
		// Not recorded as imported: a corrected file is picked up on the next
		// start rather than silently discarded.
		logger.Error("label_import_config_invalid", "path", path, "error", err)
		return
	}
	if !found {
		return
	}
	imported := 0
	for _, trigger := range config.Triggers {
		if err := s.importLabelTrigger(ctx, trigger, config.intervalSeconds()); err != nil {
			logger.Error("label_import_trigger_skipped",
				"path", path, "routine", trigger.Routine, "label", trigger.Label, "error", err)
			continue
		}
		imported++
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO routine_trigger_imports(path, imported_at, trigger_count) VALUES (?, ?, ?)
	`, path, s.now().UnixMilli(), imported); err != nil {
		logger.Error("label_import_not_recorded", "path", path, "error", err)
		return
	}
	logger.Info("label_import_completed",
		"path", path, "imported", imported, "configured", len(config.Triggers))
}

func (s *Store) importLabelTrigger(ctx context.Context, trigger labelTriggerConfig, intervalSeconds int) error {
	kind := protocol.TriggerGitHubIssue
	if trigger.kind() == labelKindPullRequest {
		kind = protocol.TriggerGitHubPullRequest
	}
	mergedAfter, err := trigger.mergedAfter()
	if err != nil {
		return err
	}
	normalized, err := normalizeRoutineTriggers([]protocol.RoutineTrigger{{
		Kind:                kind,
		Label:               trigger.Label,
		State:               trigger.state(),
		PollIntervalSeconds: intervalSeconds,
		MergedAfter:         optionalInstant(mergedAfter),
	}})
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var routineID string
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM routines
		WHERE name_key = ? AND migration_only = 0 AND read_only = 0
	`, normalizeTitleKey(trigger.Routine)).Scan(&routineID)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("no Routine carries this name")
	}
	if err != nil {
		return err
	}
	var position int
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(position) + 1, 0) FROM routine_triggers WHERE routine_id = ?
	`, routineID).Scan(&position); err != nil {
		return err
	}
	value := normalized[0]
	var mergedAfterValue any
	if value.MergedAfter != nil {
		mergedAfterValue = value.MergedAfter.UnixMilli()
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO routine_triggers(
			routine_id, position, kind, label, item_state, poll_interval_seconds, merged_after
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`, routineID, position, value.Kind, value.Label, value.State,
		value.PollIntervalSeconds, mergedAfterValue); err != nil {
		return err
	}
	return tx.Commit()
}
