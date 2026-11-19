/**
 * Copyright (c) 2023-present Plane Software, Inc. and contributors
 * SPDX-License-Identifier: AGPL-3.0-only
 * See the LICENSE file for details.
 */
// Test doubles and sequential polling intentionally use these patterns.
/* eslint-disable no-await-in-loop */
// npm install --prefix /tmp/plane-browser playwright; NODE_PATH=/tmp/plane-browser/node_modules node this-file
// Prepare live-fixture.py with the API environment first. Use an isolated deployment.
const { chromium } = require("playwright");
const fs = require("node:fs"),
  os = require("node:os"),
  path = require("node:path");
const { execFileSync } = require("node:child_process");
const assert = require("node:assert/strict");
const fixture = JSON.parse(fs.readFileSync(process.env.PLANE_LIVE_FIXTURE));
const server = process.env.PLANE_E2E_SERVER || "http://127.0.0.1:58080";
const profile = fs.mkdtempSync(path.join(os.tmpdir(), "plane-live-browser-"));
const cli = (...args) =>
  JSON.parse(
    execFileSync(
      process.env.PLANE_CLI_BINARY || "/tmp/plane-live-e2e/plane",
      ["--config-dir", profile, "--workspace", fixture.workspace, ...args],
      { encoding: "utf8", timeout: 40000 }
    ) || "null"
  );
const key = path.join(profile, "key");
fs.writeFileSync(key, fixture.token, { mode: 0o600 });
const image = path.join(profile, "proof.png");
fs.writeFileSync(
  image,
  Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Wl6nWQAAAAASUVORK5CYII=", "base64")
);
async function eventually(fn, label) {
  const end = Date.now() + 45000;
  let error;
  while (Date.now() < end) {
    try {
      if (await fn()) return;
    } catch (e) {
      error = e;
    }
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error(label + (error ? ": " + error.message : ""));
}
(async () => {
  let browser;
  try {
    cli("auth", "login", "--server", server, "--api-key-file", key);
    const issue = cli(
      "work-items",
      "create",
      "--project",
      fixture.project,
      "--data",
      JSON.stringify({ name: "Before live update" })
    );
    browser = await chromium.launch({
      executablePath: process.env.CHROMIUM_PATH || undefined,
      headless: true,
      args: ["--no-sandbox"],
    });
    const context = await browser.newContext();
    await context.addCookies([{ name: fixture.cookie_name, value: fixture.cookie, url: server }]);
    const page = await context.newPage();
    page.on("requestfailed", (r) => console.error("Failed request", r.url(), r.failure()?.errorText));
    page.on("response", (r) => {
      if (r.status() >= 400) console.error("HTTP", r.status(), r.url());
    });
    const frames = [];
    let navigations = 0;
    page.on("framenavigated", (f) => {
      if (f === page.mainFrame()) navigations++;
    });
    page.on("websocket", (ws) => {
      if (ws.url().includes("/live/events"))
        ws.on("framereceived", (e) => {
          try {
            frames.push(JSON.parse(e.payload));
          } catch {}
        });
    });
    page.on("pageerror", (e) => console.error("Browser error:", e.message));
    await page.goto(server + "/" + fixture.workspace + "/browse/LIVE-" + issue.sequence_id, { timeout: 120000 });
    await eventually(() => page.getByText("Before live update", { exact: true }).isVisible(), "initial work item");
    await eventually(
      () => frames.some((f) => f.type === "subscribed" && f.project_ids.includes(fixture.project)),
      "websocket subscription"
    );
    const initialNavigations = navigations;
    cli(
      "work-items",
      "update",
      issue.id,
      "--project",
      fixture.project,
      "--data",
      JSON.stringify({ name: "After live update" })
    );
    await eventually(
      () => page.getByText("After live update", { exact: true }).isVisible(),
      "same-user CLI title update"
    );
    const comment = cli(
      "comments",
      "add",
      "--project",
      fixture.project,
      "--issue",
      issue.id,
      "--body",
      "<p>Live image proof</p>",
      "--image",
      image
    );
    await eventually(() => page.getByText("Live image proof", { exact: true }).isVisible(), "live comment insertion");
    await eventually(
      () =>
        page
          .locator("img")
          .evaluateAll((imgs) => imgs.some((i) => i.src.includes("/assets/") && i.complete && i.naturalWidth > 0)),
      "uploaded image rendering"
    );
    const endpoint =
      "/api/v1/workspaces/" +
      fixture.workspace +
      "/projects/" +
      fixture.project +
      "/work-items/" +
      issue.id +
      "/comments/" +
      comment.id +
      "/";
    cli("api", "PATCH", endpoint, "--data", JSON.stringify({ comment_html: "<p>Live edited proof</p>" }));
    await eventually(() => page.getByText("Live edited proof", { exact: true }).isVisible(), "live comment edit");
    await context.setOffline(true);
    cli("api", "PATCH", endpoint, "--data", JSON.stringify({ comment_html: "<p>Offline edited proof</p>" }));
    await context.setOffline(false);
    await eventually(
      () => page.getByText("Offline edited proof", { exact: true }).isVisible(),
      "reconnect comment refresh"
    );
    cli("api", "DELETE", endpoint);
    await eventually(
      async () => (await page.getByText("Offline edited proof", { exact: true }).count()) === 0,
      "live comment deletion"
    );
    assert.equal(navigations, initialNavigations, "updates must not reload or navigate the page");
    console.log("PASS: same-user CLI title, image comment, edit, short reconnect, delete; no page reload");
  } catch (e) {
    if (browser) {
      for (const context of browser.contexts())
        for (const page of context.pages()) {
          await page
            .screenshot({ path: process.env.PLANE_E2E_SCREENSHOT || "/tmp/plane-live-failure.png", fullPage: true })
            .catch(() => {});
          console.error("Page URL:", page.url());
          console.error(
            (
              await page
                .locator("body")
                .innerText()
                .catch(() => "")
            ).slice(0, 2000)
          );
        }
    }
    throw e;
  } finally {
    await browser?.close();
    try {
      cli("daemon", "stop");
    } catch {}
    fs.rmSync(profile, { recursive: true, force: true });
  }
})().catch((e) => {
  console.error(e);
  process.exitCode = 1;
});
