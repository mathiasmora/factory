import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { api } from "./api";
import { RoutinesView } from "./Routines";
import type { Routine } from "./types";

const routine: Routine = {
  id: "routine-1",
  name: "Ship ready work",
  prompt: "Find ready work.",
  prompt_preview: "Find ready work.",
  runtime: "codex",
  timeout_seconds: 7200,
  concurrency_limit: 10,
  generation: 2,
  enabled: true,
  archived: false,
  read_only: false,
  repositories: [],
  repository_count: 0,
  schedule: { enabled: false, health_status: "disabled" },
  triggers: [],
  created_at: "2026-08-11T12:00:00Z",
  updated_at: "2026-08-11T12:00:00Z",
};

describe("RoutinesView", () => {
  it("reuses the Run request key after an ambiguous failure", async () => {
    const runnable = { ...routine, repository_count: 1 };
    vi.spyOn(api, "routines").mockResolvedValue([runnable]);
    const runRoutine = vi.spyOn(api, "runRoutine").mockRejectedValue(new Error("The response was lost."));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><RoutinesView onWork={() => undefined} /></QueryClientProvider>);

    await userEvent.click(await screen.findByRole("button", { name: "Run now" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("The response was lost.");
    await userEvent.click(screen.getByRole("button", { name: "Run now" }));
    expect(runRoutine).toHaveBeenCalledTimes(2);
    expect(runRoutine.mock.calls[1][1]).toBe(runRoutine.mock.calls[0][1]);
  });

  it("uses a new Run request key after the Routine generation changes", async () => {
    const runnable = { ...routine, repository_count: 1 };
    vi.spyOn(api, "routines").mockResolvedValue([runnable]);
    const runRoutine = vi.spyOn(api, "runRoutine").mockRejectedValue(new Error("The response was lost."));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><RoutinesView onWork={() => undefined} /></QueryClientProvider>);

    await userEvent.click(await screen.findByRole("button", { name: "Run now" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("The response was lost.");
    client.setQueryData(["routines", false], [{ ...runnable, name: "Updated Routine", generation: runnable.generation + 1 }]);
    expect(await screen.findByText("Updated Routine")).toBeVisible();
    await userEvent.click(screen.getByRole("button", { name: "Run now" }));
    expect(runRoutine).toHaveBeenCalledTimes(2);
    expect(runRoutine.mock.calls[1][1]).not.toBe(runRoutine.mock.calls[0][1]);
  });

  it("keeps the editor open and shows archive failures", async () => {
    vi.spyOn(api, "routines").mockResolvedValue([routine]);
    vi.spyOn(api, "routine").mockResolvedValue(routine);
    vi.spyOn(api, "repositories").mockResolvedValue([]);
    vi.spyOn(api, "archiveRoutine").mockRejectedValue(new Error("Routine changed; refresh and try again."));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><RoutinesView initialID={routine.id} onWork={() => undefined} /></QueryClientProvider>);

    const dialog = await screen.findByRole("dialog", { name: "Edit Routine" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Archive" }));

    expect(await within(dialog).findByRole("alert")).toHaveTextContent("Routine changed; refresh and try again.");
    expect(screen.getByRole("dialog", { name: "Edit Routine" })).toBeVisible();
  });

  it("pauses a Routine from the list against its current generation", async () => {
    vi.spyOn(api, "routines").mockResolvedValue([routine]);
    const setRoutineEnabled = vi.spyOn(api, "setRoutineEnabled").mockResolvedValue({ ...routine, enabled: false });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><RoutinesView onWork={() => undefined} /></QueryClientProvider>);

    const toggle = await screen.findByRole("switch", { name: "Activate Ship ready work" });
    expect(toggle).toBeChecked();
    await userEvent.click(toggle);

    expect(setRoutineEnabled).toHaveBeenCalledWith(routine.id, false, routine.generation);
  });

  it("summarises the triggers that start a Routine", async () => {
    const triggered: Routine = {
      ...routine,
      triggers: [{ kind: "github_pull_request", label: "factory:epic-chain", state: "merged", poll_interval_seconds: 120, merged_after: "2026-08-15T00:00:00Z" }],
    };
    vi.spyOn(api, "routines").mockResolvedValue([triggered]);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><RoutinesView onWork={() => undefined} /></QueryClientProvider>);

    expect(await screen.findByText("Pull request merged · factory:epic-chain")).toBeVisible();
  });

  it("saves a merged pull-request trigger with its bound", async () => {
    vi.spyOn(api, "routines").mockResolvedValue([routine]);
    vi.spyOn(api, "routine").mockResolvedValue({ ...routine, repositories: [{ id: "repo-1", remote_identity: "github.com/acme/app" }], repository_count: 1 });
    vi.spyOn(api, "repositories").mockResolvedValue([
      { id: "repo-1", remote_identity: "github.com/acme/app", enabled: true, created_at: "", updated_at: "" },
    ]);
    const updateRoutine = vi.spyOn(api, "updateRoutine").mockResolvedValue(routine);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><RoutinesView initialID={routine.id} onWork={() => undefined} /></QueryClientProvider>);

    const dialog = await screen.findByRole("dialog", { name: "Edit Routine" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Add trigger" }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Pull request" }));
    await userEvent.type(within(dialog).getByLabelText("Label"), "factory:epic-chain");
    await userEvent.selectOptions(within(dialog).getByLabelText("State"), "merged");
    await userEvent.click(within(dialog).getByRole("button", { name: "Save Routine" }));

    const saved = updateRoutine.mock.calls[0][1];
    expect(saved.triggers).toHaveLength(1);
    expect(saved.triggers[0]).toMatchObject({
      kind: "github_pull_request", label: "factory:epic-chain", state: "merged", poll_interval_seconds: 60,
    });
    // Selecting merged supplies a bound so the first poll cannot replay history.
    expect(saved.triggers[0].merged_after).toBeTruthy();
  });

  it("blocks saving a trigger without a label", async () => {
    vi.spyOn(api, "routines").mockResolvedValue([routine]);
    vi.spyOn(api, "routine").mockResolvedValue({ ...routine, repositories: [{ id: "repo-1", remote_identity: "github.com/acme/app" }], repository_count: 1 });
    vi.spyOn(api, "repositories").mockResolvedValue([
      { id: "repo-1", remote_identity: "github.com/acme/app", enabled: true, created_at: "", updated_at: "" },
    ]);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><RoutinesView initialID={routine.id} onWork={() => undefined} /></QueryClientProvider>);

    const dialog = await screen.findByRole("dialog", { name: "Edit Routine" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Add trigger" }));

    expect(within(dialog).getByRole("button", { name: "Save Routine" })).toBeDisabled();
  });

  it("keeps the editor open and shows occurrence discard failures", async () => {
    const blocked: Routine = {
      ...routine,
      schedule: {
        enabled: true,
        cron: "0 9 * * *",
        timezone: "UTC",
        pending_due_at: "2026-08-11T09:00:00Z",
        health_status: "blocked",
        health_message: "Repository unavailable.",
      },
    };
    vi.spyOn(api, "routines").mockResolvedValue([blocked]);
    vi.spyOn(api, "routine").mockResolvedValue(blocked);
    vi.spyOn(api, "repositories").mockResolvedValue([]);
    vi.spyOn(api, "discardRoutineOccurrence").mockRejectedValue(new Error("The pending occurrence changed."));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><RoutinesView initialID={blocked.id} onWork={() => undefined} /></QueryClientProvider>);

    const dialog = await screen.findByRole("dialog", { name: "Edit Routine" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Discard occurrence" }));

    expect(await within(dialog).findByRole("alert")).toHaveTextContent("The pending occurrence changed.");
    expect(screen.getByRole("dialog", { name: "Edit Routine" })).toBeVisible();
  });
});
