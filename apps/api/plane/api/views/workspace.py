# Copyright (c) 2023-present Plane Software, Inc. and contributors
# SPDX-License-Identifier: AGPL-3.0-only
# See the LICENSE file for details.

from rest_framework.response import Response
from plane.api.views.base import BaseAPIView
from plane.db.models import APIToken, Workspace


class WorkspaceListEndpoint(BaseAPIView):
    def get(self, request):
        token = APIToken.objects.get(token=request.auth)
        workspaces = Workspace.objects.filter(
            workspace_member__member=request.user,
            workspace_member__is_active=True,
            workspace_member__deleted_at__isnull=True,
        )
        if token.workspace_id:
            workspaces = workspaces.filter(pk=token.workspace_id)
        return Response(list(workspaces.order_by("slug").values("id", "slug", "name").distinct()))
