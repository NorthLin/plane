// Copyright (c) 2023-present Plane Software, Inc. and contributors
// SPDX-License-Identifier: AGPL-3.0-only
// See the LICENSE file for details.

package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	_ "modernc.org/sqlite"
)

type Inbox struct{ db *sql.DB }

func openInbox() (*Inbox, error) {
	if _, err := os.Stat(filepath.Join(profileDir(), "config.json")); err == nil {
		c, e := loadConfig()
		if e != nil {
			return nil, e
		}
		return openWorkspaceInbox(c)
	}
	return openInboxAt(profileDir())
}
func openInboxAt(dir string) (*Inbox, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "inbox.db")
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	f.Close()
	_ = os.Chmod(path, 0600)
	db, e := sql.Open("sqlite", path)
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	_, e = db.Exec("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY,value TEXT NOT NULL); CREATE TABLE IF NOT EXISTS events (seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT NOT NULL UNIQUE,payload TEXT NOT NULL); CREATE TABLE IF NOT EXISTS consumers (name TEXT PRIMARY KEY,seq INTEGER NOT NULL)")
	if e != nil {
		db.Close()
		return nil, e
	}
	if e = initCallbacks(db); e != nil {
		db.Close()
		return nil, e
	}
	return &Inbox{db}, nil
}
func (i *Inbox) cursor() string {
	var c string
	_ = i.db.QueryRow("SELECT value FROM metadata WHERE key='cursor'").Scan(&c)
	if c == "" {
		return "0"
	}
	return c
}
func (i *Inbox) put(id string, data []byte) error {
	if _, e := strconv.ParseUint(id, 10, 63); e != nil {
		return errors.New("invalid event ID")
	}
	var payload map[string]interface{}
	if e := json.Unmarshal(data, &payload); e != nil {
		return e
	}
	if payload["id"] != id {
		return errors.New("SSE ID and payload ID disagree")
	}
	tx, e := i.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("INSERT OR IGNORE INTO events(id,payload) VALUES(?,?)", id, string(data)); e != nil {
		return e
	}
	if _, e = tx.Exec("INSERT INTO metadata(key,value) VALUES('cursor',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value WHERE CAST(excluded.value AS INTEGER)>CAST(metadata.value AS INTEGER)", id); e != nil {
		return e
	}
	return tx.Commit()
}
func parseSSE(r io.Reader, receive func(string, []byte) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	var id, event string
	var data []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if event == "revoked" {
				return errors.New("authorization revoked")
			}
			if len(data) > 0 && id != "" {
				if e := receive(id, []byte(strings.Join(data, "\n"))); e != nil {
					return e
				}
			}
			id, event, data = "", "", nil
			continue
		}
		key, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch key {
		case "id":
			id = value
		case "event":
			event = value
		case "data":
			data = append(data, value)
		}
	}
	return scanner.Err()
}
func socketPath() string { return filepath.Join(profileDir(), "daemon.sock") }
func localClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath())
	}}}
}
func daemonStop() error {
	r, e := localClient().Post("http://unix/stop", "application/json", nil)
	if e != nil {
		stopped, err := daemonStopped()
		if err != nil {
			return err
		}
		if stopped {
			return nil
		}
		return e
	}
	r.Body.Close()
	// Wait for the receiver to release its DB/socket before logout removes files.
	for n := 0; n < 100; n++ {
		stopped, err := daemonStopped()
		if err != nil {
			return err
		}
		if stopped {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("daemon has not stopped")
}
func daemonStopped() (bool, error) {
	lock, err := os.OpenFile(filepath.Join(profileDir(), "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}
func daemonCommands() *cobra.Command {
	p := &cobra.Command{Use: "daemon", Short: "Receive events and execute registered callbacks", Long: "Run the background event receiver and callback runner. Stop preserves login, queued calls, and history."}
	p.AddCommand(&cobra.Command{Use: "run", Short: "Run the receiver and callbacks in the foreground", RunE: func(cmd *cobra.Command, args []string) error { return runDaemon() }}, &cobra.Command{Use: "start", Short: "Start the receiver and callbacks in the background", RunE: func(cmd *cobra.Command, args []string) error {
		if r, e := localClient().Get("http://unix/status"); e == nil {
			defer r.Body.Close()
			_, e = io.Copy(os.Stdout, r.Body)
			return e
		}
		if _, e := loadConfig(); e != nil {
			return e
		}
		if e := ensureDir(); e != nil {
			return e
		}
		exe, e := os.Executable()
		if e != nil {
			return e
		}
		log, e := os.OpenFile(filepath.Join(profileDir(), "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		defer log.Close()
		child := exec.Command(exe, "--config-dir", profileDir(), "daemon", "run")
		child.Stdout = log
		child.Stderr = log
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if e = child.Start(); e != nil {
			return e
		}
		_ = child.Process.Release()
		for n := 0; n < 50; n++ {
			r, e := localClient().Get("http://unix/status")
			if e == nil {
				defer r.Body.Close()
				_, e = io.Copy(os.Stdout, r.Body)
				return e
			}
			time.Sleep(100 * time.Millisecond)
		}
		return errors.New("daemon did not start; inspect daemon.log")
	}}, &cobra.Command{Use: "stop", Short: "Stop receiving and cancel active callbacks; keep pending work", RunE: func(cmd *cobra.Command, args []string) error { return daemonStop() }}, &cobra.Command{Use: "status", Short: "Show connection state, saved cursor, and errors", RunE: func(cmd *cobra.Command, args []string) error {
		r, e := localClient().Get("http://unix/status")
		if e != nil {
			return e
		}
		defer r.Body.Close()
		_, e = io.Copy(os.Stdout, r.Body)
		return e
	}}, serviceCommand())
	return p
}
func receiveWorkspace(ctx context.Context, c Config, inbox *Inbox, setState func(string, string)) {
	base, _ := workspacePath(c)
	for ctx.Err() == nil {
		token, err := tokenFor(c)
		if err != nil {
			setState("blocked", err.Error())
			<-ctx.Done()
			return
		}
		req, err := http.NewRequestWithContext(ctx, "GET", c.Server+base+"events/", nil)
		if err != nil {
			setState("blocked", err.Error())
			<-ctx.Done()
			return
		}
		req.Header.Set("X-Api-Key", token)
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Last-Event-ID", inbox.cursor())
		resp, err := client(c).Do(req)
		if err == nil {
			if resp.StatusCode == 200 {
				setState("connected", "")
				err = parseSSE(resp.Body, func(id string, data []byte) error {
					e := inbox.put(id, data)
					if e == nil {
						setState("connected", "")
					}
					return e
				})
			} else {
				err = fmt.Errorf("event stream HTTP %d", resp.StatusCode)
			}
			resp.Body.Close()
			if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 || resp.StatusCode == 409 || resp.StatusCode == 410 {
				setState("blocked", err.Error())
				<-ctx.Done()
				break
			}
		}
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			setState("reconnecting", err.Error())
		} else {
			setState("reconnecting", "")
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	return
}
func eventCommands() *cobra.Command {
	p := &cobra.Command{Use: "events", Short: "Inspect stored events or consume them manually"}
	var after int64
	var eventType, consumer string
	var timeout int
	for _, verb := range []string{"watch", "wait", "list"} {
		verb := verb
		cmd := &cobra.Command{Use: verb, Short: "Read durable local events (NDJSON); --consumer requires explicit ack", RunE: func(cmd *cobra.Command, args []string) error {
			inbox, e := openInbox()
			if e != nil {
				return e
			}
			defer inbox.db.Close()
			position := after
			if consumer != "" {
				var saved int64
				e = inbox.db.QueryRow("SELECT seq FROM consumers WHERE name=?", consumer).Scan(&saved)
				if e != nil && e != sql.ErrNoRows {
					return e
				}
				if saved > position {
					position = saved
				}
			}
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()
			deadline := time.Now().Add(time.Duration(timeout) * time.Second)
			for {
				batchStart := position
				rows, e := inbox.db.Query("SELECT seq,payload FROM events WHERE seq>? ORDER BY seq LIMIT 100", position)
				if e != nil {
					return e
				}
				for rows.Next() {
					var seq int64
					var raw string
					if e = rows.Scan(&seq, &raw); e != nil {
						rows.Close()
						return e
					}
					position = seq
					var payload map[string]interface{}
					if e = json.Unmarshal([]byte(raw), &payload); e != nil {
						rows.Close()
						return e
					}
					if eventType != "" && payload["type"] != eventType {
						continue
					}
					payload["local_seq"] = seq
					if e = emit(payload); e != nil {
						rows.Close()
						return e
					}

					if verb == "wait" {
						rows.Close()
						return nil
					}
				}
				e = rows.Err()
				rows.Close()
				if e != nil {
					return e
				}
				if verb == "list" && position == batchStart {
					return nil
				}
				if verb == "list" {
					continue
				}
				if timeout > 0 && time.Now().After(deadline) {
					return errors.New("timed out waiting for event")
				}
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(200 * time.Millisecond):
				}
			}
		}}
		cmd.Flags().Int64Var(&after, "after", 0, "Local sequence to resume after")
		cmd.Flags().StringVar(&eventType, "type", "", "Exact event type filter")
		cmd.Flags().StringVar(&consumer, "consumer", "", "Named durable consumer")
		cmd.Flags().IntVar(&timeout, "timeout", 300, "Timeout in seconds, 0 for unlimited")
		p.AddCommand(cmd)
	}
	p.AddCommand(&cobra.Command{Use: "ack CONSUMER LOCAL_SEQ", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		seq, e := strconv.ParseInt(args[1], 10, 64)
		if e != nil || seq < 0 {
			return errors.New("invalid local sequence")
		}
		i, e := openInbox()
		if e != nil {
			return e
		}
		defer i.db.Close()
		var latest int64
		_ = i.db.QueryRow("SELECT COALESCE(MAX(seq),0) FROM events").Scan(&latest)
		if seq > latest {
			return errors.New("cannot acknowledge beyond inbox")
		}
		_, e = i.db.Exec("INSERT INTO consumers(name,seq) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET seq=MAX(consumers.seq,excluded.seq)", args[0], seq)
		return e
	}})
	return p
}
