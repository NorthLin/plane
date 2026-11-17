// Copyright (c) 2023-present Plane Software, Inc. and contributors
// SPDX-License-Identifier: AGPL-3.0-only
// See the LICENSE file for details.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/zalando/go-keyring"
	"golang.org/x/term"
)

type Config struct {
	Server     string `json:"server"`
	Workspace  string `json:"workspace"`
	UserID     string `json:"user_id"`
	Credential string `json:"credential"`
}

var version = "dev"

var configDir, workspace string
var root = &cobra.Command{Use: "plane", Short: "Manage Plane work and run local event callbacks", Version: version, SilenceUsage: true, SilenceErrors: true}

func main() {
	root.PersistentFlags().StringVar(&configDir, "config-dir", "", "Isolated profile directory (default: ~/.config/plane)")
	root.PersistentFlags().StringVar(&workspace, "workspace", "", "Workspace for this command (overrides the saved default)")
	root.PersistentFlags().Bool("json", true, "Emit machine-readable JSON")
	root.AddCommand(authCommands(), apiCommand(), resourceCommands("projects"), resourceCommands("work-items"), commentCommands(), attachmentCommands(), daemonCommands(), workspaceCommands(), callbackCommands(), eventCommands())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func profileDir() string {
	if configDir != "" {
		return configDir
	}
	if v := os.Getenv("PLANE_CONFIG_DIR"); v != "" {
		return v
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "plane")
}
func ensureDir() error {
	if err := os.MkdirAll(profileDir(), 0700); err != nil {
		return err
	}
	return os.Chmod(profileDir(), 0700)
}
func loadConfig() (Config, error) {
	c, err := readConfig()
	if err != nil {
		return c, err
	}
	if value := os.Getenv("PLANE_WORKSPACE"); value != "" {
		c.Workspace = value
	}
	if workspace != "" {
		c.Workspace = workspace
	}
	return c, nil
}
func credentialKey(c Config) string {
	p, _ := filepath.Abs(profileDir())
	return p + "|" + c.Server + "|" + c.UserID
}
func tokenFor(c Config) (string, error) {
	if t := os.Getenv("PLANE_API_KEY"); t != "" {
		return t, nil
	}
	if c.Credential == "keyring" {
		return keyring.Get("plane-cli", credentialKey(c))
	}
	b, e := os.ReadFile(filepath.Join(profileDir(), "token"))
	return strings.TrimSpace(string(b)), e
}
func saveJSON(path string, v interface{}) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".plane-*.tmp")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(f.Name(), path)
}
func emit(v interface{}) error { return json.NewEncoder(os.Stdout).Encode(v) }
func client(c Config) *http.Client {
	return &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		// Never forward API credentials to object storage or a different origin.
		if req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme {
			req.Header.Del("X-Api-Key")
		}
		return nil
	}}
}
func apiRequest(ctx context.Context, c Config, method, path string, body io.Reader) (*http.Response, error) {
	if !strings.HasPrefix(path, "/api/v1/") || strings.Contains(path, "://") {
		return nil, errors.New("API path must start with /api/v1/")
	}
	token, e := tokenFor(c)
	if e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(ctx, method, c.Server+path, body)
	if e != nil {
		return nil, e
	}
	req.Header.Set("X-Api-Key", token)
	req.Header.Set("Content-Type", "application/json")
	resp, e := client(c).Do(req)
	if e != nil {
		return nil, e
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, fmt.Errorf("API HTTP %d: %s", resp.StatusCode, b)
	}
	return resp, nil
}
func call(c Config, method, path, body string) error {
	resp, e := apiRequest(context.Background(), c, method, path, strings.NewReader(body))
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode == 204 {
		return emit(map[string]bool{"ok": true})
	}
	_, e = io.Copy(os.Stdout, resp.Body)
	fmt.Println()
	return e
}
func workspacePath(c Config) (string, error) {
	if c.Workspace == "" {
		return "", errors.New("provide --workspace or log in with --workspace")
	}
	return "/api/v1/workspaces/" + url.PathEscape(c.Workspace) + "/", nil
}
func authCommands() *cobra.Command {
	auth := &cobra.Command{Use: "auth", Short: "Manage this device's credentials"}
	var server, keyFile string
	login := &cobra.Command{Use: "login", Short: "Validate an API key and save credentials", RunE: func(cmd *cobra.Command, args []string) error {
		u, e := url.Parse(strings.TrimRight(server, "/"))
		if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || (u.Scheme != "http" && u.Scheme != "https") {
			return errors.New("--server must be an http(s) origin")
		}
		if e = ensureDir(); e != nil {
			return e
		}
		var token string
		if keyFile != "" {
			var b []byte
			if keyFile == "-" {
				b, e = io.ReadAll(io.LimitReader(os.Stdin, 8192))
			} else {
				b, e = os.ReadFile(keyFile)
			}
			if e != nil {
				return e
			}
			token = strings.TrimSpace(string(b))
		} else {
			token = os.Getenv("PLANE_API_KEY")
		}
		if token == "" {
			fmt.Fprint(os.Stderr, "API key: ")
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			if err != nil {
				return err
			}
			token = strings.TrimSpace(string(b))
		}
		if token == "" {
			return errors.New("empty API key")
		}
		c := Config{Server: u.String(), Workspace: workspace, Credential: "file"}
		// Validate before replacing a previous login.
		req, _ := http.NewRequest("GET", c.Server+"/api/v1/users/me/", nil)
		req.Header.Set("X-Api-Key", token)
		resp, e := client(c).Do(req)
		if e != nil {
			return e
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("login HTTP %d", resp.StatusCode)
		}
		var user struct {
			ID string `json:"id"`
		}
		if e = json.NewDecoder(resp.Body).Decode(&user); e != nil {
			return e
		}
		if user.ID == "" {
			return errors.New("server returned no user ID")
		}
		c.UserID = user.ID
		if _, e = os.Stat(filepath.Join(profileDir(), "config.json")); e == nil {
			return errors.New("profile already logged in; stop daemon and run auth logout before replacing it")
		}
		if e = keyring.Set("plane-cli", credentialKey(c), token); e == nil {
			c.Credential = "keyring"
		} else {
			fmt.Fprintln(os.Stderr, "System keyring unavailable; saving API key in a mode-0600 file")
			if e = os.WriteFile(filepath.Join(profileDir(), "token"), []byte(token), 0600); e != nil {
				return e
			}
		}
		if e = saveJSON(filepath.Join(profileDir(), "config.json"), c); e != nil {
			return e
		}
		return emit(map[string]string{"status": "logged_in", "server": c.Server, "user_id": c.UserID, "credential_store": c.Credential})
	}}
	login.Flags().StringVar(&server, "server", "", "Plane server origin (required)")
	login.MarkFlagRequired("server")
	login.Flags().StringVar(&keyFile, "api-key-file", "", "Read API key from a file, or - for stdin")
	auth.AddCommand(login, &cobra.Command{Use: "status", Short: "Check credentials and show the signed-in user", RunE: func(cmd *cobra.Command, args []string) error {
		c, e := loadConfig()
		if e != nil {
			return e
		}
		return call(c, "GET", "/api/v1/users/me/", "")
	}}, &cobra.Command{Use: "logout", Short: "Stop daemon and remove login, callbacks, and local history", RunE: func(cmd *cobra.Command, args []string) error {
		c, e := loadConfig()
		if e != nil {
			return e
		}
		if e := daemonStop(); e != nil {
			return fmt.Errorf("stop daemon before logout: %w", e)
		}
		_ = keyring.Delete("plane-cli", credentialKey(c))
		_ = os.Remove(filepath.Join(profileDir(), "token"))
		_ = os.Remove(filepath.Join(profileDir(), "config.json"))
		if e := os.RemoveAll(filepath.Join(profileDir(), "workspaces")); e != nil {
			return e
		}
		_ = os.Remove(filepath.Join(profileDir(), "workspace.json"))
		for _, name := range []string{"inbox.db", "inbox.db-wal", "inbox.db-shm"} {
			_ = os.Remove(filepath.Join(profileDir(), name))
		}
		return emit(map[string]bool{"logged_out": true})
	}})
	return auth
}
func apiCommand() *cobra.Command {
	var data string
	cmd := &cobra.Command{Use: "api METHOD /api/v1/path/", Short: "Call any public API, including query parameters and pagination", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		c, e := loadConfig()
		if e != nil {
			return e
		}
		if strings.HasPrefix(data, "@") {
			b, err := os.ReadFile(data[1:])
			if err != nil {
				return err
			}
			data = string(b)
		}
		return call(c, strings.ToUpper(args[0]), args[1], data)
	}}
	cmd.Flags().StringVar(&data, "data", "", "JSON body, or @file")
	return cmd
}
func resourceCommands(resource string) *cobra.Command {
	parent := &cobra.Command{Use: resource, Short: map[string]string{"projects": "Create and manage workspace projects", "work-items": "Read and manage tasks in a project"}[resource]}
	var project string
	parent.PersistentFlags().StringVar(&project, "project", "", "Project UUID (required for work-items)")
	for _, verb := range []string{"list", "get", "create", "update", "delete"} {
		verb := verb
		var data string
		cmd := &cobra.Command{Use: verb + " [id]", Short: map[string]string{"list": "List the current page", "get": "Get details by UUID", "create": "Create from --data JSON or @file", "update": "Update fields from --data JSON or @file", "delete": "Delete by UUID"}[verb], Args: func(cmd *cobra.Command, args []string) error {
			if verb == "get" || verb == "update" || verb == "delete" {
				return cobra.ExactArgs(1)(cmd, args)
			}
			return cobra.NoArgs(cmd, args)
		}, RunE: func(cmd *cobra.Command, args []string) error {
			c, e := loadConfig()
			if e != nil {
				return e
			}
			base, e := workspacePath(c)
			if e != nil {
				return e
			}
			if resource == "work-items" {
				if project == "" {
					return errors.New("--project is required")
				}
				base += "projects/" + url.PathEscape(project) + "/"
			}
			path := base + resource + "/"
			if len(args) > 0 {
				path += url.PathEscape(args[0]) + "/"
			}
			methods := map[string]string{"list": "GET", "get": "GET", "create": "POST", "update": "PATCH", "delete": "DELETE"}
			if strings.HasPrefix(data, "@") {
				b, err := os.ReadFile(data[1:])
				if err != nil {
					return err
				}
				data = string(b)
			}
			return call(c, methods[verb], path, data)
		}}
		if verb == "create" || verb == "update" {
			cmd.Flags().StringVar(&data, "data", "", "JSON body or @file")
			cmd.MarkFlagRequired("data")
		}
		parent.AddCommand(cmd)
	}
	return parent
}
func commentCommands() *cobra.Command {
	p := &cobra.Command{Use: "comments", Short: "Read or add task comments", Long: "Read comment_html or post literal HTML with --body. Embedded image-component src values\nare asset UUIDs: resolve them with plane api GET /api/v1/workspaces/SLUG/assets/UUID/.\nDownload the returned temporary asset_url. Local image upload/insertion into comments\nis not supported; attachments upload adds a task attachment. See the packaged README.md."}
	var project, issue, body string
	p.PersistentFlags().StringVar(&project, "project", "", "Project UUID")
	p.PersistentFlags().StringVar(&issue, "issue", "", "Work item UUID")
	for _, verb := range []string{"list", "add"} {
		verb := verb
		c := &cobra.Command{Use: verb, Short: map[string]string{"list": "List task comments", "add": "Post an HTML comment with --body"}[verb], Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
			cfg, e := loadConfig()
			if e != nil {
				return e
			}
			base, e := workspacePath(cfg)
			if e != nil {
				return e
			}
			if project == "" || issue == "" {
				return errors.New("--project and --issue are required")
			}
			method, data := "GET", ""
			if verb == "add" {
				method = "POST"
				b, _ := json.Marshal(map[string]string{"comment_html": body})
				data = string(b)
			}
			return call(cfg, method, base+"projects/"+url.PathEscape(project)+"/work-items/"+url.PathEscape(issue)+"/comments/", data)
		}}
		if verb == "add" {
			c.Long = "Post literal HTML using --body. Local image paths and @file are not read or uploaded.\nInline image upload/insertion into comments is not supported. Use attachments upload\nfor a task attachment, or the web editor for inline comment images."
			c.Example = "  plane comments add --project PROJECT_UUID --issue ISSUE_UUID --body '<p>Ready for review</p>'"
			c.Flags().StringVar(&body, "body", "", "Literal comment HTML; local paths and @file are not uploaded/read")
			c.MarkFlagRequired("body")
		} else {
			c.Long = "List comments including comment_html. For an embedded image-component, resolve its\nsrc asset UUID with plane api GET /api/v1/workspaces/SLUG/assets/UUID/, then download\nthe returned temporary asset_url. See the packaged README.md for a curl example."
		}
		p.AddCommand(c)
	}
	return p
}
func attachmentCommands() *cobra.Command {
	p := &cobra.Command{Use: "attachments", Short: "Upload, list, and download task files", Long: "Manage files in a task's attachment area with --project and --issue UUIDs.\nUploads do not insert images into task descriptions or comments. Embedded images\nare referenced in description_html/comment_html; resolve their asset UUID with\nplane api GET /api/v1/workspaces/SLUG/assets/UUID/ and download asset_url."}
	p.AddCommand(uploadCommand())
	var project, issue, output string
	p.PersistentFlags().StringVar(&project, "project", "", "Project UUID")
	p.PersistentFlags().StringVar(&issue, "issue", "", "Work item UUID")
	for _, verb := range []string{"list", "download"} {
		verb := verb
		cmd := &cobra.Command{Use: verb + " [attachment-id]", Short: map[string]string{"list": "List files attached to a task", "download": "Save an attachment to --output; never overwrite"}[verb], Args: func(cmd *cobra.Command, args []string) error {
			if verb == "download" {
				return cobra.ExactArgs(1)(cmd, args)
			}
			return cobra.NoArgs(cmd, args)
		}, RunE: func(cmd *cobra.Command, args []string) error {
			c, e := loadConfig()
			if e != nil {
				return e
			}
			base, e := workspacePath(c)
			if e != nil {
				return e
			}
			if project == "" || issue == "" {
				return errors.New("--project and --issue are required")
			}
			path := base + "projects/" + url.PathEscape(project) + "/work-items/" + url.PathEscape(issue) + "/attachments/"
			if verb == "list" {
				return call(c, "GET", path, "")
			}
			resp, e := apiRequest(context.Background(), c, "GET", path+url.PathEscape(args[0])+"/", nil)
			if e != nil {
				return e
			}
			defer resp.Body.Close()
			f, e := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if e != nil {
				return e
			}
			_, e = io.Copy(f, resp.Body)
			ce := f.Close()
			if e != nil {
				_ = os.Remove(output)
				return e
			}
			if ce != nil {
				return ce
			}
			return emit(map[string]string{"path": output})
		}}
		if verb == "download" {
			cmd.Flags().StringVar(&output, "output", "", "Destination file (must not exist)")
			cmd.MarkFlagRequired("output")
		}
		p.AddCommand(cmd)
	}
	return p
}
