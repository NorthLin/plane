# Copyright (c) 2023-present Plane Software, Inc. and contributors
# SPDX-License-Identifier: AGPL-3.0-only
# See the LICENSE file for details.

"""Durable, ordered notifications. The row is also the transactional outbox."""

from django.db import models
from django.core.serializers.json import DjangoJSONEncoder


class WorkspaceEvent(models.Model):
    id = models.BigAutoField(primary_key=True)
    workspace = models.ForeignKey("db.Workspace", on_delete=models.CASCADE)
    project_id = models.UUIDField(null=True)
    resource_id = models.UUIDField()
    issue_id = models.UUIDField(null=True)
    issue_created_by_id = models.UUIDField(null=True)
    actor_id = models.UUIDField(null=True)
    type = models.CharField(max_length=80)
    data = models.JSONField(default=dict, encoder=DjangoJSONEncoder)
    created_at = models.DateTimeField(auto_now_add=True)

    class Meta:
        db_table = "workspace_events"
        indexes = [models.Index(fields=["workspace", "id"], name="workspace_event_cursor_idx")]
        ordering = ["id"]
