import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Archive, CalendarClock, Eye, GitBranch, Pencil, Play, Plus, Tag, Trash2, X } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "./api";
import { timeAgo } from "./format";
import type { ManagedRepository, Routine, RoutineTrigger, Runtime, SaveRoutineInput, TriggerKind, TriggerState } from "./types";
import { EmptyState, ErrorState, InlineError, LoadingState, StatusBadge, ViewHeader } from "./ui";

const triggerKindLabel: Record<TriggerKind, string> = {
  github_issue: "Issue",
  github_pull_request: "Pull request",
};

/** Renders an instant for datetime-local, which expects local wall time. */
function toLocalInput(value?: string): string {
  if (!value) return "";
  const instant = new Date(value);
  if (Number.isNaN(instant.getTime())) return "";
  const pad = (part: number) => String(part).padStart(2, "0");
  return `${instant.getFullYear()}-${pad(instant.getMonth() + 1)}-${pad(instant.getDate())}T${pad(instant.getHours())}:${pad(instant.getMinutes())}`;
}

function fromLocalInput(value: string): string | undefined {
  if (!value) return undefined;
  const instant = new Date(value);
  return Number.isNaN(instant.getTime()) ? undefined : instant.toISOString();
}

/** Describes what starts this Routine without opening its editor. */
function automationSummary(routine: Routine): string {
  const parts: string[] = [];
  if (routine.schedule.enabled) parts.push(`${routine.schedule.cron} · ${routine.schedule.timezone}`);
  for (const trigger of routine.triggers ?? []) {
    parts.push(`${triggerKindLabel[trigger.kind]} ${trigger.state} · ${trigger.label}`);
  }
  return parts.length ? parts.join(" — ") : "Manual only";
}

export function RoutinesView({ initialID, createOpen, onWork }: { initialID?: string; createOpen?: boolean; onWork: (id: string) => void }) {
  const client = useQueryClient();
  const [editing, setEditing] = useState<Routine | "new" | null>(createOpen ? "new" : null);
  const [showArchived, setShowArchived] = useState(false);
  const runRequests = useRef(new Map<string, { generation: number; requestKey: string }>());
  const query = useQuery({ queryKey: ["routines", showArchived], queryFn: () => api.routines(showArchived) });
  const run = useMutation({
    mutationFn: ({ id, requestKey }: { id: string; generation: number; requestKey: string }) => api.runRoutine(id, requestKey),
    onSuccess: (detail, request) => {
      runRequests.current.delete(request.id);
      void client.invalidateQueries({ queryKey: ["overview"] });
      void client.invalidateQueries({ queryKey: ["work"] });
      onWork(detail.work.id);
    },
  });
  const archive = useMutation({
    mutationFn: (routine: Routine) => api.archiveRoutine(routine.id, !routine.archived, routine.generation),
    onSuccess: () => {
      setEditing(null);
      void client.invalidateQueries({ queryKey: ["routines"] });
      void client.invalidateQueries({ queryKey: ["overview"] });
    },
  });
  const setEnabled = useMutation({
    mutationFn: (routine: Routine) => api.setRoutineEnabled(routine.id, !routine.enabled, routine.generation),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ["routines"] });
      void client.invalidateQueries({ queryKey: ["overview"] });
    },
  });
  const resetArchive = archive.reset;
  const openRoutine = useCallback((id: string) => {
    resetArchive();
    void api.routine(id).then(setEditing);
  }, [resetArchive]);
  useEffect(() => {
    if (initialID) openRoutine(initialID);
  }, [initialID, openRoutine]);
  if (query.isPending) return <LoadingState label="Loading Routines" />;
  if (query.isError) return <ErrorState error={query.error} onRetry={() => void query.refetch()} />;
  return <div className="page">
    <ViewHeader title="Routines" fetching={query.isFetching} updatedAt={query.dataUpdatedAt} onRefresh={() => void query.refetch()} />
    <div className="view-toolbar routine-toolbar">
      <p>A prompt you can run now or schedule across repositories.</p>
      <div className="work-toolbar-actions">
        <button className={`quiet-toggle ${showArchived ? "active" : ""}`} onClick={() => setShowArchived((value) => !value)}><Archive size={14} /> Archived</button>
        <button className="button button-primary" onClick={() => { resetArchive(); setEditing("new"); }}><Plus size={15} /> New Routine</button>
      </div>
    </div>
    <InlineError error={run.error ?? setEnabled.error} />
    {!query.data?.length ? <EmptyState icon={<CalendarClock size={22} />} title="No Routines yet" description="Create one prompt, choose its repositories, then run it now or on a schedule." action={<button className="button button-primary" onClick={() => setEditing("new")}><Plus size={15} /> New Routine</button>} /> :
      <div className="routine-list panel">
        {query.data.map((routine) => <article className="routine-row" key={routine.id}>
          <button className="routine-copy" onClick={() => openRoutine(routine.id)}>
            <span className="routine-title-line"><strong>{routine.name}</strong>{routine.archived && <span className="subtle-pill">Archived</span>}{routine.read_only && <span className="subtle-pill">Read-only</span>}</span>
            <span>{routine.prompt_preview}</span>
            <small><GitBranch size={12} /> {routine.repository_count} repos · {routine.runtime} · up to {routine.concurrency_limit} at once</small>
          </button>
          <div className="routine-schedule">
            <label className="routine-power">
              <input
                type="checkbox"
                role="switch"
                aria-label={`Activate ${routine.name}`}
                checked={routine.enabled}
                disabled={routine.read_only || routine.archived || setEnabled.isPending}
                onChange={() => setEnabled.mutate(routine)}
              />
              <span>{routine.enabled ? "Active" : "Paused"}</span>
              {routine.schedule.enabled && routine.enabled && routine.schedule.health_status !== "healthy" &&
                <StatusBadge state={routine.schedule.health_status} />}
            </label>
            <small title={automationSummary(routine)}>{automationSummary(routine)}</small>
          </div>
          <div className="routine-last"><span>{routine.last_work_state ? <StatusBadge state={routine.last_work_state} /> : "No Work yet"}</span><small>Edited {timeAgo(routine.updated_at)}</small></div>
          <div className="routine-actions">
            <button className="icon-button" aria-label={`${routine.read_only ? "View" : "Edit"} ${routine.name}`} onClick={() => openRoutine(routine.id)}>{routine.read_only ? <Eye size={15} /> : <Pencil size={15} />}</button>
            <button className="button button-secondary" title={routine.repository_count === 0 ? "Add a repository before running" : undefined} disabled={routine.read_only || routine.archived || routine.repository_count === 0 || run.isPending} onClick={() => {
              const previous = runRequests.current.get(routine.id);
              const requestKey = previous?.generation === routine.generation ? previous.requestKey : crypto.randomUUID();
              runRequests.current.set(routine.id, { generation: routine.generation, requestKey });
              run.mutate({ id: routine.id, generation: routine.generation, requestKey });
            }}><Play size={14} /> Run now</button>
          </div>
        </article>)}
      </div>}
    {editing && <RoutineComposer routine={editing === "new" ? undefined : editing} onClose={() => setEditing(null)} onSaved={() => { setEditing(null); void client.invalidateQueries({ queryKey: ["routines"] }); void client.invalidateQueries({ queryKey: ["overview"] }); }} onArchive={(routine) => archive.mutate(routine)} archiveError={archive.error} archivePending={archive.isPending} />}
  </div>;
}

