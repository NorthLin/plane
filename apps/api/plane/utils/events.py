# Copyright (c) 2023-present Plane Software, Inc. and contributors
# SPDX-License-Identifier: AGPL-3.0-only
# See the LICENSE file for details.

"""Write notifications in the same transaction as model changes.

One transaction advisory lock serializes event-producing saves: a sequence alone
is NOT a commit-order cursor (a later sequence could commit before an earlier one).
QuerySet.update and bulk writes other than assignment/label creation bypass hooks;
integrations must use save() for event-producing writes. No Celery worker or webhook is needed to deliver.
"""

from contextlib import contextmanager
from crum import get_current_user
from django.db import connections, transaction
from plane.db.models.event import WorkspaceEvent

EVENT_MODELS = {
    "project": ("project", ["name", "description", "identifier", "archived_at", "deleted_at"]),
    "issue": (
        "work_item",
        ["name", "description_html", "priority", "state_id", "start_date", "target_date", "archived_at", "deleted_at"],
    ),
    "issuecomment": ("comment", ["comment_html", "access", "deleted_at"]),
    "fileasset": ("asset", ["is_uploaded", "is_deleted", "deleted_at"]),
    "issueassignee": ("work_item.assignee", ["assignee_id", "deleted_at"]),
    "issuelabel": ("work_item.label", ["label_id", "deleted_at"]),
    "cycle": ("cycle", ["name", "start_date", "end_date", "deleted_at"]),
    "module": ("module", ["name", "status", "deleted_at"]),
}


@contextmanager
def record_model_event(instance, using="default"):
    spec = EVENT_MODELS.get(instance._meta.model_name)
    if spec is None:
        yield
        return
    # Lock ordering is consistent across events, and the lock lasts until commit.
    with transaction.atomic(using=using):
        with connections[using].cursor() as cursor:
            cursor.execute("SELECT pg_advisory_xact_lock(%s)", [728194620])
        fields = [f for f in spec[1] if hasattr(instance, f)]
        before = None
        if not instance._state.adding:
            before = instance.__class__.all_objects.using(using).filter(pk=instance.pk).values(*fields).first()
        yield
        workspace_id = getattr(instance, "workspace_id", None)
        if not workspace_id:
            return
        # Read persisted values: update_fields may intentionally omit dirty fields.
        after = instance.__class__.all_objects.using(using).filter(pk=instance.pk).values(*fields).first()
        if after is None:
            return
        changes = {
            key: {"old": before.get(key) if before else None, "new": value}
            for key, value in after.items()
            if before is None or before.get(key) != value
        }
        if before is not None and not changes:
            return
        kind = spec[0]
        action = "created" if before is None else "updated"
        if after.get("deleted_at") or after.get("is_deleted"):
            action = "deleted"
        if kind == "asset":
            # Notify only once the bytes are confirmed uploaded, not when reserving a URL.
            if not after.get("is_uploaded") and action != "deleted":
                return
            if after.get("is_uploaded") and (before is None or not before.get("is_uploaded")):
                action = "uploaded"
        issue = instance if kind == "work_item" else getattr(instance, "issue", None)
        if issue is None and getattr(instance, "comment_id", None):
            issue = instance.comment.issue
        user = get_current_user()
        actor_id = user.pk if user and user.is_authenticated else (instance.updated_by_id or instance.created_by_id)
        data = {"changes": changes}
        if kind == "comment":
            data["access"] = instance.access
        if kind == "asset" and getattr(instance, "comment_id", None):
            data["access"] = instance.comment.access
        if kind == "asset":
            data.update(entity_type=instance.entity_type, attributes=instance.attributes)
        WorkspaceEvent.objects.using(using).create(
            workspace_id=workspace_id,
            project_id=instance.pk if kind == "project" else getattr(instance, "project_id", None),
            resource_id=instance.pk,
            issue_id=issue.pk if issue else None,
            issue_created_by_id=issue.created_by_id if issue else None,
            actor_id=actor_id,
            type=f"{kind}.{action}",
            data=data,
        )
