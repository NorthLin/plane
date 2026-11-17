# Copyright (c) 2023-present Plane Software, Inc. and contributors
# SPDX-License-Identifier: AGPL-3.0-only
# See the LICENSE file for details.

from django.urls import path
from plane.api.views.workspace import WorkspaceListEndpoint

urlpatterns = [path("workspaces/", WorkspaceListEndpoint.as_view(), name="api-workspaces")]
