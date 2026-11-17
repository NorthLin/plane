// Copyright (c) 2023-present Plane Software, Inc. and contributors
// SPDX-License-Identifier: AGPL-3.0-only
// See the LICENSE file for details.

package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

func readConfig() (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(profileDir(), "config.json"))
	if err != nil {
		return c, fmt.Errorf("not logged in: plane auth login: %w", err)
	}
	err = json.Unmarshal(b, &c)
	return c, err
}

func workspaceBinding(c Config) string { return c.Server + "|" + c.Workspace + "|" + c.UserID }

// Each workspace keeps its own cursor, callbacks, runs and consumer positions.
// Keep the pre-multi-workspace inbox in place for its original binding.
func inboxDir(c Config) (string, error) {
	legacy := filepath.Join(profileDir(), "inbox.db")
	if _, err := os.Stat(legacy); err == nil {
		db, err := sql.Open("sqlite", legacy)
		if err != nil {
			return "", err
		}
		var binding string
		err = db.QueryRow("SELECT value FROM metadata WHERE key='binding'").Scan(&binding)
		db.Close()
		if err != nil && err != sql.ErrNoRows {
			return "", err
		}
		if binding == workspaceBinding(c) {
			return profileDir(), nil
		}
		if binding == "" {
			saved, e := readConfig()
			if e != nil {
				return "", e
			}
			if saved.Workspace == "" {
				return "", errors.New("legacy inbox has no workspace binding")
			}
			if saved.Workspace == c.Workspace && saved.Server == c.Server && saved.UserID == c.UserID {
				return profileDir(), nil
			}
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(workspaceBinding(c))))
	return filepath.Join(profileDir(), "workspaces", key), nil
}

func openWorkspaceInbox(c Config) (*Inbox, error) {
	if c.Workspace == "" {
		return nil, errors.New("select a workspace: plane workspace use SLUG")
	}
	dir, err := inboxDir(c)
	if err != nil {
		return nil, err
	}
	inbox, err := openInboxAt(dir)
	if err != nil {
		return nil, err
	}
	binding := workspaceBinding(c)
	_, err = inbox.db.Exec("INSERT OR IGNORE INTO metadata(key,value) VALUES('binding',?)", binding)
	if err == nil {
		var stored string
		err = inbox.db.QueryRow("SELECT value FROM metadata WHERE key='binding'").Scan(&stored)
		if err == nil && stored != binding {
			err = errors.New("inbox belongs to a different server/workspace/user")
		}
	}
	if err == nil {
		err = saveJSON(filepath.Join(dir, "workspace.json"), map[string]string{"server": c.Server, "user_id": c.UserID, "workspace": c.Workspace})
	}
	if err != nil {
		inbox.db.Close()
		return nil, err
	}
	return inbox, nil
}

