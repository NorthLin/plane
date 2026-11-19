# Copyright (c) 2023-present Plane Software, Inc. and contributors
# SPDX-License-Identifier: AGPL-3.0-only
# See the LICENSE file for details.

import json
from types import SimpleNamespace
from unittest.mock import Mock
from uuid import uuid4

from plane.utils import live_events


def test_publish_is_gated_and_contains_ids_only(monkeypatch):
    client = Mock()
    monkeypatch.setattr(live_events, "_publish_client", lambda: client)
    monkeypatch.delenv("LIVE_EVENTS_ENABLED", raising=False)
    project, issue, actor = uuid4(), uuid4(), uuid4()
    assert live_events.publish_work_item_event(project, [issue], "comment", "created", actor) is False
    client.publish.assert_not_called()
    monkeypatch.setenv("LIVE_EVENTS_ENABLED", "1")
    assert live_events.publish_work_item_event(project, [issue, issue], "comment", "created", actor)
    channel, raw = client.publish.call_args.args
    payload = json.loads(raw)
    assert channel == "plane:live-events:" + str(project)
    assert payload["issue_ids"] == [str(issue)]
    assert set(payload) == {"v", "project_id", "kind", "verb", "issue_ids", "actor_id", "ts", "settle"}
    assert payload["actor_id"] == str(actor)


def test_redis_outage_does_not_break_mutations(monkeypatch):
    monkeypatch.setenv("LIVE_EVENTS_ENABLED", "1")
    client = Mock()
    client.publish.side_effect = ConnectionError("offline")
    monkeypatch.setattr(live_events, "_publish_client", lambda: client)
    assert live_events.publish_work_item_event(uuid4(), [uuid4()], "issue", "updated", None) is False


def test_parent_and_related_issues_are_invalidated():
    issue, parent, previous_parent, related = [str(uuid4()) for _ in range(4)]
    ids = live_events.extract_issue_ids(
        "issue.updated", issue, {"parent": {"id": parent}}, {"parent": previous_parent}, []
    )
    assert set(ids) == {issue, parent, previous_parent}
    ids = live_events.extract_issue_ids("issue_relation.deleted", issue, {"related_issue": related}, {}, [])
    assert set(ids) == {issue, related}


def test_bulk_and_metadata_invalidations(monkeypatch):
    monkeypatch.setenv("LIVE_EVENTS_ENABLED", "1")
    client = Mock()
    monkeypatch.setattr(live_events, "_publish_client", lambda: client)
    project = uuid4()
    assert live_events.publish_work_item_event(project, [uuid4() for _ in range(501)], "issue", "updated", None)
    assert json.loads(client.publish.call_args.args[1])["issue_ids"] == "*"
    assert live_events.publish_metadata_event(project, "state", "updated", None)
    assert json.loads(client.publish.call_args.args[1])["issue_ids"] == "*"
    assert not live_events.publish_metadata_event(project, "unknown", "updated", None)
    issue = str(uuid4())
    assert live_events.extract_issue_ids("issue.updated", None, {}, {}, [SimpleNamespace(issue_id=issue)]) == [issue]
