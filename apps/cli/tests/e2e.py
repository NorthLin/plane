# Copyright (c) 2023-present Plane Software, Inc. and contributors
# SPDX-License-Identifier: AGPL-3.0-only
# See the LICENSE file for details.

import base64
import hashlib
import json
import os
import shutil
import subprocess
import tempfile
import time
from pathlib import Path

# Run with the API environment, against an already running local API + S3 service.
# Every run creates its own user/workspace and removes them in finally.
import django
import secrets

os.environ.setdefault("DJANGO_SETTINGS_MODULE", "plane.settings.local")
django.setup()
from plane.db.models import (  # noqa: E402
    APIToken,
    User,
    Workspace,
    WorkspaceMember,
    Project,
    ProjectMember,
    FileAsset,
    Issue,
    WorkspaceEvent,
)  # noqa: E402
from plane.settings.storage import S3Storage  # noqa: E402

suffix = secrets.token_hex(4)
user = User.objects.create(
    email=f"cli-e2e-{suffix}@example.invalid", username="cli-e2e-" + suffix, first_name="CLI", last_name="E2E"
)
ws = Workspace.objects.create(name="CLI E2E", slug="cli-e2e-" + suffix, owner=user)
WorkspaceMember.objects.create(workspace=ws, member=user, role=20)
project = Project.objects.create(workspace=ws, name="CLI verification", identifier="CLI")
ProjectMember.objects.create(workspace=ws, project=project, member=user, role=20)
token = APIToken.objects.create(user=user, workspace=ws, label="CLI E2E temporary")
ctx = dict(token=token.token, user=str(user.id), workspace=ws.slug, project=str(project.id))
server = os.environ.get("PLANE_E2E_SERVER", "http://127.0.0.1:8000")
profile = tempfile.mkdtemp(prefix="plane-e2e-profile-")
key = Path(profile) / "input-key"
key.write_text(ctx["token"])
key.chmod(0o600)
cli = [os.environ.get("PLANE_CLI_BINARY", "/tmp/plane"), "--config-dir", profile]


def run(*args, parse=True):
    p = subprocess.run(cli + list(args), text=True, capture_output=True, timeout=40)
    if p.returncode:
        raise RuntimeError(f"{args[0:2]} failed: {p.stderr}")
    return json.loads(p.stdout) if parse and p.stdout.strip() else p.stdout


def wait_event(type, resource):
    for _ in range(100):
        events = [json.loads(line) for line in run("events", "list", parse=False).splitlines() if line]
        matches = [e for e in events if e["type"] == type and e["resource_id"] == resource]
        if matches:
            return matches[-1]
        time.sleep(0.2)
    raise AssertionError(("event not received", type, run("daemon", "status")))


