// Copyright (c) 2023-present Plane Software, Inc. and contributors
// SPDX-License-Identifier: AGPL-3.0-only
// See the LICENSE file for details.

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func callbackFixture(t *testing.T) (*Inbox, Callback) {
	t.Helper()
	configDir = t.TempDir()
	t.Cleanup(func() { configDir = "" })
	inbox, err := openInbox()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inbox.db.Close() })
	rule := Callback{Name: "agent", Events: []string{"work_item.updated"}, Project: "p", Issue: "i", IgnoreSelf: true, Command: []string{"/bin/sh", "-c", "cat"}, Dir: configDir, Timeout: 1, MaxAttempts: 2, RetryDelay: 1, AfterID: 10, Enabled: true}
	data, _ := json.Marshal(rule)
	if _, err = inbox.db.Exec("INSERT INTO callbacks(name,config) VALUES(?,?)", rule.Name, string(data)); err != nil {
		t.Fatal(err)
	}
	return inbox, rule
}
func putCallbackEvent(t *testing.T, inbox *Inbox, id, kind, project, issue, actor string) {
	t.Helper()
	data, _ := json.Marshal(map[string]string{"id": id, "type": kind, "project_id": project, "issue_id": issue, "actor_id": actor})
	if err := inbox.put(id, data); err != nil {
		t.Fatal(err)
	}
}

func TestCallbackFiltersBaselineAndDurableDedup(t *testing.T) {
	inbox, _ := callbackFixture(t)
	putCallbackEvent(t, inbox, "10", "work_item.updated", "p", "i", "other") // history arriving late
	putCallbackEvent(t, inbox, "11", "comment.created", "p", "i", "other")
	putCallbackEvent(t, inbox, "12", "work_item.updated", "different", "i", "other")
	putCallbackEvent(t, inbox, "13", "work_item.updated", "p", "different", "other")
	putCallbackEvent(t, inbox, "14", "work_item.updated", "p", "i", "self")
	putCallbackEvent(t, inbox, "15", "work_item.updated", "p", "i", "other")
	for n := 0; n < 2; n++ {
		if err := queueCallbacks(inbox.db, "self"); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	inbox.db.QueryRow("SELECT COUNT(*) FROM callback_runs").Scan(&count)
	if count != 1 {
		t.Fatal(count)
	}
	job, err := nextCallback(inbox.db)
	if err != nil || job == nil || job.EventID != "15" {
		t.Fatalf("%+v %v", job, err)
	}
	if err = recoverCallbacks(inbox.db); err != nil {
		t.Fatal(err)
	}
	job, err = nextCallback(inbox.db)
	if err != nil || job == nil || job.Attempts != 1 {
		t.Fatalf("restart lost call: %+v %v", job, err)
	}
	if err = finishCallback(inbox.db, job, "ok", nil, false); err != nil {
		t.Fatal(err)
	}
	if err = queueCallbacks(inbox.db, "self"); err != nil {
		t.Fatal(err)
	}
	job, err = nextCallback(inbox.db)
	if err != nil || job != nil {
		t.Fatalf("duplicate delivery: %+v %v", job, err)
	}
}

func TestCallbackStdinEnvironmentLiteralArgumentsAndTimeout(t *testing.T) {
	_, rule := callbackFixture(t)
	script := filepath.Join(configDir, "handler with spaces.sh")
	body := `#!/bin/sh
printf '%s|%s|%s|%s|%s' "$1" "$PLANE_CONFIG_DIR" "$PLANE_EVENT_ID" "$PLANE_CALLBACK_ATTEMPT" "$PWD"
cat
`
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	rule.Command = []string{script, "$(touch should-not-exist)"}
	job := &callbackJob{Seq: 9, EventID: "42", Raw: `{"id":"42","type":"work_item.updated"}`, Attempts: 2, Rule: rule}
	t.Setenv("PLANE_CONFIG_DIR", "/wrong-profile")
	output, err := invokeCallback(context.Background(), job, Config{Server: "https://plane.example", Workspace: "test"})
	if err != nil {
		t.Fatal(err)
	}
	physicalDir, err := filepath.EvalSymlinks(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "$(touch should-not-exist)|"+configDir+"|42|2|"+physicalDir) || !strings.Contains(output, `"local_seq":9`) {
		t.Fatal(output)
	}
	if _, err = os.Stat(filepath.Join(configDir, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatal("expanded callback argument")
	}
	job.Rule.Command = []string{"/bin/sh", "-c", "sleep 60 & wait"}
	started := time.Now()
	_, err = invokeCallback(context.Background(), job, Config{})
	if err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(started) > 3*time.Second {
		t.Fatalf("timeout failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started = time.Now()
	_, err = invokeCallback(ctx, job, Config{})
	if err != context.Canceled || time.Since(started) > time.Second {
		t.Fatalf("stop failed: %v", err)
	}
}

func TestCallbackRetryPauseAndAttemptLimit(t *testing.T) {
	inbox, _ := callbackFixture(t)
	putCallbackEvent(t, inbox, "20", "work_item.updated", "p", "i", "other")
	inbox.db.Exec("UPDATE callbacks SET enabled=0")
	if err := queueCallbacks(inbox.db, "self"); err != nil {
		t.Fatal(err)
	}
	job, err := nextCallback(inbox.db)
	if err != nil || job != nil {
		t.Fatal("disabled callback ran")
	}
	inbox.db.Exec("UPDATE callbacks SET enabled=1")
	if err = queueCallbacks(inbox.db, "self"); err != nil {
		t.Fatal(err)
	}
	job, err = nextCallback(inbox.db)
	if err != nil || job == nil {
		t.Fatalf("resume lost event: %v", err)
	}
	failure := os.ErrPermission
	if err = finishCallback(inbox.db, job, "stderr", failure, false); err != nil {
		t.Fatal(err)
	}
	job, err = nextCallback(inbox.db)
	if err != nil || job != nil {
		t.Fatal("retry ignored delay")
	}
	inbox.db.Exec("UPDATE callback_runs SET next_at=0")
	job, err = nextCallback(inbox.db)
	if err != nil || job == nil || job.Attempts != 2 {
		t.Fatal("missing retry")
	}
	if err = finishCallback(inbox.db, job, "stderr", failure, false); err != nil {
		t.Fatal(err)
	}
	var state string
	inbox.db.QueryRow("SELECT state FROM callback_runs").Scan(&state)
	if state != "failed" {
		t.Fatal(state)
	}
	job, err = nextCallback(inbox.db)
	if err != nil || job != nil {
		t.Fatal("exceeded retry budget")
	}
}
