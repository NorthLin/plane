# Plane CLI and local event daemon

A macOS/Linux client for self-hosted Plane. The daemon opens an outbound HTTPS
SSE subscription; no inbound port, public callback URL, or tunnel is required.
Commands use the existing /api/v1/ API and emit JSON. Events are NDJSON.

## Build and log in

From this directory, with Go 1.19 or newer:

    go build -o plane .
    install -m 755 plane ~/.local/bin/plane
    plane --workspace my-workspace auth login --server https://plane.example.com
    plane daemon start
    plane daemon status
    plane callbacks add agent --event work_item.updated --exec /path/to/handle-task

Create an API key in Plane's account settings. Login prompts without echoing it.
For automation use --api-key-file PATH (- reads stdin), or PLANE_API_KEY.
The server URL is the public API origin; deployments with a reverse proxy should
use that proxy origin, not the web development server on port 3000.

Credentials use macOS Keychain / Linux Secret Service when available. Headless
Linux falls back to a mode-0600 credential file inside a mode-0700 directory.
No key is passed in child-process arguments. auth logout stops the daemon,
removes the saved key, callback registrations and local history. It does not revoke the server
API key; revoke that separately in Plane if required.

Each profile holds one server/login and a default workspace. Use --config-dir PATH or
PLANE_CONFIG_DIR for independent profiles. The default is ~/.config/plane.
The local API listens on a mode-0600 Unix socket, never a TCP port. The daemon
uses a process lock to prevent concurrent receivers for one profile.

## Workspaces

    plane workspace list
    plane workspace current
    plane workspace use team-b
    plane --workspace team-a projects list
    plane --workspace team-a callbacks add agent --event work_item.updated --exec /path/to/handler
    plane --workspace team-a callbacks runs agent

Selection priority: --workspace, PLANE_WORKSPACE, saved default. Switching validates
access and reuses the login. A workspace-scoped API key cannot access another
workspace; use a key with the required scope. workspace use saves the default;
one-command overrides do not change it.

Callbacks remain bound to the workspace where they were registered. The daemon
continues receiving for saved workspaces after switching, and discovers newly
selected workspaces without restart. daemon start/stop/install manage the whole
profile; daemon status includes each workspace's connection state. Callback
commands and event inspection use the selected workspace. Identical callback names
are allowed in different workspaces. Callback children inherit PLANE_WORKSPACE,
so nested CLI calls stay in that workspace even if the default changes.

Each workspace keeps its own SQLite inbox under ~/.config/plane/workspaces/<key>/.
Existing installations retain their original inbox.db for the original workspace.
Credentials are shared; logout clears all workspace state in the profile.

## Commands

    plane projects list
    plane projects create --data '{"name":"Agent work","identifier":"AGENT"}'
    plane work-items list --project PROJECT_UUID
    plane work-items create --project PROJECT_UUID --data @task.json
    plane work-items get ISSUE_UUID --project PROJECT_UUID
    plane work-items update ISSUE_UUID --project PROJECT_UUID --data '{"priority":"high"}'
    plane comments list --project PROJECT_UUID --issue ISSUE_UUID
    plane comments add --project PROJECT_UUID --issue ISSUE_UUID --body '<p>Ready for review</p>'
    plane attachments upload screenshot.png --project PROJECT_UUID --issue ISSUE_UUID
    plane attachments list --project PROJECT_UUID --issue ISSUE_UUID
    plane attachments download ASSET_UUID --project PROJECT_UUID --issue ISSUE_UUID --output screenshot.png
    plane api GET '/api/v1/workspaces/my-workspace/projects/?per_page=50'

Projects and work-items support list/get/create/update/delete. IDs are UUIDs
in this first version. --data accepts JSON or @file. The api command exposes the
rest of the public API, including query parameters and pagination. List commands
return the server's page; follow its pagination with plane api for larger lists.
The public generic asset API can resolve inline description image IDs:

    plane api GET /api/v1/workspaces/my-workspace/assets/ASSET_UUID/