try:
    run("--workspace", ctx["workspace"], "auth", "login", "--server", server, "--api-key-file", str(key))
    run("auth", "status")
    run("daemon", "start")
    project = ctx["project"]
    args = ["--project", project]
    issue = run(
        "work-items",
        "create",
        *args,
        "--data",
        json.dumps({"name": "CLI end-to-end task", "description_html": "<p>Image evidence follows</p>"}),
    )
    issue_id = issue["id"]
    ctx["issue"] = issue_id
    e = wait_event("work_item.created", issue_id)
    run("work-items", "update", issue_id, *args, "--data", '{"name":"Updated from CLI"}')
    wait_event("work_item.updated", issue_id)
    fetched = run("work-items", "get", issue_id, *args)
    assert fetched["name"] == "Updated from CLI"
    comment = run("comments", "add", *args, "--issue", issue_id, "--body", "<p>Agent progress</p>")
    wait_event("comment.created", comment["id"])
    run("daemon", "stop")
    time.sleep(0.4)
    run("work-items", "update", issue_id, *args, "--data", '{"priority":"high"}')
    run("daemon", "start")
    event = wait_event("work_item.updated", issue_id)
    for _ in range(50):
        events = [json.loads(x) for x in run("events", "list", parse=False).splitlines()]
        if any(e.get("changes", {}).get("priority", {}).get("new") == "high" for e in events):
            break
        time.sleep(0.2)
    else:
        raise AssertionError("offline event not replayed")
    path = f"/api/v1/workspaces/{ctx['workspace']}/projects/{project}/work-items/{issue_id}/attachments/"
    png = base64.b64decode(
        "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jH2kAAAAASUVORK5CYII="
    )
    source = Path(profile) / "source.png"
    source.write_bytes(png)
    upload = run("attachments", "upload", str(source), *args, "--issue", issue_id)
    asset = upload["asset_id"]
    wait_event("asset.uploaded", asset)
    dest = Path(profile) / "downloaded.png"
    run("attachments", "download", asset, *args, "--issue", issue_id, "--output", str(dest))
    assert hashlib.sha256(dest.read_bytes()).digest() == hashlib.sha256(png).digest()
    events = [json.loads(x) for x in run("events", "list", parse=False).splitlines()]
    assert len({e["id"] for e in events}) == len(events)
    pending = run("events", "wait", "--consumer", "agent", "--timeout", "2")
    run("events", "ack", "agent", str(pending["local_seq"]))
    next_event = run("events", "wait", "--consumer", "agent", "--timeout", "2")
    assert next_event["local_seq"] > pending["local_seq"]
    # Register once: daemon invokes this command, supplies JSON, and confirms
    # success automatically. No events wait/watch/ack drives these callbacks.
    handler = Path(profile) / "callback.py"
    handler.write_text("""import json, os, subprocess, sys
from pathlib import Path
event = json.load(sys.stdin)
assert event['id'] == os.environ['PLANE_EVENT_ID']
assert Path(os.environ['PLANE_CONFIG_DIR']).resolve() == Path(sys.argv[2]).resolve()
assert sys.argv[3] == '$(literal argument)'
name = os.environ['PLANE_CALLBACK_NAME']
count = Path(os.environ['PLANE_CONFIG_DIR']) / (name + '-' + event['id'])
attempt = int(count.read_text()) + 1 if count.exists() else 1
count.write_text(str(attempt))
if name == 'cancel' and attempt == 1:
    import time
    time.sleep(60)
if name == 'fail-once' and attempt == 1:
    raise SystemExit(7)
if name == 'manual' and not (count.parent / 'allow-manual').exists():
    raise SystemExit(8)
result = subprocess.run([sys.argv[1], 'work-items', 'get', event['issue_id'],
    '--project', event['project_id']], text=True, capture_output=True, check=True)
assert json.loads(result.stdout)['id'] == event['issue_id']
print(json.dumps({'event': event, 'attempt': attempt, 'profile_ok': True}))
""")

    def callback_runs(name):
        return run("callbacks", "runs", name)

    def wait_callback(name, state, after_id=0):
        for _ in range(150):
            matches = [r for r in callback_runs(name) if r["id"] > after_id and r["state"] == state]
            if matches:
                return matches[0]
            time.sleep(0.2)
        raise AssertionError(("callback not completed", name, state, callback_runs(name)))

    def register(name, *extra):
        import sys

        return run(
            "callbacks",
            "add",
            name,
            "--event",
            "work_item.updated",
            "--issue",
            issue_id,
            "--retry-delay",
            "1",
            "--max-attempts",
            "2",
            *extra,
            "--",
            sys.executable,
            str(handler),
            cli[0],
            profile,
            "$(literal argument)",
        )

    register("fail-once")
    register("manual")
    register("skip-self", "--ignore-self")
    assert not callback_runs("fail-once"), "registration replayed old history"
    run("callbacks", "disable", "manual")
    run("work-items", "update", issue_id, *args, "--data", '{"name":"Automatic callback delivery"}')
    completed = wait_callback("fail-once", "succeeded")
    assert completed["attempts"] == 2
    payload = json.loads(completed["output"])
    assert payload["event"]["issue_id"] == issue_id and payload["profile_ok"]
    assert payload["event"]["changes"]["name"]["new"] == "Automatic callback delivery"
    assert not callback_runs("manual") and not callback_runs("skip-self")
    # Disabled registrations retain their backlog; manual retry resets the budget.
    run("callbacks", "enable", "manual")
    failed = wait_callback("manual", "failed")
    assert failed["attempts"] == 2 and "exit status 8" in failed["error"]
    (Path(profile) / "allow-manual").touch()
    run("callbacks", "retry", str(failed["id"]))
    assert wait_callback("manual", "succeeded")["attempts"] == 1
    run("callbacks", "remove", "manual")
    run("callbacks", "remove", "skip-self")
    # Persistent registration and pending event survive daemon downtime.
    run("daemon", "stop")
    run("work-items", "update", issue_id, *args, "--data", '{"name":"Callback after daemon restart"}')
    run("daemon", "start")
    restarted = wait_callback("fail-once", "succeeded", completed["id"])
    assert restarted["attempts"] == 2
    time.sleep(0.5)
    assert len(callback_runs("fail-once")) == 2, "callback delivered twice after success"
    run("callbacks", "remove", "fail-once")
    register("cancel", "--timeout", "120", "--max-attempts", "1")
    run("work-items", "update", issue_id, *args, "--data", '{"name":"Stop an active callback"}')
    wait_callback("cancel", "running")
    for _ in range(100):
        if list(Path(profile).glob("cancel-*")):
            break
        time.sleep(0.1)
    else:
        raise AssertionError("callback process did not start")
    run("daemon", "stop")
    assert callback_runs("cancel")[0]["state"] == "pending"
    run("daemon", "start")
    resumed = wait_callback("cancel", "succeeded")
    assert resumed["attempts"] == 1, "shutdown consumed the retry budget"
    assert json.loads(resumed["output"])["attempt"] == 2
    run("callbacks", "remove", "cancel")
    assert run("callbacks", "list") == []
    # Revoke the actual server key while the receiver is connected.
    token.is_active = False
    token.save(update_fields=["is_active"])
    for _ in range(100):
        status = run("daemon", "status")
        if status["state"] == "blocked":
            break
        time.sleep(0.2)
    else:
        raise AssertionError(("revoked key kept receiving", status))
    token.is_active = True
    token.save(update_fields=["is_active"])
    if os.environ.get("PLANE_E2E_SERVICE") == "1":
        run("daemon", "stop")
        run("daemon", "install")
        for _ in range(100):
            try:
                if run("daemon", "status")["state"] == "connected":
                    break
            except RuntimeError:
                pass
            time.sleep(0.2)
        else:
            raise AssertionError("installed service did not connect")
    # Exercise actual TCP connection loss, automatic reconnect and catch-up.
    # A local TCP relay keeps the API/storage fixtures real while cutting only
    # this client's connection (no disruption to other Plane users).
    if os.environ.get("PLANE_E2E_NETWORK") == "1":
        import socket
        import threading
        import select
        from urllib.parse import urlparse

        origin = urlparse(server)
        assert origin.scheme == "http", "TCP relay test uses a local HTTP API"
        relay = socket.socket()
        relay.bind(("127.0.0.1", 0))
        relay.listen()
        relay.settimeout(0.2)
        online = threading.Event()
        online.set()
        done = threading.Event()

        def forward(downstream):
            upstream = None
            try:
                if not online.is_set():
                    return
                upstream = socket.create_connection((origin.hostname, origin.port or 80), timeout=5)
                while not done.is_set() and online.is_set():
                    ready, _, _ = select.select([downstream, upstream], [], [], 0.1)
                    for src in ready:
                        data = src.recv(65536)
                        if not data:
                            return
                        (upstream if src is downstream else downstream).sendall(data)
            except OSError:
                pass
            finally:
                downstream.close()
                if upstream:
                    upstream.close()

        def accept():
            while not done.is_set():
                try:
                    conn, _ = relay.accept()
                    threading.Thread(target=forward, args=(conn,), daemon=True).start()
                except socket.timeout:
                    pass
                except OSError:
                    return

        threading.Thread(target=accept, daemon=True).start()
        network_profile = tempfile.mkdtemp(prefix="plane-net-")
        original_cli = cli
        cli = [original_cli[0], "--config-dir", network_profile]
        try:
            run(
                "--workspace",
                ws.slug,
                "auth",
                "login",
                "--server",
                f"http://127.0.0.1:{relay.getsockname()[1]}",
                "--api-key-file",
                str(key),
            )
            run("daemon", "start")
            wait_event("work_item.created", issue_id)
            online.clear()
            time.sleep(0.5)
            resource = Issue.objects.get(pk=issue_id)
            resource.name = "Changed while TCP disconnected"
            resource.save(update_fields=["name"])
            expected = str(WorkspaceEvent.objects.filter(resource_id=resource.id).latest("id").id)
            online.set()
            for _ in range(100):
                if any(
                    e["id"] == expected
                    for e in [json.loads(x) for x in run("events", "list", parse=False).splitlines()]
                ):
                    break
                time.sleep(0.2)
            else:
                raise AssertionError("TCP outage lost an event")
            # A full server-side rotation must also reconnect with its saved cursor.
            time.sleep(27)
            resource.name = "Changed after SSE rotation"
            resource.save(update_fields=["name"])
            expected = str(WorkspaceEvent.objects.filter(resource_id=resource.id).latest("id").id)
            for _ in range(100):
                if any(
                    e["id"] == expected
                    for e in [json.loads(x) for x in run("events", "list", parse=False).splitlines()]
                ):
                    break
                time.sleep(0.2)
            else:
                raise AssertionError("SSE rotation lost an event")
        finally:
            try:
                run("auth", "logout")
            except Exception:
                pass
            done.set()
            relay.close()
            shutil.rmtree(network_profile)
            cli = original_cli
    checks = [
        "login",
        "SSE delivery",
        "create/read/update work item",
        "comment event",
        "offline replay",
        "PNG upload event",
        "download SHA256 equality",
        "event deduplication",
        "consumer acknowledgment",
        "midstream API key revocation",
        "registered callback automatic JSON delivery",
        "callback retry, pause/resume, manual retry, self filter",
        "callback persistence across daemon restart",
        "active callback cancellation and restart recovery",
    ]
    if os.environ.get("PLANE_E2E_NETWORK") == "1":
        checks += ["TCP outage reconnect/catch-up", "SSE connection rotation"]
    if os.environ.get("PLANE_E2E_SERVICE") == "1":
        checks += ["systemd user service install/connect"]
    print(
        json.dumps(
            {
                "result": "PASS",
                "checks": checks,
                "workspace": ctx["workspace"],
                "profile": profile,
            }
        )
    )
finally:
    try:
        run("daemon", "stop")
    except Exception:
        pass
    try:
        run("auth", "logout")
    except Exception:
        pass
    storage = S3Storage()
    for asset in FileAsset.all_objects.filter(workspace=ws):
        storage.s3_client.delete_object(Bucket=os.environ["AWS_S3_BUCKET_NAME"], Key=asset.asset.name)
    ws.delete(soft=False)
    user.delete()
    shutil.rmtree(profile)
    if os.environ.get("PLANE_E2E_SERVICE") == "1":
        subprocess.run(
            ["systemctl", "--user", "disable", "--now", "plane-daemon.service"], check=True, capture_output=True
        )
        (Path.home() / ".config/systemd/user/plane-daemon.service").unlink(missing_ok=True)
        subprocess.run(["systemctl", "--user", "daemon-reload"], check=True, capture_output=True)