function RoutineComposer({ routine, onClose, onSaved, onArchive, archiveError, archivePending }: { routine?: Routine; onClose: () => void; onSaved: () => void; onArchive: (routine: Routine) => void; archiveError: Error | null; archivePending: boolean }) {
  const repositories = useQuery({ queryKey: ["repositories"], queryFn: api.repositories });
  const readOnly = routine?.read_only ?? false;
  const [name, setName] = useState(routine?.name ?? "");
  const [prompt, setPrompt] = useState(routine?.prompt ?? "");
  const [runtime, setRuntime] = useState<Runtime>(routine?.runtime ?? "codex");
  const [timeout, setTimeoutValue] = useState(routine?.timeout_seconds ?? 7200);
  const [concurrency, setConcurrency] = useState(routine?.concurrency_limit ?? 10);
  const [selected, setSelected] = useState<string[]>(routine?.repositories?.map((repository) => repository.id) ?? []);
  const [enabled, setEnabled] = useState(routine ? routine.enabled : true);
  const [scheduled, setScheduled] = useState(routine?.schedule.enabled ?? false);
  const [cron, setCron] = useState(routine?.schedule.cron ?? "0 9 * * 1");
  const [timezone, setTimezone] = useState(routine?.schedule.timezone ?? Intl.DateTimeFormat().resolvedOptions().timeZone);
  const [triggers, setTriggers] = useState<RoutineTrigger[]>(routine?.triggers ?? []);
  const save = useMutation({
    mutationFn: () => {
      const input: SaveRoutineInput = {
        name, prompt, runtime, enabled: routine?.archived ? false : enabled, timeout_seconds: timeout,
        concurrency_limit: concurrency, repository_ids: selected,
        schedule: { enabled: scheduled, cron: scheduled ? cron : undefined, timezone: scheduled ? timezone : undefined },
        triggers: triggers.map((trigger) => ({
          ...trigger,
          merged_after: trigger.state === "merged" ? trigger.merged_after : undefined,
        })),
        expected_generation: routine?.generation,
      };
      return routine ? api.updateRoutine(routine.id, input) : api.createRoutine(input);
    },
    onSuccess: onSaved,
  });
  const updateTrigger = (index: number, patch: Partial<RoutineTrigger>) =>
    setTriggers((current) => current.map((trigger, position) => position === index ? { ...trigger, ...patch } : trigger));
  // A merged trigger needs a bound, so adding the state supplies "from now".
  const changeTriggerState = (index: number, state: TriggerState) =>
    updateTrigger(index, {
      state,
      merged_after: state === "merged"
        ? (triggers[index].merged_after ?? new Date().toISOString())
        : undefined,
    });
  const triggersIncomplete = triggers.some((trigger) =>
    !trigger.label.trim() || (trigger.state === "merged" && !trigger.merged_after));
  const discard = useMutation({
    mutationFn: () => api.discardRoutineOccurrence(routine!.id, routine!.schedule.pending_due_at!),
    onSuccess: onSaved,
  });
  const toggleRepository = (id: string) => setSelected((current) => current.includes(id) ? current.filter((value) => value !== id) : [...current, id]);
  return <div className="modal-layer" role="presentation">
    <button className="modal-scrim" aria-label="Close Routine editor" onClick={onClose} />
    <section className="modal routine-modal" role="dialog" aria-modal="true" aria-labelledby="routine-composer-title">
      <header className="modal-header"><div><h2 id="routine-composer-title">{readOnly ? "Routine revision" : routine ? "Edit Routine" : "New Routine"}</h2><p>{readOnly ? "Historical revision preserved as read-only." : "One prompt, one repository set, optional schedule."}</p></div><button className="icon-button" aria-label="Close" onClick={onClose}><X size={17} /></button></header>
      <div className="modal-body routine-form">
        <label className="field"><span>Name</span><input autoFocus={!readOnly} disabled={readOnly} value={name} onChange={(event) => setName(event.target.value)} placeholder="Weekly bug scan" /></label>
        <label className="field"><span>Prompt</span><textarea className="routine-prompt" disabled={readOnly} value={prompt} onChange={(event) => setPrompt(event.target.value)} placeholder="Review the repository and fix..." /></label>
        <div className="routine-settings">
          <div className="field"><span>Runtime</span><div className="choice-control">{[["codex", "Codex"], ["claude-code", "Claude"], ["pi", "Pi"]].map(([value, label]) => <button type="button" key={value} disabled={readOnly} aria-pressed={runtime === value} onClick={() => setRuntime(value as Runtime)}>{label}</button>)}</div></div>
          <div className="field"><span>Timeout</span><div className="choice-control timeout-control">{[[1800, "30m"], [3600, "1h"], [7200, "2h"], [14400, "4h"], [28800, "8h"]].map(([value, label]) => <button type="button" key={value} disabled={readOnly} aria-pressed={timeout === value} onClick={() => setTimeoutValue(Number(value))}>{label}</button>)}</div></div>
          <label className="field"><span>Parallel targets</span><input type="number" min={1} max={100} disabled={readOnly} value={concurrency} onChange={(event) => setConcurrency(Number(event.target.value))} /></label>
        </div>
        <div className="field"><span>Repositories</span><div className="repository-picker">{(repositories.data ?? []).map((repository: ManagedRepository) => <button type="button" key={repository.id} disabled={readOnly} className={selected.includes(repository.id) ? "selected" : ""} onClick={() => toggleRepository(repository.id)}><span className="check-mark">{selected.includes(repository.id) ? "✓" : ""}</span><GitBranch size={14} />{repository.remote_identity}</button>)}</div></div>
        <div className="schedule-card">
          <label className="switch-line">
            <span><strong>Active</strong><small>A paused Routine runs no schedule and no trigger. Run now stays available.</small></span>
            <input type="checkbox" role="switch" disabled={readOnly || routine?.archived} checked={routine?.archived ? false : enabled} onChange={(event) => setEnabled(event.target.checked)} />
          </label>
        </div>
        <div className="schedule-card">
          <label className="switch-line"><span><strong>Schedule</strong><small>Run automatically using a five-field cron schedule.</small></span><input type="checkbox" disabled={readOnly} checked={scheduled} onChange={(event) => setScheduled(event.target.checked)} /></label>
          {scheduled && <div className="routine-settings schedule-fields"><label className="field"><span>Cron</span><input className="mono" disabled={readOnly} value={cron} onChange={(event) => setCron(event.target.value)} /></label><label className="field"><span>Timezone</span><input disabled={readOnly} value={timezone} onChange={(event) => setTimezone(event.target.value)} /></label></div>}
          {routine?.schedule.pending_due_at && <div className="pending-occurrence"><span><strong>{routine.schedule.health_status === "disabled" ? "Occurrence paused" : "Occurrence blocked"}</strong><small>{routine.schedule.health_message}</small></span>{!readOnly && (routine.schedule.health_status === "blocked" || routine.schedule.health_status === "disabled") && <button className="button button-danger-secondary" disabled={discard.isPending} onClick={() => discard.mutate()}>{discard.isPending ? "Discarding…" : "Discard occurrence"}</button>}</div>}
        </div>
        <div className="schedule-card">
          <div className="switch-line">
            <span><strong>GitHub label triggers</strong><small>Start Work for every issue or pull request in this Routine’s repositories that carries a label.</small></span>
            {!readOnly && <button type="button" className="button button-secondary" disabled={triggers.length >= 10} onClick={() => setTriggers((current) => [...current, {
              kind: "github_issue", label: "", state: "open", poll_interval_seconds: 60,
            }])}><Plus size={14} /> Add trigger</button>}
          </div>
          {!triggers.length ? <p className="quiet-empty">No trigger. This Routine starts only when you run it or when its schedule is due.</p> :
            <div className="trigger-list">
              {triggers.map((trigger, index) => <div className="trigger-row" key={index}>
                <div className="field">
                  <span>Event</span>
                  <div className="choice-control trigger-kind-control">
                    {(["github_issue", "github_pull_request"] as TriggerKind[]).map((kind) => <button
                      type="button" key={kind} disabled={readOnly} aria-pressed={trigger.kind === kind}
                      onClick={() => updateTrigger(index, {
                        kind,
                        // Only a pull request can be merged, so the state falls back.
                        ...(kind === "github_issue" && trigger.state === "merged" ? { state: "open" as TriggerState, merged_after: undefined } : {}),
                      })}
                    >{triggerKindLabel[kind]}</button>)}
                  </div>
                </div>
                <label className="field">
                  <span>Label</span>
                  <input className="mono" disabled={readOnly} value={trigger.label} placeholder="factory:dispatch"
                    onChange={(event) => updateTrigger(index, { label: event.target.value })} />
                </label>
                <label className="field">
                  <span>State</span>
                  <select disabled={readOnly} value={trigger.state} onChange={(event) => changeTriggerState(index, event.target.value as TriggerState)}>
                    <option value="open">Open</option>
                    <option value="closed">Closed</option>
                    {trigger.kind === "github_pull_request" && <option value="merged">Merged</option>}
                  </select>
                </label>
                <label className="field">
                  <span>Poll every (s)</span>
                  <input type="number" min={10} max={86400} step={10} disabled={readOnly}
                    value={trigger.poll_interval_seconds}
                    onChange={(event) => updateTrigger(index, { poll_interval_seconds: Number(event.target.value) })} />
                </label>
                {!readOnly && <button type="button" className="icon-button trigger-remove" aria-label={`Remove trigger ${index + 1}`}
                  onClick={() => setTriggers((current) => current.filter((_, position) => position !== index))}><Trash2 size={15} /></button>}
                {trigger.state === "merged" && <label className="field trigger-bound">
                  <span>Only merged after</span>
                  <input type="datetime-local" disabled={readOnly} value={toLocalInput(trigger.merged_after)}
                    onChange={(event) => updateTrigger(index, { merged_after: fromLocalInput(event.target.value) })} />
                  <small>Without a bound the first poll would admit Work for every pull request ever merged with this label.</small>
                </label>}
              </div>)}
            </div>}
          {triggers.length > 0 && !selected.length && <p className="quiet-empty"><Tag size={12} /> Select at least one repository — a trigger polls the Routine’s repositories.</p>}
        </div>
        <InlineError error={save.error ?? archiveError ?? discard.error ?? repositories.error} />
      </div>
      <footer className="modal-footer">{routine && !readOnly && <button className="button button-danger-secondary" disabled={archivePending} onClick={() => onArchive(routine)}><Archive size={14} /> {archivePending ? (routine.archived ? "Restoring…" : "Archiving…") : (routine.archived ? "Restore" : "Archive")}</button>}<span /><button className="button button-secondary" onClick={onClose}>{readOnly ? "Close" : "Cancel"}</button>{!readOnly && <button className="button button-primary" disabled={save.isPending || !name.trim() || !prompt.trim() || ((scheduled || triggers.length > 0) && selected.length === 0) || triggersIncomplete} onClick={() => save.mutate()}>{save.isPending ? "Saving…" : "Save Routine"}</button>}</footer>
    </section>
  </div>;
}
