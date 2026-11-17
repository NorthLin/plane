# Copyright (c) 2023-present Plane Software, Inc. and contributors
# SPDX-License-Identifier: AGPL-3.0-only
# See the LICENSE file for details.

import pytest
from django.utils import timezone
from plane.db.models import Workspace, WorkspaceMember

pytestmark = [pytest.mark.contract, pytest.mark.django_db]


def test_list_workspaces_respects_membership_and_token_scope(api_key_client, api_token, workspace, create_user):
    other = Workspace.objects.create(name="Other", slug="cli-other", owner=create_user)
    WorkspaceMember.objects.create(workspace=other, member=create_user, role=20)
    hidden = Workspace.objects.create(name="Hidden", slug="cli-hidden", owner=create_user)
    api_token.workspace = None
    api_token.save(update_fields=["workspace"])
    response = api_key_client.get("/api/v1/workspaces/")
    assert response.status_code == 200
    ids = {str(w["id"]) for w in response.data}
    assert str(other.id) in ids and str(hidden.id) not in ids
    WorkspaceMember.objects.filter(workspace=other, member=create_user).update(deleted_at=timezone.now())
    response = api_key_client.get("/api/v1/workspaces/")
    assert str(other.id) not in {str(w["id"]) for w in response.data}
    api_token.workspace = workspace
    api_token.save(update_fields=["workspace"])
    response = api_key_client.get("/api/v1/workspaces/")
    assert {str(w["id"]) for w in response.data} == {str(workspace.id)}
    api_token.is_active = False
    api_token.save(update_fields=["is_active"])
    assert api_key_client.get("/api/v1/workspaces/").status_code in (401, 403)
