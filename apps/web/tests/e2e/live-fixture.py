# Copyright (c) 2023-present Plane Software, Inc. and contributors
# SPDX-License-Identifier: AGPL-3.0-only
# See the LICENSE file for details.

"""Isolated browser fixture. Run with the API environment; JSON contains temporary credentials."""

import json
import os
import secrets
import sys
from pathlib import Path

import django

os.environ.setdefault("DJANGO_SETTINGS_MODULE", "plane.settings.local")
django.setup()
from django.conf import settings  # noqa: E402
from django.test import Client  # noqa: E402
from django.utils import timezone  # noqa: E402
from plane.db.models import (  # noqa: E402
    APIToken,
    Profile,
    Project,
    ProjectMember,
    User,
    Workspace,
    WorkspaceMember,
)  # noqa: E402
from plane.license.models import Instance  # noqa: E402
from plane.settings.storage import S3Storage  # noqa: E402

path = Path(sys.argv[2])
if sys.argv[1] == "cleanup":
    data = json.loads(path.read_text())
    Workspace.objects.filter(slug=data["workspace"]).delete()
    User.objects.filter(pk=data["user"]).delete()
    path.unlink()
else:
    suffix = secrets.token_hex(5)
    Instance.objects.get_or_create(
        defaults=dict(
            instance_name="E2E",
            current_version="1.5.2",
            last_checked_at=timezone.now(),
            is_setup_done=True,
            is_signup_screen_visited=True,
            is_verified=True,
        ),
        instance_id="live-e2e",
    )
    user = User.objects.create(
        email=f"live-{suffix}@example.invalid",
        username=f"live-{suffix}",
        first_name="Live",
        last_name="Test",
    )
    ws = Workspace.objects.create(name="Live E2E", slug=f"live-{suffix}", owner=user)
    WorkspaceMember.objects.create(workspace=ws, member=user, role=20)
    Profile.objects.update_or_create(
        user=user,
        defaults=dict(
            is_onboarded=True,
            is_tour_completed=True,
            last_workspace_id=ws.id,
            onboarding_step=dict(
                profile_complete=True,
                workspace_create=True,
                workspace_invite=True,
                workspace_join=True,
            ),
        ),
    )
    project = Project.objects.create(
        workspace=ws, name="Live verification", identifier="LIVE"
    )
    ProjectMember.objects.create(workspace=ws, project=project, member=user, role=20)
    token = APIToken.objects.create(user=user, workspace=ws, label="Temporary live E2E")
    client = Client()
    client.force_login(user)
    storage = S3Storage()
    try:
        storage.s3_client.head_bucket(Bucket=storage.aws_storage_bucket_name)
    except Exception:
        storage.s3_client.create_bucket(Bucket=storage.aws_storage_bucket_name)
    data = dict(
        user=str(user.id),
        workspace=ws.slug,
        project=str(project.id),
        token=token.token,
        cookie_name=settings.SESSION_COOKIE_NAME,
        cookie=client.cookies[settings.SESSION_COOKIE_NAME].value,
    )
    path.write_text(json.dumps(data))
    path.chmod(0o600)
    print("Created isolated live E2E fixture")
