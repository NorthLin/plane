// Copyright (c) 2023-present Plane Software, Inc. and contributors
// SPDX-License-Identifier: AGPL-3.0-only
// See the LICENSE file for details.

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

type Callback struct {
	Workspace   string   `json:"workspace"`
	Name        string   `json:"name"`
	Events      []string `json:"events"`
	Project     string   `json:"project,omitempty"`
	Issue       string   `json:"issue,omitempty"`
	IgnoreSelf  bool     `json:"ignore_self"`
	Command     []string `json:"command"`
	Dir         string   `json:"dir"`
	Timeout     int      `json:"timeout_seconds"`
	MaxAttempts int      `json:"max_attempts"`
	RetryDelay  int      `json:"retry_delay_seconds"`
	AfterID     int64    `json:"after_event_id"`
	Enabled     bool     `json:"enabled"`
}

func initCallbacks(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS callbacks (name TEXT PRIMARY KEY, config TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1, cursor INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS callback_runs (id INTEGER PRIMARY KEY AUTOINCREMENT, callback TEXT NOT NULL, event_seq INTEGER NOT NULL, state TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0, next_at INTEGER NOT NULL DEFAULT 0, started_at TEXT, finished_at TEXT, error TEXT NOT NULL DEFAULT '', output TEXT NOT NULL DEFAULT '', UNIQUE(callback,event_seq));
 CREATE INDEX IF NOT EXISTS callback_runs_ready ON callback_runs(state,next_at,id);`)
	return err
}

// Include events still in transit when choosing the future-events baseline.
func callbackBaseline(c Config) (int64, error) {
	base, err := workspacePath(c)
	if err != nil {
		return 0, err
	}
	var after int64
	for {
		resp, err := apiRequest(context.Background(), c, "GET", base+"events/?limit=500&after="+strconv.FormatInt(after, 10), nil)
		if err != nil {
			return 0, err
		}
		var page struct {
			Results []json.RawMessage `json:"results"`
			Next    string            `json:"next_cursor"`
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return 0, err
		}
		if len(page.Results) == 0 {
			return after, nil
		}
		next, err := strconv.ParseInt(page.Next, 10, 64)
		if err != nil || next <= after {
			return 0, errors.New("invalid event baseline from server")
		}
		after = next
	}
}

func callbackCommands() *cobra.Command {
	p := &cobra.Command{Use: "callbacks", Short: "Run registered commands automatically on Plane events", Long: "Callbacks run in the daemon and receive one event JSON on stdin. Exit 0 confirms\ndelivery; failure or timeout retries. PLANE_CONFIG_DIR selects the login profile.\nPLANE_EVENT_ID stays the same across retries. Start with plane daemon start."}
	var rule Callback
	var executable, directory string
	add := &cobra.Command{Use: "add NAME [-- COMMAND ARG...]", Short: "Register a command; deliver each event as JSON on stdin", Long: "Receive one event JSON on stdin; exit 0 for success. Use --exec PATH or a command\nafter --. Arguments are literal, without shell expansion. New rules handle future events; no daemon restart is needed.",
		Example: "  plane callbacks add agent --event work_item.updated --exec /path/to/handle-task\n  plane callbacks add agent --event comment.created -- python3 /path/to/handle.py",
		Args:    cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			rule.Workspace = cfg.Workspace
			rule.Name = args[0]
			if rule.Name == "" {
				return errors.New("callback name must not be empty")
			}
			if executable != "" && len(args) > 1 {
				return errors.New("use --exec or a command after --, not both")
			}
			if executable != "" {
				rule.Command = []string{executable}
			} else {
				if cmd.ArgsLenAtDash() != 1 || len(args) < 2 {
					return errors.New("provide --exec PATH or -- COMMAND ARG...")
				}
				rule.Command = append([]string(nil), args[1:]...)
			}
			if len(rule.Events) == 0 {
				return errors.New("provide --event TYPE (repeatable), or --event '*' for all events")
			}
			for _, event := range rule.Events {
				if strings.TrimSpace(event) == "" {
					return errors.New("event type must not be empty")
				}
			}
			if rule.Timeout < 1 || rule.Timeout > 86400 || rule.MaxAttempts < 1 || rule.MaxAttempts > 100 || rule.RetryDelay < 1 || rule.RetryDelay > 3600 {
				return errors.New("timeout must be 1..86400, max-attempts 1..100, retry-delay 1..3600")
			}
			path, err := exec.LookPath(rule.Command[0])
			if err != nil {
				return err
			}
			rule.Command[0], err = filepath.Abs(path)
			if err != nil {
				return err
			}
			rule.Dir, err = filepath.Abs(directory)
			if err != nil {
				return err
			}
			info, err := os.Stat(rule.Dir)
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return errors.New("--dir must be a directory")
			}
			inbox, err := openWorkspaceInbox(cfg)
			if err != nil {
				return err
			}
			defer inbox.db.Close()
			var exists int
			if err = inbox.db.QueryRow("SELECT COUNT(*) FROM callbacks WHERE name=?", rule.Name).Scan(&exists); err != nil {
				return err
			}
			if exists != 0 {
				return errors.New("callback name already exists; remove it before registering again")
			}
			rule.AfterID, err = callbackBaseline(cfg)
			if err != nil {
				return err
			}
			rule.Enabled = true
			data, err := json.Marshal(rule)
			if err != nil {
				return err
			}
			_, err = inbox.db.Exec("INSERT INTO callbacks(name,config) VALUES(?,?)", rule.Name, string(data))
			if err != nil {
				return err
			}
			return emit(rule)
		}}
	add.Flags().StringSliceVar(&rule.Events, "event", nil, "Event type (repeatable); '*' matches all")
	add.Flags().StringVar(&rule.Project, "project", "", "Only events in this project UUID")
	add.Flags().StringVar(&rule.Issue, "issue", "", "Only events for this work item UUID")
	add.Flags().BoolVar(&rule.IgnoreSelf, "ignore-self", false, "Skip events caused by the logged-in user")
	add.Flags().StringVar(&executable, "exec", "", "Executable or script path; receives JSON on stdin")
	add.Flags().StringVar(&directory, "dir", ".", "Working directory, saved as an absolute path")
	add.Flags().IntVar(&rule.Timeout, "timeout", 300, "Seconds allowed per invocation")
	add.Flags().IntVar(&rule.MaxAttempts, "max-attempts", 3, "Total attempts per event, including the first")
	add.Flags().IntVar(&rule.RetryDelay, "retry-delay", 5, "Seconds between failed attempts")
	p.AddCommand(add, &cobra.Command{Use: "list", Short: "List registered callbacks and their settings", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		i, e := openInbox()
		if e != nil {
			return e
		}
		defer i.db.Close()
		rules, e := readCallbacks(i.db, false)
		if e != nil {
			return e
		}
		return emit(rules)
	}})
	for _, action := range []string{"enable", "disable", "remove"} {
		action := action
		descriptions := map[string]string{"enable": "Resume queued calls and matching events", "disable": "Pause new calls; let an active call finish", "remove": "Remove a rule and its history; let an active call finish"}
		p.AddCommand(&cobra.Command{Use: action + " NAME", Short: descriptions[action], Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			i, e := openInbox()
			if e != nil {
				return e
			}
			defer i.db.Close()
			tx, e := i.db.Begin()
			if e != nil {
				return e
			}
			defer tx.Rollback()
			var result sql.Result
			if action == "remove" {
				result, e = tx.Exec("DELETE FROM callbacks WHERE name=?", args[0])
			} else {
				result, e = tx.Exec("UPDATE callbacks SET enabled=? WHERE name=?", action == "enable", args[0])
			}
			if e != nil {
				return e
			}
			n, _ := result.RowsAffected()
			if n == 0 {
				return errors.New("callback not found")
			}
			if action == "remove" {
				if _, e = tx.Exec("DELETE FROM callback_runs WHERE callback=?", args[0]); e != nil {
					return e
				}
			}
			if e = tx.Commit(); e != nil {
				return e
			}
			return emit(map[string]string{"name": args[0], "action": action})
		}})
	}
	var limit int
	runs := &cobra.Command{Use: "runs [NAME]", Short: "Show recent calls, attempts, output, and errors", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		i, e := openInbox()
		if e != nil {
			return e
		}
		defer i.db.Close()
		filter := ""
		params := []interface{}{}
		if len(args) > 0 {
			filter = " WHERE r.callback=?"
			params = append(params, args[0])
		}
		if limit < 1 || limit > 1000 {
			return errors.New("limit must be 1..1000")
		}
		params = append(params, limit)
		rows, e := i.db.Query("SELECT r.id,r.callback,e.id,r.event_seq,r.state,r.attempts,r.error,r.output,COALESCE(r.started_at,''),COALESCE(r.finished_at,'') FROM callback_runs r JOIN events e ON e.seq=r.event_seq"+filter+" ORDER BY r.id DESC LIMIT ?", params...)
		if e != nil {
			return e
		}
		defer rows.Close()
		result := []map[string]interface{}{}
		for rows.Next() {
			var id, seq int64
			var attempts int
			var name, event, state, lastError, output, started, finished string
			if e = rows.Scan(&id, &name, &event, &seq, &state, &attempts, &lastError, &output, &started, &finished); e != nil {
				return e
			}
			result = append(result, map[string]interface{}{"id": id, "callback": name, "event_id": event, "local_seq": seq, "state": state, "attempts": attempts, "error": lastError, "output": output, "started_at": started, "finished_at": finished})
		}
		if e = rows.Err(); e != nil {
			return e
		}
		return emit(result)
	}}
	runs.Flags().IntVar(&limit, "limit", 20, "Maximum recent calls to return")
	p.AddCommand(runs, &cobra.Command{Use: "retry RUN_ID", Short: "Retry a failed call with a fresh attempt budget", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, e := strconv.ParseInt(args[0], 10, 64)
		if e != nil {
			return e
		}
		i, e := openInbox()
		if e != nil {
			return e
		}
		defer i.db.Close()
		r, e := i.db.Exec("UPDATE callback_runs SET state='pending',attempts=0,next_at=0,error='',finished_at=NULL WHERE id=? AND state='failed'", id)
		if e != nil {
			return e
		}
		n, _ := r.RowsAffected()
		if n == 0 {
			return errors.New("failed call not found")
		}
		return emit(map[string]int64{"retried": id})
	}})
	return p
}

func readCallbacks(db *sql.DB, enabledOnly bool) ([]Callback, error) {
	query := "SELECT config,enabled FROM callbacks"
	if enabledOnly {
		query += " WHERE enabled=1"
	}
	query += " ORDER BY name"
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rules := []Callback{}
	for rows.Next() {
		var raw string
		var rule Callback
		var enabled bool
		if err = rows.Scan(&raw, &enabled); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &rule); err != nil {
			return nil, err
		}
		rule.Enabled = enabled
		rules = append(rules, rule)
	}
	return rules, rows.Err()
}

func (r Callback) matches(payload map[string]interface{}, user string) bool {
	if r.Project != "" && payload["project_id"] != r.Project {
		return false
	}
	if r.Issue != "" && payload["issue_id"] != r.Issue {
		return false
	}
	if r.IgnoreSelf && payload["actor_id"] == user {
		return false
	}
	for _, kind := range r.Events {
		if kind == "*" || payload["type"] == kind {
			return true
		}
	}
	return false
}

// Persist matching deliveries and scanning progress together, independent of execution.
func queueCallbacks(db *sql.DB, user string) error {
	rules, err := readCallbacks(db, true)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		err = queueCallback(tx, rule, user)
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
func queueCallback(tx *sql.Tx, rule Callback, user string) error {
	var cursor int64
	if err := tx.QueryRow("SELECT cursor FROM callbacks WHERE name=? AND enabled=1", rule.Name).Scan(&cursor); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	rows, err := tx.Query("SELECT seq,id,payload FROM events WHERE seq>? ORDER BY seq LIMIT 100", cursor)
	if err != nil {
		return err
	}
	var matches []int64
	for rows.Next() {
		var seq int64
		var id, raw string
		if err = rows.Scan(&seq, &id, &raw); err != nil {
			rows.Close()
			return err
		}
		cursor = seq
		eventID, e := strconv.ParseInt(id, 10, 64)
		if e != nil {
			rows.Close()
			return e
		}
		if eventID <= rule.AfterID {
			continue
		}
		var payload map[string]interface{}
		if err = json.Unmarshal([]byte(raw), &payload); err != nil {
			rows.Close()
			return err
		}
		if rule.matches(payload, user) {
			matches = append(matches, seq)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, seq := range matches {
		if _, err = tx.Exec("INSERT OR IGNORE INTO callback_runs(callback,event_seq) VALUES(?,?)", rule.Name, seq); err != nil {
			return err
		}
	}
	_, err = tx.Exec("UPDATE callbacks SET cursor=? WHERE name=?", cursor, rule.Name)
	return err
}

func recoverCallbacks(db *sql.DB) error {
	_, err := db.Exec("UPDATE callback_runs SET state='pending',attempts=MAX(attempts-1,0),next_at=0,error='daemon stopped before confirmation' WHERE state='running'")
	return err
}

type callbackJob struct {
	ID, Seq      int64
	EventID, Raw string
	Attempts     int
	Rule         Callback
}

func nextCallback(db *sql.DB) (*callbackJob, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var j callbackJob
	var raw string
	err = tx.QueryRow("SELECT r.id,r.event_seq,e.id,e.payload,r.attempts,c.config FROM callback_runs r JOIN callbacks c ON c.name=r.callback JOIN events e ON e.seq=r.event_seq WHERE c.enabled=1 AND r.state='pending' AND r.next_at<=? ORDER BY r.id LIMIT 1", time.Now().Unix()).Scan(&j.ID, &j.Seq, &j.EventID, &j.Raw, &j.Attempts, &raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(raw), &j.Rule); err != nil {
		return nil, err
	}
	j.Attempts++
	_, err = tx.Exec("UPDATE callback_runs SET state='running',attempts=?,started_at=?,finished_at=NULL WHERE id=?", j.Attempts, time.Now().UTC().Format(time.RFC3339Nano), j.ID)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &j, nil
}

// Bound a verbose child's combined stdout/stderr to 64 KiB.
type callbackOutput struct{ bytes.Buffer }

func (b *callbackOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := 64*1024 - b.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.Buffer.Write(p)
	}
	return n, nil
}

func invokeCallback(ctx context.Context, j *callbackJob, c Config) (string, error) {
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(j.Raw), &payload); err != nil {
		return "", err
	}
	payload["local_seq"] = j.Seq
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	if len(j.Rule.Command) == 0 {
		return "", errors.New("callback command is empty")
	}
	child := exec.Command(j.Rule.Command[0], j.Rule.Command[1:]...)
	child.Dir = j.Rule.Dir
	child.Stdin = bytes.NewReader(append(raw, byte(10)))
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	dir, err := filepath.Abs(profileDir())
	if err != nil {
		return "", err
	}
	// Override inherited profile hints so nested Plane calls always use this login.
	overrides := map[string]string{"PLANE_CONFIG_DIR": dir, "PLANE_SERVER": c.Server, "PLANE_WORKSPACE": c.Workspace, "PLANE_CALLBACK_NAME": j.Rule.Name, "PLANE_EVENT_ID": j.EventID, "PLANE_CALLBACK_ATTEMPT": strconv.Itoa(j.Attempts)}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, exists := overrides[key]; !exists {
			child.Env = append(child.Env, entry)
		}
	}
	for key, value := range overrides {
		child.Env = append(child.Env, key+"="+value)
	}
	var output callbackOutput
	child.Stdout = &output
	child.Stderr = &output
	if err = child.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	timer := time.NewTimer(time.Duration(j.Rule.Timeout) * time.Second)
	defer timer.Stop()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
		<-done
		err = ctx.Err()
	case <-timer.C:
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
		<-done
		err = fmt.Errorf("callback timed out after %ds", j.Rule.Timeout)
	}
	return strings.ToValidUTF8(output.String(), "�"), err
}

func finishCallback(db *sql.DB, j *callbackJob, output string, callErr error, cancelled bool) error {
	state, lastError, next := "succeeded", "", int64(0)
	attempts := j.Attempts
	if cancelled {
		state = "pending"
		lastError = "daemon stopped before confirmation"
		attempts--
	} else if callErr != nil {
		lastError = callErr.Error()
		state = "failed"
		if attempts < j.Rule.MaxAttempts {
			state = "pending"
			next = time.Now().Unix() + int64(j.Rule.RetryDelay)
		}
	}
	_, err := db.Exec("UPDATE callback_runs SET state=?,attempts=?,next_at=?,finished_at=?,error=?,output=? WHERE id=?", state, attempts, next, time.Now().UTC().Format(time.RFC3339Nano), lastError, output, j.ID)
	return err
}

func runCallbacks(ctx context.Context, inbox *Inbox, c Config) {
	for ctx.Err() == nil {
		if err := queueCallbacks(inbox.db, c.UserID); err != nil {
			fmt.Fprintln(os.Stderr, "queue callbacks:", err)
		} else {
			job, err := nextCallback(inbox.db)
			if err != nil {
				fmt.Fprintln(os.Stderr, "claim callback:", err)
			} else if job != nil {
				output, callErr := invokeCallback(ctx, job, c)
				if err = finishCallback(inbox.db, job, output, callErr, ctx.Err() != nil); err != nil {
					// Keep the running row for startup recovery instead of silently losing it.
					fmt.Fprintln(os.Stderr, "record callback:", err)
					return
				}
				continue
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}
