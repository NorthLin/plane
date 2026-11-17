# Copyright (c) 2023-present Plane Software, Inc. and contributors
# SPDX-License-Identifier: AGPL-3.0-only
# See the LICENSE file for details.

"""Authenticated SSE and finite polling over the same durable cursor."""

import asyncio
import json
import time
from datetime import timedelta

from channels.db import database_sync_to_async
from django.core.handlers.asgi import ASGIRequest
from django.conf import settings
from django.core.serializers.json import DjangoJSONEncoder
from django.db import close_old_connections
from django.db.models import Max, Q
from django.http import StreamingHttpResponse
from django.utils import timezone
from rest_framework.response import Response
from rest_framework.renderers import JSONRenderer

from plane.api.views.base import BaseAPIView
from plane.db.models import APIToken, ProjectMember, Workspace, WorkspaceEvent, WorkspaceMember


def visible_events(user_id, workspace_id):
    members = ProjectMember.objects.filter(
        workspace_id=workspace_id, member_id=user_id, is_active=True, project__deleted_at__isnull=True
    )
    full = members.filter(Q(role__gt=5) | Q(project__guest_view_all_features=True)).values("project_id")
    guests = members.filter(role=5, project__guest_view_all_features=False).values("project_id")
    return (
        WorkspaceEvent.objects.filter(workspace_id=workspace_id)
        .filter(
            Q(project_id__in=full)
            | Q(project_id__in=guests, issue_created_by_id=user_id)
            | Q(project_id__isnull=True, issue_id__isnull=True)
        )
        .exclude(
            (
                (Q(data__has_key="access") & Q(data__access="INTERNAL"))
                | (Q(data__changes__has_key="access") & Q(data__changes__access__old="INTERNAL"))
            )
            & ~Q(project_id__in=members.filter(role__gt=5).values("project_id"))
        )
    )


def event_payload(event):
    return {
        "id": str(event.id),
        "type": event.type,
        "workspace_id": str(event.workspace_id),
        "project_id": str(event.project_id) if event.project_id else None,
        "resource_id": str(event.resource_id),
        "issue_id": str(event.issue_id) if event.issue_id else None,
        "actor_id": str(event.actor_id) if event.actor_id else None,
        "occurred_at": event.created_at.isoformat(),
        **event.data,
    }


class EventRenderer(JSONRenderer):
    media_type = "text/event-stream"
    format = "sse"


class EventStreamEndpoint(BaseAPIView):
    renderer_classes = [JSONRenderer, EventRenderer]

    def get(self, request, slug):
        workspace = Workspace.objects.filter(slug=slug).first()
        if (
            workspace is None
            or not WorkspaceMember.objects.filter(workspace=workspace, member=request.user, is_active=True).exists()
        ):
            return Response({"error": "Workspace not found"}, status=404)
        token = APIToken.objects.get(token=request.auth)
        if token.workspace_id and token.workspace_id != workspace.id:
            return Response({"error": "Workspace not found"}, status=404)
        try:
            cursor = int(request.headers.get("Last-Event-ID") or request.query_params.get("after", "0"))
            limit = min(max(int(request.query_params.get("limit", "100")), 1), 500)
            if cursor < 0:
                raise ValueError
        except ValueError:
            return Response({"error": "after must be a nonnegative integer; limit must be an integer"}, status=400)
        cutoff = timezone.now() - timedelta(days=getattr(settings, "EVENT_RETENTION_DAYS", 30))
        expired = WorkspaceEvent.objects.filter(workspace=workspace, created_at__lt=cutoff).aggregate(value=Max("id"))[
            "value"
        ]
        if cursor and expired and cursor < expired:
            return Response({"error": "cursor_expired", "reset_after": str(expired)}, status=410)
        latest = WorkspaceEvent.objects.filter(workspace=workspace).aggregate(value=Max("id"))["value"] or 0
        if cursor > latest:
            return Response({"error": "cursor_ahead", "latest": str(latest)}, status=409)
        if "text/event-stream" not in request.headers.get("Accept", ""):
            rows = list(
                visible_events(request.user.pk, workspace.pk).filter(id__gt=cursor, created_at__gte=cutoff)[:limit]
            )
            return Response(
                {"results": [event_payload(e) for e in rows], "next_cursor": str(rows[-1].id if rows else cursor)}
            )
        user_id, workspace_id, token_id = request.user.pk, workspace.pk, token.pk

        def stream(sleep=True):
            nonlocal cursor
            # Short-lived streams bound WSGI worker occupancy and reauthenticate regularly.
            deadline = time.monotonic() + 25
            try:
                yield "retry: 1000\n\n"
                while time.monotonic() < deadline:
                    close_old_connections()
                    now = timezone.now()
                    if (
                        not APIToken.objects.filter(pk=token_id, user__is_active=True, is_active=True)
                        .filter(Q(expired_at__isnull=True) | Q(expired_at__gt=now))
                        .exists()
                        or not WorkspaceMember.objects.filter(
                            workspace_id=workspace_id, member_id=user_id, is_active=True
                        ).exists()
                    ):
                        yield "event: revoked\ndata: {}\n\n"
                        return
                    rows = list(
                        visible_events(user_id, workspace_id).filter(id__gt=cursor, created_at__gte=cutoff)[:limit]
                    )
                    for event in rows:
                        payload = json.dumps(event_payload(event), cls=DjangoJSONEncoder)
                        yield f"id: {event.id}\nevent: message\ndata: {payload}\n\n"
                        cursor = event.id
                    yield ": heartbeat\n\n"
                    if sleep:
                        time.sleep(1)
            finally:
                close_old_connections()

        async def async_stream():
            # Django buffers sync iterators under ASGI. Advance the iterator off
            # the event loop, closing DB connections after each polling batch.
            iterator = stream(sleep=False)
            sentinel = object()

            def advance():
                return next(iterator, sentinel)

            try:
                while True:
                    frame = await database_sync_to_async(advance)()
                    if frame is sentinel:
                        return
                    yield frame
                    if frame == ": heartbeat\n\n":
                        await asyncio.sleep(1)
            finally:
                await database_sync_to_async(iterator.close)()

        content = async_stream() if isinstance(request._request, ASGIRequest) else stream()
        response = StreamingHttpResponse(content, content_type="text/event-stream")
        response["Cache-Control"] = "no-cache, no-transform"
        response["X-Accel-Buffering"] = "no"
        response["Content-Encoding"] = "identity"
        return response
