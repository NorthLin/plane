# Copyright (c) 2023-present Plane Software, Inc. and contributors
# SPDX-License-Identifier: AGPL-3.0-only
# See the LICENSE file for details.

"""Registered command -> daemon -> Codex -> Plane comment smoke test.

Run from apps/api using its environment. Requires a running API/storage and an
authenticated Codex executable (CODEX_BINARY). No business fixtures are reused.
"""

import hashlib
import json
import os
from pathlib import Path
import secrets
import shlex
import shutil
import subprocess
import tempfile
import time

import django

os.environ.setdefault("DJANGO_SETTINGS_MODULE", "plane.settings.local")
django.setup()
from plane.db.models import (  # noqa: E402
    APIToken,
    FileAsset,
    IssueComment,
    Project,
    ProjectMember,
    User,
    Workspace,
    WorkspaceMember,
)
from plane.settings.storage import S3Storage  # noqa: E402


def main():
    codex = os.environ.get("CODEX_BINARY") or shutil.which("codex")
    if not codex:
        raise RuntimeError("Set CODEX_BINARY to an authenticated Codex executable")
    binary = os.environ.get("PLANE_CLI_BINARY") or shutil.which("plane")
    if not binary:
        raise RuntimeError("Build/install Plane CLI or set PLANE_CLI_BINARY")
    report_dir = Path(os.environ.get("PLANE_CALLBACK_REPORT_DIR") or tempfile.mkdtemp(prefix="plane-callback-report-"))
    report_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    report_dir.chmod(0o700)
    root = Path(tempfile.mkdtemp(prefix="plane-codex-callback-"))
    profile = root / "profile"
    cli = [binary, "--config-dir", str(profile)]
    user = ws = None

    def run(*args, parse=True):
        result = subprocess.run(cli + list(args), capture_output=True, text=True, timeout=45)
        if result.returncode:
            raise RuntimeError(f"{args[:2]}: {result.stderr}")
        return json.loads(result.stdout) if parse and result.stdout.strip() else result.stdout

    try:
        suffix = secrets.token_hex(4)
        user = User.objects.create(email=f"cli-callback-{suffix}@example.invalid", username=f"cli-callback-{suffix}")
        ws = Workspace.objects.create(name="CLI Codex callback test", slug=f"cli-callback-{suffix}", owner=user)
        WorkspaceMember.objects.create(workspace=ws, member=user, role=20)
        project = Project.objects.create(workspace=ws, name="Callback verification", identifier="CALL")
        ProjectMember.objects.create(workspace=ws, project=project, member=user, role=20)
        token = APIToken.objects.create(user=user, workspace=ws, label="Temporary callback test")
        key = root / "input-key"
        key.write_text(token.token)
        key.chmod(0o600)
        run(
            "auth",
            "login",
            "--server",
            os.environ.get("PLANE_E2E_SERVER", "http://127.0.0.1:8000"),
            "--workspace",
            ws.slug,
            "--api-key-file",
            str(key),
        )
        key.unlink()
        run("daemon", "start")
        proof = secrets.token_hex(12)
        issue = run(
            "work-items",
            "create",
            "--project",
            str(project.id),
            "--data",
            json.dumps({"name": "Codex callback test", "description_html": f"<p>Verification code: {proof}</p>"}),
        )
        issue_id = issue["id"]
        attachment = root / "evidence.txt"
        attachment.write_text(f"Evidence from Plane attachment: {secrets.token_hex(24)}\n")
        expected_hash = hashlib.sha256(attachment.read_bytes()).hexdigest()
        uploaded = run("attachments", "upload", str(attachment), "--project", str(project.id), "--issue", issue_id)
        command = shlex.join(cli)
        download = root / "downloaded-evidence.txt"
        marker = f"CODEX_CALLBACK_OK_{suffix}"
        prompt = f"""You are running an explicitly authorized local Plane callback integration test.
The Plane daemon invoked this registered callback after a task update.
Use shell commands to complete only this isolated task.
Plane CLI command prefix: {command}
Project UUID: {project.id}
Work item UUID: {issue_id}
Attachment UUID: {uploaded["asset_id"]}
1. Run the CLI work-items get command and read the verification code from description_html.
2. Run attachments download for the given asset, --project and --issue, --output {shlex.quote(str(download))}.
3. Compute the downloaded file's SHA-256.
4. Run comments add with --project and --issue and --body containing an HTML paragraph with
   the marker {marker}, the verification code you read, and the SHA-256 you computed.
Do not access credential files, change code, delete anything, or acknowledge events.
After the API comment succeeds, return its ID and a brief confirmation.
"""
        (report_dir / "prompt.txt").write_text(prompt)
        # The daemon invokes this registered script after receiving the event.
        callback = root / "invoke-codex.py"
        callback.write_text("""import json, os, subprocess, sys
from pathlib import Path
event = json.load(sys.stdin)
assert event['id'] == os.environ['PLANE_EVENT_ID']
report = Path(sys.argv[2])
(report / 'event.json').write_text(json.dumps(event, indent=2))
with (report / 'codex.jsonl').open('w') as output, (report / 'codex.stderr').open('w') as errors:
    result = subprocess.run([sys.argv[1], 'exec', '--ephemeral', '--skip-git-repo-check',
        '-C', sys.argv[3], '-s', 'workspace-write', '-c', 'sandbox_workspace_write.network_access=true',
        '-c', 'approval_policy="never"', '--json', '-o', str(report / 'codex-final.txt'), '-'],
        input=(report / 'prompt.txt').read_text(), text=True, stdout=output, stderr=errors, timeout=240)
raise SystemExit(result.returncode)
""")
        import sys

        run(
            "callbacks",
            "add",
            "codex",
            "--event",
            "work_item.updated",
            "--issue",
            issue_id,
            "--timeout",
            "270",
            "--max-attempts",
            "1",
            "--",
            sys.executable,
            str(callback),
            codex,
            str(report_dir),
            str(root),
        )
        triggered_at = time.monotonic()
        run(
            "work-items",
            "update",
            issue_id,
            "--project",
            str(project.id),
            "--data",
            '{"name":"Please verify the callback"}',
        )
        received_seconds = None
        for _ in range(600):
            calls = run("callbacks", "runs", "codex")
            if calls and received_seconds is None:
                received_seconds = round(time.monotonic() - triggered_at, 3)
            if calls and calls[0]["state"] in {"succeeded", "failed"}:
                break
            time.sleep(0.5)
        else:
            raise AssertionError("registered Codex callback did not finish")
        assert calls[0]["state"] == "succeeded", calls
        assert calls[0]["attempts"] == 1
        event = json.loads((report_dir / "event.json").read_text())
        assert event["issue_id"] == issue_id
        comments = list(IssueComment.objects.filter(issue_id=issue_id, comment_html__contains=marker))
        assert len(comments) == 1, "Codex did not post exactly one callback result"
        comment = comments[0]
        assert proof in comment.comment_html, "Codex did not read the task verification code"
        assert expected_hash in comment.comment_html, "Codex did not verify the attachment bytes"
        assert download.exists() and hashlib.sha256(download.read_bytes()).hexdigest() == expected_hash
        report = {
            "result": "PASS",
            "codex_version": subprocess.check_output([codex, "--version"], text=True).strip(),
            "workspace": ws.slug,
            "issue_id": issue_id,
            "event_id": event["id"],
            "local_seq": event["local_seq"],
            "comment_id": str(comment.id),
            "comment_html": comment.comment_html,
            "attachment_sha256": expected_hash,
            "event_latency_seconds": received_seconds,
            "total_seconds": round(time.monotonic() - triggered_at, 3),
            "checks": [
                "SSE callback",
                "real Codex execution",
                "task read",
                "attachment download/hash",
                "Plane comment writeback",
                "registered command auto-dispatch",
                "automatic success confirmation",
            ],
        }
        (report_dir / "result.json").write_text(json.dumps(report, indent=2))
        print(json.dumps({**report, "report_dir": str(report_dir)}))
    finally:
        for args in [("daemon", "stop"), ("auth", "logout")]:
            try:
                run(*args)
            except Exception:
                pass
        if ws:
            storage = S3Storage()
            for asset in FileAsset.all_objects.filter(workspace=ws):
                storage.s3_client.delete_object(Bucket=os.environ["AWS_S3_BUCKET_NAME"], Key=asset.asset.name)
            ws.delete(soft=False)
        if user:
            user.delete()
        shutil.rmtree(root)


if __name__ == "__main__":
    main()
