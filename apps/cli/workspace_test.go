// Copyright (c) 2023-present Plane Software, Inc. and contributors
// SPDX-License-Identifier: AGPL-3.0-only
// See the LICENSE file for details.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceSelectionIsolationAndLegacy(t *testing.T) {
	configDir = t.TempDir()
	defer func() { configDir = ""; workspace = "" }()
	t.Setenv("PLANE_WORKSPACE", "")
	t.Setenv("PLANE_API_KEY", "test")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/workspaces/b/events/" {
			w.Write([]byte("{}"))
			return
		}
		w.WriteHeader(404)
	}))
	defer server.Close()
	c := Config{Server: server.URL, UserID: "u", Workspace: "a"}
	if err := saveJSON(filepath.Join(configDir, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	// Existing single-workspace installs keep their original DB and callback IDs.
	legacy, err := openInboxAt(configDir)
	if err != nil {
		t.Fatal(err)
	}
	legacy.db.Exec("INSERT INTO metadata(key,value) VALUES('binding',?)", workspaceBinding(c))
	legacy.db.Exec("INSERT INTO callbacks(name,config) VALUES('agent','{}')")
	legacy.db.Close()
	if err = useWorkspace("b"); err != nil {
		t.Fatal(err)
	}
	got, _ := loadConfig()
	if got.Workspace != "b" {
		t.Fatal(got)
	}
	other, err := openInbox()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	other.db.QueryRow("SELECT COUNT(*) FROM callbacks").Scan(&n)
	if n != 0 {
		t.Fatal("callback leaked across workspaces")
	}
	other.db.Close()
	workspace = "a"
	original, err := openInbox()
	if err != nil {
		t.Fatal(err)
	}
	original.db.QueryRow("SELECT COUNT(*) FROM callbacks").Scan(&n)
	original.db.Close()
	if n != 1 {
		t.Fatal("old callback lost")
	}
	workspace = ""
	if err = useWorkspace("missing"); err == nil {
		t.Fatal("inaccessible workspace accepted")
	}
	got, _ = readConfig()
	if got.Workspace != "b" {
		t.Fatal("failed selection changed default")
	}
	t.Setenv("PLANE_WORKSPACE", "a")
	got, _ = loadConfig()
	if got.Workspace != "a" {
		t.Fatal("callback workspace env ignored")
	}
	workspace = "b"
	got, _ = loadConfig()
	if got.Workspace != "b" {
		t.Fatal("explicit workspace must override env")
	}
	names, err := knownWorkspaces(c)
	if err != nil || len(names) != 2 {
		t.Fatalf("%v %v", names, err)
	}
	// Stored credentials never change when selecting a workspace.
	raw, _ := os.ReadFile(filepath.Join(configDir, "config.json"))
	var saved Config
	json.Unmarshal(raw, &saved)
	if saved.UserID != "u" {
		t.Fatal(saved)
	}
}
