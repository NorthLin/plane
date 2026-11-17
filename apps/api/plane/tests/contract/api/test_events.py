# Copyright (c) 2023-present Plane Software, Inc. and contributors
# SPDX-License-Identifier: AGPL-3.0-only
# See the LICENSE file for details.

from datetime import timedelta
from unittest.mock import patch

import pytest
from django.db import transaction
from django.utils import timezone
from plane.api.views.events import visible_events
from plane.db.models import (
    APIToken,
    FileAsset,
    Issue,
    IssueComment,
    Project,
    ProjectMember,
    WorkspaceEvent,
    WorkspaceMember,
)
from plane.tests.factories import UserFactory

pytestmark = [pytest.mark.contract, pytest.mark.django_db]


@pytest.fixture
def event_project(workspace, create_user):
    project = Project.objects.create(name="Events", identifier="EVT", workspace=workspace)
    ProjectMember.objects.create(project=project, workspace=workspace, member=create_user, role=20)
    return project


def test_business_and_event_rollback_together(workspace, event_project):
    before = WorkspaceEvent.objects.count()
    with pytest.raises(RuntimeError), transaction.atomic():
        Issue.objects.create(name="rolled back", project=event_project, workspace=workspace)
        raise RuntimeError("rollback")
    assert not Issue.objects.filter(name="rolled back").exists()
    assert WorkspaceEvent.objects.count() == before


def test_cursor_replay_and_changes(api_key_client, workspace, event_project):
    issue = Issue.objects.create(name="before", project=event_project, workspace=workspace)
    cursor = WorkspaceEvent.objects.latest("id").id
    issue.name = "after"
    issue.save(update_fields=["name"])
    url = f"/api/v1/workspaces/{workspace.slug}/events/?after={cursor}"
    response = api_key_client.get(url)
    assert response.status_code == 200
    events = response.data["results"]
    assert len(events) == 1
    assert events[0]["type"] == "work_item.updated"
    assert events[0]["changes"]["name"] == {"old": "before", "new": "after"}
    assert api_key_client.get(url).data == response.data
    assert api_key_client.get(url.split("?")[0], {"after": response.data["next_cursor"]}).data["results"] == []


def test_asset_event_only_after_upload(workspace, event_project):
    issue = Issue.objects.create(name="Screenshot", project=event_project, workspace=workspace)
    asset = FileAsset.objects.create(
        workspace=workspace, project=event_project, issue=issue, entity_type="ISSUE_ATTACHMENT", asset="test.png"
    )
    assert not WorkspaceEvent.objects.filter(resource_id=asset.id).exists()
    asset.is_uploaded = True
    asset.save(update_fields=["is_uploaded"])
    assert WorkspaceEvent.objects.get(resource_id=asset.id).type == "asset.uploaded"
    asset.save(update_fields=["is_uploaded"])
    assert WorkspaceEvent.objects.filter(resource_id=asset.id).count() == 1


def test_scopes_and_revocation(api_key_client, workspace, event_project, create_user):
    issue = Issue.objects.create(name="Secret", project=event_project, workspace=workspace)
    assert visible_events(create_user.id, workspace.id).filter(resource_id=issue.id).exists()
    membership = ProjectMember.objects.get(project=event_project, member=create_user)
    membership.is_active = False
    membership.save()
    assert not visible_events(create_user.id, workspace.id).exists()
    WorkspaceMember.objects.filter(workspace=workspace, member=create_user).update(is_active=False)
    assert api_key_client.get(f"/api/v1/workspaces/{workspace.slug}/events/").status_code == 404


def test_guest_visibility(workspace, event_project):
    guest = UserFactory()
    WorkspaceMember.objects.create(workspace=workspace, member=guest, role=5)
    ProjectMember.objects.create(workspace=workspace, project=event_project, member=guest, role=5)
    other = Issue.objects.create(name="Other", workspace=workspace, project=event_project)
    own = Issue(name="Own", workspace=workspace, project=event_project)
    own.save(created_by_id=guest.id)
    comment = IssueComment.objects.create(issue=own, project=event_project, workspace=workspace, access="INTERNAL")
    events = visible_events(guest.id, workspace.id)
    assert events.filter(resource_id=own.id).exists()
    assert not events.filter(resource_id__in=[other.id, comment.id]).exists()


