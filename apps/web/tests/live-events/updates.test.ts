/**
 * Copyright (c) 2023-present Plane Software, Inc. and contributors
 * SPDX-License-Identifier: AGPL-3.0-only
 * See the LICENSE file for details.
 */
// Test doubles and sequential polling intentionally use these patterns.
/* eslint-disable typescript-eslint/no-extraneous-class */
import { describe, it, expect, vi, afterEach } from "vitest";
vi.mock("@plane/constants", () => ({ LIVE_BASE_PATH: "/live", LIVE_BASE_URL: "" }));
vi.mock("@/services/issue/issue.service", () => ({ IssueService: class {} }));
vi.mock("@/services/inbox", () => ({ InboxIssueService: class {} }));
vi.mock("@/store/issue/helpers/base-issues.store", () => ({ EIssueGroupedAction: { DELETE: "DELETE" } }));
import { LiveEventsClient } from "../../lib/live-events/client";
import { LiveWorkItemsApplier } from "../../lib/live-events/apply";

afterEach(() => vi.useRealTimers());

describe("live work item updates", () => {
  it("resynchronizes after a one second websocket outage", () => {
    vi.useFakeTimers();
    const client = new LiveEventsClient() as any;
    client.markDisconnected();
    vi.advanceTimersByTime(1000);
    expect(client.consumeResyncNeed()).toBe(true);
  });
  it("resynchronization invalidates cached comments for offline edits and deletions", () => {
    vi.useFakeTimers();
    const deps = {
      projectId: "p",
      getRoute: () => ({}),
      issueDetail: { peekIssue: { projectId: "p", issueId: "i" }, comment: { comments: { i: ["old"] } } },
    };
    const applier = new LiveWorkItemsApplier(deps as any) as any;
    applier.scheduleCoarse = vi.fn();
    applier.scheduleIntakeList = vi.fn();
    applier.enqueue = vi.fn();
    applier.handle({ type: "resync", project_id: "p" });
    const keys = new Set(applier.enqueue.mock.calls[0][1]);
    applier.fetchIssueDetail = vi.fn();
    applier.refreshIssueDetail("i", keys);
    expect(deps.issueDetail.comment.comments.i).toBeUndefined();
    applier.dispose();
  });
  it("does not discard changes from an API key owned by the current web user", () => {
    vi.useFakeTimers();
    const applier = new LiveWorkItemsApplier({ projectId: "p", getCurrentUserId: () => "same-user" } as any) as any;
    applier.enqueue = vi.fn();
    applier.handle({
      type: "events",
      project_id: "p",
      items: [
        {
          issue_id: "i",
          kinds: ["comment"],
          verbs: ["created"],
          actor_ids: ["same-user"],
        },
      ],
    });
    vi.advanceTimersByTime(1600);
    expect(applier.enqueue).toHaveBeenCalledWith(["i"], ["comment:created"]);
    applier.dispose();
  });
});

it("does not decrement a list when the deleted issue was never in that list", async () => {
  const removeIssueFromList = vi.fn();
  const applier = new LiveWorkItemsApplier({
    workspaceSlug: "w",
    projectId: "p",
    issueMap: { getIssueById: () => ({ id: "i" }), removeIssue: vi.fn() },
  } as any) as any;
  applier.pending = new Map([["i", new Set(["issue:deleted"])]]);
  applier.issueService = { retrieveIssues: async () => [] };
  applier.refreshIntake = async () => {};
  applier.getTargets = () => [
    {
      coarse: false,
      store: { groupedIssueIds: ["another-issue"], removeIssueFromList },
    },
  ];
  applier.refreshOpenIssues = async () => {};
  await applier.flush();
  expect(removeIssueFromList).not.toHaveBeenCalled();
  applier.dispose();
});

it("resynchronizes after the project subscription is acknowledged", () => {
  const client = new LiveEventsClient() as any;
  client.emit = vi.fn();
  client.handleFrame({ type: "subscribed", project_ids: ["p"], denied: [] });
  expect(client.emit).toHaveBeenCalledWith({ type: "resync", project_id: "p" });
});

it("refetches totals for matching work items outside the loaded page", async () => {
  const item = { id: "i", project_id: "p", state_id: "state", is_draft: false, archived_at: null };
  const store = { groupedIssueIds: ["another"], addIssueToList: vi.fn(), updateIssueList: vi.fn() };
  const applier = new LiveWorkItemsApplier({
    workspaceSlug: "w",
    projectId: "p",
    issueMap: { getIssueById: () => item, addIssue: vi.fn() },
  } as any) as any;
  applier.pending = new Map([["i", new Set(["issue:updated"])]]);
  applier.issueService = { retrieveIssues: async () => [item] };
  applier.refreshIntake = async () => {};
  applier.getTargets = () => [{ scope: { kind: "project" }, coarse: false, store, matches: () => true }];
  applier.refreshOpenIssues = async () => {};
  applier.noteUnknownMetadata = () => {};
  applier.scheduleCoarse = vi.fn();
  await applier.flush();
  expect(store.addIssueToList).not.toHaveBeenCalled();
  expect(applier.scheduleCoarse).toHaveBeenCalledWith(true);
  applier.dispose();
});
