// Copyright (c) 2023-present Plane Software, Inc. and contributors
// SPDX-License-Identifier: AGPL-3.0-only
// See the LICENSE file for details.

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestStopRefusesToRemoveLiveDaemonState(t *testing.T) {
	configDir = t.TempDir()
	defer func() { configDir = "" }()
	if err := daemonStop(); err != nil {
		t.Fatal("stopping an absent daemon must be idempotent:", err)
	}
	lock, err := os.OpenFile(filepath.Join(configDir, "daemon.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err = daemonStop(); err == nil {
		t.Fatal("an unresponsive daemon holding the lock must block logout")
	}
}

func TestAttachmentMIMETypeOmitsCharset(t *testing.T) {
	for path, want := range map[string]string{"evidence.txt": "text/plain", "screenshot.png": "image/png", "file.unknown-plane-type": "application/octet-stream"} {
		if got := attachmentMediaType(path); got != want {
			t.Errorf("%s: got %q, want %q", path, got, want)
		}
	}
}

func TestSSEAndDurableDedup(t *testing.T) {
	configDir = t.TempDir()
	defer func() { configDir = "" }()
	inbox, e := openInbox()
	if e != nil {
		t.Fatal(e)
	}
	input := ": heartbeat\n\nid: 42\nevent: message\ndata: {\"id\":\"42\",\"type\":\"asset.uploaded\"}\n\n"
	for n := 0; n < 2; n++ {
		if e = parseSSE(strings.NewReader(input), inbox.put); e != nil {
			t.Fatal(e)
		}
	}
	if inbox.cursor() != "42" {
		t.Fatal(inbox.cursor())
	}
	var count int
	inbox.db.QueryRow("SELECT COUNT(*) FROM events").Scan(&count)
	if count != 1 {
		t.Fatal(count)
	}
	inbox.db.Close()
	inbox, e = openInbox()
	if e != nil {
		t.Fatal(e)
	}
	defer inbox.db.Close()
	if inbox.cursor() != "42" {
		t.Fatal("cursor lost on restart")
	}
	if e = inbox.put("41", []byte("{\"id\":\"41\"}")); e != nil {
		t.Fatal(e)
	}
	if inbox.cursor() != "42" {
		t.Fatal("cursor regressed")
	}
	if e = inbox.put("43", []byte("not json")); e == nil {
		t.Fatal("malformed payload accepted")
	}
	if inbox.cursor() != "42" {
		t.Fatal("advanced past malformed event")
	}
	info, _ := os.Stat(filepath.Join(configDir, "inbox.db"))
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
}
func TestSSEPartialFrameAndRevocation(t *testing.T) {
	count := 0
	f := func(id string, b []byte) error { count++; return nil }
	if e := parseSSE(strings.NewReader("id: 1\ndata: {}"), f); e != nil {
		t.Fatal(e)
	}
	if count != 0 {
		t.Fatal("accepted incomplete event")
	}
	if e := parseSSE(strings.NewReader("event: revoked\ndata: {}\n\n"), f); e == nil {
		t.Fatal("missed revocation")
	}
}
func TestAPIKeyNotSentToStorage(t *testing.T) {
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "" {
			t.Error("API key leaked to storage")
		}
		fmt.Fprint(w, "image bytes")
	}))
	defer storage.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "secret" {
			t.Error("missing key")
		}
		http.Redirect(w, r, storage.URL, 302)
	}))
	defer api.Close()
	t.Setenv("PLANE_API_KEY", "secret")
	response, e := apiRequest(context.Background(), Config{Server: api.URL}, "GET", "/api/v1/file/", nil)
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	b, _ := io.ReadAll(response.Body)
	if string(b) != "image bytes" {
		t.Fatal(string(b))
	}
}
func TestServiceEscaping(t *testing.T) {
	if got := xmlText("a&<b>"); got != "a&amp;&lt;b&gt;" {
		t.Fatal(got)
	}
	if !strings.Contains(systemdQuote("/tmp/a% b"), "%%") {
		t.Fatal("systemd specifier not escaped")
	}
}