func knownWorkspaces(c Config) ([]string, error) {
	names := map[string]bool{}
	if c.Workspace != "" {
		names[c.Workspace] = true
	}
	// Discover saved inboxes, including the legacy profile-root inbox.
	dirs := []string{profileDir()}
	entries, err := os.ReadDir(filepath.Join(profileDir(), "workspaces"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, filepath.Join(profileDir(), "workspaces", entry.Name()))
		}
	}
	for _, dir := range dirs {
		raw, err := os.ReadFile(filepath.Join(dir, "workspace.json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var saved Config
		if err = json.Unmarshal(raw, &saved); err != nil {
			return nil, err
		}
		if saved.Server == c.Server && saved.UserID == c.UserID && saved.Workspace != "" {
			names[saved.Workspace] = true
		}
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	return result, nil
}

func validateWorkspace(c Config) error {
	base, err := workspacePath(c)
	if err != nil {
		return err
	}
	resp, err := apiRequest(context.Background(), c, "GET", base+"events/?limit=1", nil)
	if err != nil {
		return fmt.Errorf("cannot access workspace %q: %w", c.Workspace, err)
	}
	resp.Body.Close()
	return nil
}

func useWorkspace(slug string) error {
	if strings.TrimSpace(slug) == "" {
		return errors.New("workspace slug must not be empty")
	}
	// Serialize default selection without holding a DB lock during the network request.
	c, err := readConfig()
	if err != nil {
		return err
	}
	target := c
	target.Workspace = slug
	if err = validateWorkspace(target); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(profileDir(), "config.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	latest, err := readConfig()
	if err != nil {
		return err
	}
	if latest.Server != c.Server || latest.UserID != c.UserID {
		return errors.New("login changed during workspace selection; try again")
	}
	// Preserve the old workspace's legacy state before changing the default.
	if c.Workspace != "" {
		i, e := openWorkspaceInbox(c)
		if e != nil {
			return e
		}
		i.db.Close()
	}
	i, err := openWorkspaceInbox(target)
	if err != nil {
		return err
	}
	i.db.Close()
	latest.Workspace = slug
	if err = saveJSON(filepath.Join(profileDir(), "config.json"), latest); err != nil {
		return err
	}
	return emit(map[string]string{"workspace": slug, "server": latest.Server})
}

func workspaceCommands() *cobra.Command {
	p := &cobra.Command{Use: "workspace", Short: "List workspaces or change the default", Long: `Switch the default without logging in again. Callbacks stay in their registered
workspace; the daemon continues serving saved workspaces. --workspace selects
a workspace for one command; PLANE_WORKSPACE provides a process default.`}
	p.AddCommand(&cobra.Command{Use: "current", Short: "Show the effective workspace and server", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		c, e := loadConfig()
		if e != nil {
			return e
		}
		return emit(map[string]string{"workspace": c.Workspace, "server": c.Server})
	}}, &cobra.Command{Use: "use SLUG", Short: "Validate access and save the default workspace", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error { return useWorkspace(args[0]) }}, &cobra.Command{Use: "list", Short: "List workspaces accessible with this API key", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		c, e := loadConfig()
		if e != nil {
			return e
		}
		return call(c, "GET", "/api/v1/workspaces/", "")
	}})
	return p
}

type workspaceState struct {
	State     string `json:"state"`
	LastError string `json:"last_error"`
	Cursor    string `json:"cursor"`
}

func runDaemon() error {
	c, err := readConfig()
	if err != nil {
		return err
	}
	if c.Workspace == "" {
		return errors.New("select a workspace: plane workspace use SLUG")
	}
	if err = ensureDir(); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(profileDir(), "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("daemon already running")
	}
	_ = os.Remove(socketPath())
	listener, err := net.Listen("unix", socketPath())
	if err != nil {
		return err
	}
	defer os.Remove(socketPath())
	_ = os.Chmod(socketPath(), 0600)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	states := map[string]workspaceState{}
	var mu sync.Mutex
	var workers sync.WaitGroup
	// Workers pin their own config: selecting a new default cannot redirect a callback.
	start := func(slug string) error {
		mu.Lock()
		_, exists := states[slug]
		mu.Unlock()
		if exists {
			return nil
		}
		cfg := c
		cfg.Workspace = slug
		inbox, e := openWorkspaceInbox(cfg)
		if e != nil {
			return e
		}
		if e = recoverCallbacks(inbox.db); e != nil {
			inbox.db.Close()
			return e
		}
		mu.Lock()
		states[slug] = workspaceState{State: "connecting", Cursor: inbox.cursor()}
		mu.Unlock()
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer inbox.db.Close()
			done := make(chan struct{})
			go func() { defer close(done); runCallbacks(ctx, inbox, cfg) }()
			receiveWorkspace(ctx, cfg, inbox, func(state, lastError string) {
				cursor := inbox.cursor()
				mu.Lock()
				states[slug] = workspaceState{State: state, LastError: lastError, Cursor: cursor}
				mu.Unlock()
			})
			<-done
		}()
		return nil
	}
	if err = start(c.Workspace); err != nil {
		return err
	}
	defer func() { cancel(); workers.Wait() }()
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		current, e := readConfig()
		if e != nil {
			http.Error(w, e.Error(), 500)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		selected := states[current.Workspace]
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"state": selected.State, "last_error": selected.LastError, "cursor": selected.Cursor, "workspace": current.Workspace, "workspaces": states})
	})
	mux.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		w.WriteHeader(204)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		cancel()
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(listener)
	defer server.Close()
	for ctx.Err() == nil {
		current, e := readConfig()
		if e != nil {
			return e
		}
		if current.Server != c.Server || current.UserID != c.UserID {
			return errors.New("login changed; restart daemon")
		}
		slugs, e := knownWorkspaces(current)
		if e != nil {
			return e
		}
		for _, slug := range slugs {
			if e = start(slug); e != nil {
				return e
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil
}
