# Copyright (c) 2023-present Plane Software, Inc. and contributors
# SPDX-License-Identifier: AGPL-3.0-only
# See the LICENSE file for details.

"""Two-workspace callback E2E. Run from apps/api with its environment."""

import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import sys
import tempfile
import time
import django

os.environ.setdefault("DJANGO_SETTINGS_MODULE", "plane.settings.local")
django.setup()
from plane.db.models import APIToken, Project, ProjectMember, User, Workspace, WorkspaceMember  # noqa: E402


def main():
    suffix = secrets.token_hex(4)
    root = Path(tempfile.mkdtemp(prefix="plane-ws-e2e-"))
    binary = os.environ.get("PLANE_CLI_BINARY", "/tmp/plane")
    cli = [binary, "--config-dir", str(root)]
    user = User.objects.create(email=f"cli-ws-{suffix}@example.invalid", username=f"cli-ws-{suffix}")
    workspaces = []

    def run(*args):
        p = subprocess.run(cli + list(args), text=True, capture_output=True, timeout=40)
        if p.returncode:
            raise RuntimeError(p.stderr)
        return json.loads(p.stdout) if p.stdout.strip() else None

    def wait_run(slug, count):
        for _ in range(100):
            calls = run("--workspace", slug, "callbacks", "runs", "agent")
            if len(calls) >= count and all(c["state"] == "succeeded" for c in calls):
                return calls
            time.sleep(0.2)
        raise AssertionError((slug, calls, run("daemon", "status")))

    try:
        projects = []
        for label in ("a", "b"):
            ws = Workspace.objects.create(name=f"CLI workspace {label}", slug=f"cli-ws-{label}-{suffix}", owner=user)
            workspaces.append(ws)
            WorkspaceMember.objects.create(workspace=ws, member=user, role=20)
            project = Project.objects.create(workspace=ws, name=f"Project {label}", identifier=label.upper())
            ProjectMember.objects.create(workspace=ws, project=project, member=user, role=20)
            projects.append(project)
        a, b = workspaces
        token = APIToken.objects.create(user=user, workspace=None, label="Workspace switch E2E")
        key = root / "key"
        key.write_text(token.token)
        key.chmod(0o600)
        run(
            "auth",
            "login",
            "--server",
            os.environ.get("PLANE_E2E_SERVER", "http://127.0.0.1:8000"),
            "--workspace",
            a.slug,
            "--api-key-file",
            str(key),
        )
        key.unlink()
        assert {w["slug"] for w in run("workspace", "list")} == {a.slug, b.slug}
        handler = root / "handler.py"
        handler.write_text("""import json, os, subprocess, sys
event=json.load(sys.stdin)
assert os.environ['PLANE_WORKSPACE']==sys.argv[1]
p=subprocess.run([sys.argv[2], 'workspace', 'current'], capture_output=True, text=True, check=True)
assert json.loads(p.stdout)['workspace']==sys.argv[1]
assert event['workspace_id']==sys.argv[3]
print(json.dumps(event))
""")
        run("daemon", "start")
        issues = []
        for ws, project in zip(workspaces, projects):
            run("workspace", "use", ws.slug)
            assert run("workspace", "current")["workspace"] == ws.slug
            assert run("callbacks", "list") == []
            run(
                "callbacks",
                "add",
                "agent",
                "--event",
                "work_item.updated",
                "--",
                sys.executable,
                str(handler),
                ws.slug,
                binary,
                str(ws.id),
            )
            issues.append(run("work-items", "create", "--project", str(project.id), "--data", '{"name":"Task"}'))
        for ws, project, issue in zip(workspaces, projects, issues):
            run(
                "--workspace",
                ws.slug,
                "work-items",
                "update",
                issue["id"],
                "--project",
                str(project.id),
                "--data",
                '{"name":"Both workspaces active"}',
            )
            wait_run(ws.slug, 1)
        assert run("workspace", "current")["workspace"] == b.slug
        failed = subprocess.run(cli + ["workspace", "use", "missing-" + suffix], capture_output=True)
        assert failed.returncode and run("workspace", "current")["workspace"] == b.slug
        assert run("--workspace", a.slug, "workspace", "current")["workspace"] == a.slug
        for ws in workspaces:
            env = {**os.environ, "PLANE_WORKSPACE": ws.slug}
            p = subprocess.run(cli + ["events", "list"], capture_output=True, text=True, env=env, check=True)
            assert all(json.loads(line)["workspace_id"] == str(ws.id) for line in p.stdout.splitlines())
        run("daemon", "stop")
        for ws, project, issue in zip(workspaces, projects, issues):
            run(
                "--workspace",
                ws.slug,
                "work-items",
                "update",
                issue["id"],
                "--project",
                str(project.id),
                "--data",
                '{"priority":"high"}',
            )
        run("daemon", "start")
        for ws in workspaces:
            wait_run(ws.slug, 2)
        token.workspace = a
        token.save(update_fields=["workspace"])
        assert [w["slug"] for w in run("workspace", "list")] == [a.slug]
        failed = subprocess.run(cli + ["workspace", "use", b.slug], capture_output=True)
        assert failed.returncode
        run("workspace", "use", a.slug)
        print(
            json.dumps(
                {
                    "result": "PASS",
                    "checks": [
                        "workspace list/use/current",
                        "temporary override",
                        "two active workspaces",
                        "same callback name isolated",
                        "callback env pinned",
                        "event isolation",
                        "restart replay both workspaces",
                        "scope rejection preserves default",
                    ],
                }
            )
        )
    finally:
        try:
            if (root / "config.json").exists():
                run("auth", "logout")
        finally:
            for ws in workspaces:
                ws.delete(soft=False)
            user.delete()
            shutil.rmtree(root)


if __name__ == "__main__":
    main()