Download the returned temporary asset_url to give the image bytes to an Agent.
Task attachments have dedicated upload/download commands. Downloads never
forward Plane API credentials to a different storage origin and never overwrite
an existing destination file.

## Local callbacks

Register a script once; the daemon invokes it automatically for matching events.
No watcher process or manual acknowledgement is needed.

    plane callbacks add agent --event work_item.updated --exec /path/to/handle-task
    plane callbacks add review --event comment.created -- python3 /path/to/review.py --mode review
    plane daemon start
    plane callbacks runs agent

Each invocation receives one event JSON on stdin, including changes, resource IDs
and local_seq. Exit 0 confirms delivery; nonzero exit or timeout retries. The
profile is in PLANE_CONFIG_DIR, so nested Plane commands use the same login.
PLANE_EVENT_ID is stable across retries; PLANE_CALLBACK_ATTEMPT starts at 1.
PLANE_SERVER, PLANE_WORKSPACE and PLANE_CALLBACK_NAME are also provided.

A minimal Python handler:

    import json, subprocess, sys
    event = json.load(sys.stdin)
    subprocess.run(["plane", "work-items", "get", event["issue_id"],
                    "--project", event["project_id"]], check=True)

Use --event repeatedly (or '\*' for all events), --project, --issue and
--ignore-self to narrow delivery. Commands run directly with literal arguments;
executable and working directory (--dir, default current directory) are saved as
absolute paths. Keep interpreter/script paths available to the background service.

    plane callbacks list
    plane callbacks disable agent
    plane callbacks enable agent
    plane callbacks retry RUN_ID
    plane callbacks remove agent

Registrations apply without restart and start after a server-history baseline
captured during registration; they require a reachable server. Disable pauses calls
and preserves the backlog; enable resumes it. Disable/remove lets an active call
finish; remove deletes the registration and its history. Re-registering the same
name starts a new rule for future events.

Defaults: 300-second timeout, 3 total attempts, 5-second retry delay. Change these
with --timeout, --max-attempts and --retry-delay. Calls execute one at a time per
workspace; delayed retries may be overtaken by newer calls. Runs retain state,
attempt count, last error and the latest combined output (up to 64 KiB). Rules and
runs are stored in the workspace inbox.db. Stop cancels the active process group and leaves the
call pending for restart. A crash after an external effect can cause redelivery:
make handlers idempotent using the event ID. Automatic retries confirm delivery,
not exactly-once business execution.

The handler decides how to invoke or wake an Agent. Sending to an existing Codex
session remains the handler's responsibility.

## Inspect events

    plane events list
    plane events wait --consumer my-agent --type work_item.updated --timeout 300
    plane events ack my-agent LOCAL_SEQ

These commands are for inspection or custom consumers. They do not drive registered
callbacks. The receiver stores each event and its cursor in one SQLite transaction,
reconnects with the saved cursor, and deduplicates replayed IDs. Named consumers
have separate explicit ACK progress and must run serially when sharing a name.

Events include ID, type, workspace/project/resource/issue IDs, actor, timestamp,
and field changes. Supported changes: project, work item, comment, cycle, module,
assignment, label, and confirmed asset uploads/deletes. Task/attachment contents
remain retrievable through the API. There are no image bytes in the event stream.

On 401/403/404 or cursor 409/410, the daemon stays blocked and reports the error
in daemon status; it never silently skips history. Correct credentials or
permissions and restart. A cursor older than the replay window requires a fresh
API snapshot and a fresh profile/inbox. Already downloaded local data is not
remotely erased when server permissions change.

## Background service

    plane daemon install

Installs and starts a login service: launchd on macOS, systemd --user on Linux.
Keep the binary at its installed path. Only one installed service is supported per
OS user (use foreground daemon run for additional profiles). A user session /
user service manager must exist. For Linux startup without an interactive login,
an administrator can enable lingering for the user.

To remove the service:

- Linux: systemctl --user disable --now plane-daemon, then remove
  ~/.config/systemd/user/plane-daemon.service and run systemctl --user daemon-reload.
