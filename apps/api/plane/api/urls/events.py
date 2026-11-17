# Copyright (c) 2023-present Plane Software, Inc. and contributors
# SPDX-License-Identifier: AGPL-3.0-only
# See the LICENSE file for details.

from django.urls import path
from plane.api.views.events import EventStreamEndpoint

urlpatterns = [path("workspaces/<str:slug>/events/", EventStreamEndpoint.as_view(), name="workspace-events")]
