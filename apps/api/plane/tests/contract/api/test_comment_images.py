# Copyright (c) 2023-present Plane Software, Inc. and contributors
# SPDX-License-Identifier: AGPL-3.0-only
# See the LICENSE file for details.

from unittest.mock import patch

import pytest

from plane.db.models import FileAsset, Issue, IssueComment, Project, ProjectMember
from plane.tests.factories import UserFactory

pytestmark = [pytest.mark.contract, pytest.mark.django_db]


@pytest.fixture
def image_task(workspace, create_user):
    project = Project.objects.create(workspace=workspace, name="Comment images", identifier="IMG")
    ProjectMember.objects.create(workspace=workspace, project=project, member=create_user, role=20)
    return Issue.objects.create(workspace=workspace, project=project, name="Screenshot evidence")


def image_asset(task, user, **kwargs):
    asset = FileAsset(
        workspace=task.workspace,
        project=task.project,
        entity_type="COMMENT_DESCRIPTION",
        asset="comment.png",
        attributes={"name": "comment.png", "type": "image/png"},
        is_uploaded=True,
        storage_metadata={"size": 1},
        **kwargs,
    )
    asset.save(created_by_id=user.id)
    return asset


def comment_url(task):
    return f"/api/v1/workspaces/{task.workspace.slug}/projects/{task.project_id}/work-items/{task.id}/comments/"


def test_comment_images_bound_and_readable(api_key_client, image_task, create_user):
    asset = image_asset(image_task, create_user)
    node = f'<image-component src="{asset.id}" status="uploaded"></image-component>'
    with patch("plane.api.views.issue.issue_activity.delay"), patch("plane.api.views.issue.model_activity.delay"):
        response = api_key_client.post(
            comment_url(image_task),
            {
                "comment_html": f"<p>Before</p>{node}<p>After</p>",
                "asset_ids": [str(asset.id)],
            },
            format="json",
        )
    assert response.status_code == 201, response.data
    asset.refresh_from_db()
    assert str(asset.comment_id) == str(response.data["id"])
    assert asset.issue_id == image_task.id
    assert asset.entity_type == "COMMENT_DESCRIPTION"
    assert asset.asset_url.endswith(f"/{asset.id}/")
    assert (
        response.data["comment_html"].index("Before")
        < response.data["comment_html"].index(str(asset.id))
        < response.data["comment_html"].index("After")
    )
    # Same project/workspace policy as ordinary image reads; cannot steal/rebind a used image.
    retry = api_key_client.post(
        comment_url(image_task), {"comment_html": node, "asset_ids": [str(asset.id)]}, format="json"
    )
    assert retry.status_code == 400
    assert IssueComment.objects.filter(issue=image_task).count() == 1


@pytest.mark.parametrize("invalid", ["other_user", "other_project", "pending", "attachment"])
def test_comment_rejects_invalid_assets_atomically(api_key_client, image_task, create_user, invalid):
    good = image_asset(image_task, create_user)
    bad = image_asset(image_task, UserFactory() if invalid == "other_user" else create_user)
    if invalid == "other_project":
        bad.project = Project.objects.create(workspace=image_task.workspace, name="Other", identifier="OTH")
    if invalid == "pending":
        bad.is_uploaded = False
    if invalid == "attachment":
        bad.entity_type = "ISSUE_ATTACHMENT"
    bad.save(disable_auto_set_user=True)
    response = api_key_client.post(
        comment_url(image_task),
        {"comment_html": "<p>Evidence</p>", "asset_ids": [str(good.id), str(bad.id)]},
        format="json",
    )
    assert response.status_code == 400, response.data
    assert not IssueComment.objects.filter(issue=image_task).exists()
    good.refresh_from_db()
    assert good.comment_id is None


def test_comment_image_upload_type(api_key_client, image_task):
    url = f"/api/v1/workspaces/{image_task.workspace.slug}/assets/"
    data = {
        "name": "image.png",
        "type": "image/png",
        "size": 12,
        "project_id": str(image_task.project_id),
        "entity_type": "COMMENT_DESCRIPTION",
    }
    with patch("plane.api.views.asset.S3Storage") as storage:
        storage.return_value.generate_presigned_post.return_value = {"url": "https://storage", "fields": {}}
        response = api_key_client.post(url, data, format="json")
    assert response.status_code == 200, response.data
    assert response.data["entity_type"] == "COMMENT_DESCRIPTION"
    asset = FileAsset.objects.get(id=response.data["asset_id"])
    assert asset.comment_id is None and asset.project_id == image_task.project_id
    data["type"] = "image/svg+xml"
    assert api_key_client.post(url, data, format="json").status_code == 400
