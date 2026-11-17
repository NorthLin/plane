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
	"strings"
	"testing"
)

func TestCommentImagePositions(t *testing.T) {
	ids := []string{"one", "two"}
	for _, tc := range []struct{ body, want string }{
		{"<p>Evidence</p>", "<p>Evidence</p>" + commentImageNode("one") + commentImageNode("two")},
		{"<p>Before</p>{{image:2}}<p>After</p>{{image:1}}", "<p>Before</p>" + commentImageNode("two") + "<p>After</p>" + commentImageNode("one")},
		{"{{image:1}}", commentImageNode("one") + commentImageNode("two")},
	} {
		got, err := imageCommentHTML(tc.body, ids)
		if err != nil || got != tc.want {
			t.Fatalf("%q: %q %v", tc.body, got, err)
		}
		if strings.Contains(got, "{{image:") {
			t.Fatal("unresolved placeholder")
		}
	}
	for _, body := range []string{"{{image:0}}", "{{image:3}}", "{{image:x}}"} {
		if _, err := imageCommentHTML(body, ids); err == nil {
			t.Fatalf("accepted %q", body)
		}
	}
}

func TestOldServerDoesNotReceiveImageBytes(t *testing.T) {
	configDir = t.TempDir()
	defer func() { configDir = "" }()
	t.Setenv("PLANE_API_KEY", "test-key")
	var uploaded bool
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { uploaded = true }))
	defer storage.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "test-key" {
			t.Error("missing API credential")
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"asset_id": "test", "upload_data": map[string]interface{}{"url": storage.URL, "fields": map[string]string{}}})
	}))
	defer api.Close()
	file := filepath.Join(configDir, "image.png")
	if err := os.WriteFile(file, []byte("image bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := uploadFile(Config{Server: api.URL}, "/api/v1/workspaces/test/assets/", file, "p", true)
	if err == nil || !strings.Contains(err.Error(), "upgrade Plane") {
		t.Fatalf("%v", err)
	}
	if uploaded {
		t.Fatal("sent bytes to storage before checking server compatibility")
	}
}

func TestFailedImageUploadDoesNotPostComment(t *testing.T) {
	configDir = t.TempDir()
	defer func() { configDir = "" }()
	t.Setenv("PLANE_API_KEY", "test-key")
	var posted, uploaded bool
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploaded = true
		if r.Header.Get("X-Api-Key") != "" {
			t.Error("Plane credential sent to storage")
		}
		http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
	}))
	defer storage.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/comments/") {
			posted = true
		}
		if r.Method == "GET" {
			w.Write([]byte("{}"))
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"asset_id": "test", "entity_type": "COMMENT_DESCRIPTION",
			"upload_data": map[string]interface{}{"url": storage.URL, "fields": map[string]string{}},
		})
	}))
	defer api.Close()
	file := filepath.Join(configDir, "image.png")
	if err := os.WriteFile(file, []byte("image bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	err := addImageComment(Config{Server: api.URL}, "/api/v1/workspaces/test/", "p", "i", "<p>Evidence</p>", []string{file})
	if err == nil || !strings.Contains(err.Error(), "comment was not posted") {
		t.Fatalf("%v", err)
	}
	if !uploaded || posted {
		t.Fatalf("storage requested=%v comment posted=%v", uploaded, posted)
	}
}