- macOS: launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/so.plane.daemon.plist,
  then remove that plist.

## Server installation and deployment

Apply the new migration with uv run python manage.py migrate. The event endpoint
is /api/v1/workspaces/{slug}/events/. Send Accept: text/event-stream and optional
Last-Event-ID; without that Accept header it returns a finite JSON page.

Events are inserted synchronously in the transaction that saves the model. A
PostgreSQL transaction advisory lock orders event producers so cursor order follows
commit order. This intentionally serializes event-producing writes globally in
this first implementation. At larger write volumes, replace it with a dedicated
ordered outbox dispatcher. Assignment/label bulk creation is covered; arbitrary
QuerySet.update, other bulk writes and hard deletion bypass model hooks and do
not emit events. Existing soft-delete UI/API paths are covered. Existing outbound
webhooks remain unchanged and independent.

The replay window defaults to 30 days (EVENT_RETENTION_DAYS). Old rows are kept
in the database so expired cursors can be detected; physical pruning and archival
are not included. Local inbox rows also remain until logout or explicit profile
cleanup.

The endpoint rechecks API-key/workspace access and current project membership
while streaming, including restricted guest visibility. SSE sends heartbeats and
rotates the connection after 25 seconds; the daemon resumes automatically. Disable
proxy response buffering/compression and allow idle timeouts above 30 seconds.
Responses include X-Accel-Buffering: no and bypass Django gzip buffering.
This Django WSGI implementation occupies one worker/thread per active stream;
provision threaded workers for the number of subscribed clients. Large concurrent
fleets will need an async event gateway. Storage URLs must be reachable from clients.

## Verification

    go test -race ./...
    go vet ./...
    CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o dist/plane-darwin-arm64 .
    CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -o dist/plane-darwin-amd64 .
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dist/plane-linux-amd64 .
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o dist/plane-linux-arm64 .

Backend contract tests:

    cd ../api
    uv run --env-file .env pytest plane/tests/contract/api/test_events.py

For an end-to-end run, start the local API and reachable S3-compatible storage,
apply migrations and build /tmp/plane (or set PLANE_CLI_BINARY). From apps/api:

    PYTHONPATH=. uv run --env-file .env python ../cli/tests/e2e.py

The E2E creates an isolated user/workspace, logs in, starts the daemon, creates and
updates a task, posts a comment, verifies offline replay, uploads and downloads a
PNG with SHA-256 equality, verifies deduplication and consumer acknowledgement,
and removes its fixtures. It uses real HTTP, PostgreSQL, object storage and the
compiled CLI, with no event mocks. Set PLANE_E2E_SERVER if the API is not at
http://127.0.0.1:8000. It must use the same database as the API.

Two-workspace switching and callback isolation (from apps/api):

    PYTHONPATH=. uv run --env-file .env python ../cli/tests/workspaces.py

Optional extra Linux E2E checks: set PLANE_E2E_NETWORK=1 to cut a local TCP relay
and verify reconnect/catch-up plus normal SSE rotation. Set PLANE_E2E_SERVICE=1
with a working systemd user session to install/start/verify/remove the user service.
The service check assumes no existing plane-daemon user service is installed.

To exercise a real local Codex callback, use an authenticated Codex CLI and run
from apps/api:

    CODEX_BINARY=/path/to/codex PYTHONPATH=. uv run --env-file .env python ../cli/tests/codex_callback.py

This test registers a local command, then updates a task. The daemon invokes the
command with event JSON; the command runs Codex to read the task, download and
hash an attachment, and post a verification comment. The test checks the actual
comment and automatic success record, then removes its fixtures. Set
PLANE_CLI_BINARY for the Plane executable and PLANE_CALLBACK_REPORT_DIR for logs.
It uses existing Codex authentication/provider settings and a real model request.

macOS binaries can be cross-compiled from Linux; launchd and Keychain integration
still need runtime verification on a Mac. The CLI workflow runs unit tests on both
Ubuntu and macOS and builds all four platform/architecture artifacts.
