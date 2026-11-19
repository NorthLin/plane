/**
 * Copyright (c) 2023-present Plane Software, Inc. and contributors
 * SPDX-License-Identifier: AGPL-3.0-only
 * See the LICENSE file for details.
 */
// Test doubles and sequential polling intentionally use these patterns.
/* eslint-disable typescript-eslint/no-extraneous-class, no-await-in-loop */
import { EventEmitter } from "node:events";
import { afterEach, describe, expect, it, vi } from "vitest";
vi.mock("@/services/api.service", () => ({ APIService: class {} }));
import { EventsHub } from "@/events/hub";
import { parseEventsConfig, EVENTS_CHANNEL_PREFIX } from "@/events/config";

const project = "10000000-0000-4000-8000-000000000001";
const issue = "10000000-0000-4000-8000-000000000002";
class Socket extends EventEmitter {
  readyState = 1;
  bufferedAmount = 0;
  frames: any[] = [];
  closed: number | undefined;
  send(raw: string) {
    this.frames.push(JSON.parse(raw));
  }
  ping() {}
  close(code?: number) {
    this.closed = code;
    this.readyState = 3;
    this.emit("close");
  }
  terminate() {
    this.close();
  }
}
let hub: EventsHub;
afterEach(async () => {
  await hub?.stop();
  vi.useRealTimers();
});
const flush = async () => {
  for (let i = 0; i < 20; i++) await Promise.resolve();
};
async function setup(roles: Record<string, number> = { [project]: 20 }) {
  vi.useFakeTimers();
  const sub = Object.assign(new EventEmitter(), { psubscribe: vi.fn(async () => 1), quit: vi.fn(async () => {}) });
  const auth = { currentUser: vi.fn(async () => ({ id: "user" })), projectRoles: vi.fn(async () => roles) };
  const config = {
    ...parseEventsConfig({ LIVE_EVENTS_ENABLED: "1", WEB_URL: "http://plane.test" }),
    revalidateMs: 1000,
    revalidateJitter: 0,
    rolesCacheMs: 0,
  };
  hub = new EventsHub({ config, auth, createSubscriber: () => sub, log: { warn: () => {} } });
  await hub.start();
  const connect = (origin = "http://plane.test", cookie = "session-id=test") => {
    const socket = new Socket();
    hub.handleConnection(socket, { headers: { origin, cookie } });
    return socket;
  };
  const socket = connect();
  socket.emit("message", JSON.stringify({ type: "subscribe", workspace_slug: "test", project_ids: [project] }));
  await flush();
  return { sub, auth, socket, connect };
}
describe("project event authorization and delivery", () => {
  it("delivers ids to members, rejects strangers, origins and absent sessions", async () => {
    const { sub, socket, connect } = await setup();
    expect(socket.frames).toContainEqual({ type: "subscribed", project_ids: [project], denied: [] });
    sub.emit(
      "pmessage",
      "*",
      EVENTS_CHANNEL_PREFIX + project,
      JSON.stringify({ v: 1, project_id: project, issue_ids: [issue], kind: "comment", verb: "created" })
    );
    await vi.advanceTimersByTimeAsync(300);
    expect(socket.frames.some((f) => f.type === "events" && f.items[0].issue_id === issue)).toBe(true);
    expect(connect("http://evil.test").closed).toBe(4403);
    expect(connect("http://plane.test", "").closed).toBe(4401);
  });
  it("denies guests and unknown projects", async () => {
    const { socket } = await setup({ [project]: 5 });
    expect(socket.frames).toContainEqual({ type: "subscribed", project_ids: [], denied: [project] });
  });
  it("revokes a subscription when project membership is removed", async () => {
    const { auth, socket } = await setup();
    auth.projectRoles.mockResolvedValue({});
    await vi.advanceTimersByTimeAsync(1100);
    expect(socket.frames).toContainEqual({ type: "revoked", project_ids: [project] });
  });
  it("waits for Redis subscription acknowledgement before requesting a resync", async () => {
    const { sub, socket } = await setup();
    sub.emit("ready");
    await flush();
    socket.frames = [];
    let ack!: () => void;
    sub.psubscribe.mockImplementationOnce(
      () =>
        new Promise<number>((r) => {
          ack = () => r(1);
        })
    );
    sub.emit("ready");
    await flush();
    expect(socket.frames).toEqual([]);
    ack();
    await flush();
    expect(socket.frames).toContainEqual({ type: "resync" });
  });
});