@pytest.mark.django_db(transaction=True)
def test_sse_frames_and_midstream_revocation(api_key_client, api_token, workspace, event_project):
    issue = Issue.objects.create(name="SSE", workspace=workspace, project=event_project)
    response = api_key_client.get(f"/api/v1/workspaces/{workspace.slug}/events/", HTTP_ACCEPT="text/event-stream")
    assert response.status_code == 200
    assert response["X-Accel-Buffering"] == "no"
    stream = iter(response.streaming_content)
    assert next(stream) == b"retry: 1000\n\n"
    assert str(event_project.id).encode() in next(stream)
    assert str(issue.id).encode() in next(stream)
    assert next(stream) == b": heartbeat\n\n"
    APIToken.objects.filter(pk=api_token.pk).update(is_active=False)
    with patch("plane.api.views.events.time.sleep"):
        assert next(stream) == b"event: revoked\ndata: {}\n\n"
    response.close()


def test_invalid_and_expired_cursors(api_key_client, workspace, event_project):
    url = f"/api/v1/workspaces/{workspace.slug}/events/"
    assert api_key_client.get(url, {"after": "no"}).status_code == 400
    assert api_key_client.get(url, {"after": "-1"}).status_code == 400
    first = WorkspaceEvent.objects.latest("id")
    issue = Issue.objects.create(name="Expired", project=event_project, workspace=workspace)
    WorkspaceEvent.objects.filter(resource_id=issue.id).update(created_at=timezone.now() - timedelta(days=31))
    assert api_key_client.get(url, {"after": first.id}).status_code == 410
    assert api_key_client.get(url, {"after": 9999999999}).status_code in (409, 410)


def test_update_fields_does_not_report_unsaved_changes(workspace, event_project):
    issue = Issue.objects.create(name="Persisted", workspace=workspace, project=event_project)
    cursor = WorkspaceEvent.objects.latest("id").id
    issue.name = "Not saved"
    issue.save(update_fields=["updated_at"])
    assert not WorkspaceEvent.objects.filter(id__gt=cursor).exists()


def test_bulk_assignment_events_and_deletion(workspace, event_project, create_user):
    from plane.db.models import IssueAssignee

    issue = Issue.objects.create(name="Assign", project=event_project, workspace=workspace)
    assignment = IssueAssignee(issue=issue, project=event_project, workspace=workspace, assignee=create_user)
    IssueAssignee.objects.bulk_create([assignment])
    assert WorkspaceEvent.objects.filter(resource_id=assignment.pk, type="work_item.assignee.created").exists()
    IssueAssignee.objects.filter(pk=assignment.pk).delete()
    assert WorkspaceEvent.objects.filter(resource_id=assignment.pk, type="work_item.assignee.deleted").exists()


def test_comment_access_change_does_not_leak_previous_internal_html(workspace, event_project):
    guest = UserFactory()
    WorkspaceMember.objects.create(workspace=workspace, member=guest, role=5)
    ProjectMember.objects.create(workspace=workspace, project=event_project, member=guest, role=5)
    own = Issue(name="Own", workspace=workspace, project=event_project)
    own.save(created_by_id=guest.id)
    comment = IssueComment.objects.create(issue=own, project=event_project, workspace=workspace, access="INTERNAL")
    comment.access = "EXTERNAL"
    comment.comment_html = "<p>Now public</p>"
    comment.save()
    assert not visible_events(guest.id, workspace.id).filter(resource_id=comment.id).exists()


@pytest.mark.django_db(transaction=True)
def test_event_cursor_follows_commit_order(workspace, event_project):
    import threading
    from django.db import close_old_connections

    started = threading.Event()
    committed = threading.Event()
    failures = []

    def concurrent_save():
        close_old_connections()
        try:
            started.set()
            Project.objects.create(name="Second", identifier="SECOND", workspace_id=workspace.id)
            committed.set()
        except Exception as error:
            failures.append(error)
        finally:
            close_old_connections()

    with transaction.atomic():
        first = Project.objects.create(name="First", identifier="FIRST", workspace=workspace)
        first_event = WorkspaceEvent.objects.get(resource_id=first.id)
        worker = threading.Thread(target=concurrent_save)
        worker.start()
        assert started.wait(3)
        assert not committed.wait(0.2), "second event committed ahead of an earlier uncommitted event"
    worker.join(5)
    assert not failures
    assert committed.is_set()
    second = Project.objects.get(identifier="SECOND", workspace=workspace)
    assert WorkspaceEvent.objects.get(resource_id=second.id).id > first_event.id
