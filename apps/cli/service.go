// Copyright (c) 2023-present Plane Software, Inc. and contributors
// SPDX-License-Identifier: AGPL-3.0-only
// See the LICENSE file for details.

package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
func systemdQuote(s string) string { return strconv.Quote(strings.ReplaceAll(s, "%", "%%")) }
func serviceCommand() *cobra.Command {
	return &cobra.Command{Use: "install", Short: "Install and start a login service (launchd / systemd --user)", RunE: func(cmd *cobra.Command, args []string) error {
		if _, e := loadConfig(); e != nil {
			return e
		}
		exe, e := os.Executable()
		if e != nil {
			return e
		}
		dir, e := filepath.Abs(profileDir())
		if e != nil {
			return e
		}
		home, e := os.UserHomeDir()
		if e != nil {
			return e
		}
		// The built executable must stay at this path after installation.
		switch runtime.GOOS {
		case "linux":
			folder := filepath.Join(home, ".config", "systemd", "user")
			if e = os.MkdirAll(folder, 0700); e != nil {
				return e
			}
			unit := "[Unit]\nDescription=Plane event daemon\nAfter=network-online.target\n[Service]\nExecStart=" + systemdQuote(exe) + " --config-dir " + systemdQuote(dir) + " daemon run\nRestart=on-failure\nRestartSec=5\n[Install]\nWantedBy=default.target\n"
			if e = os.WriteFile(filepath.Join(folder, "plane-daemon.service"), []byte(unit), 0600); e != nil {
				return e
			}
			if b, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
				return fmt.Errorf("systemd user manager: %s: %w", b, err)
			}
			if e = daemonStop(); e != nil {
				return e
			}
			if b, err := exec.Command("systemctl", "--user", "enable", "--now", "plane-daemon.service").CombinedOutput(); err != nil {
				return fmt.Errorf("%s: %w", b, err)
			}
		case "darwin":
			folder := filepath.Join(home, "Library", "LaunchAgents")
			if e = os.MkdirAll(folder, 0700); e != nil {
				return e
			}
			path := filepath.Join(folder, "so.plane.daemon.plist")
			plist := "<?xml version=\"1.0\" encoding=\"UTF-8\"?><!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\"><plist version=\"1.0\"><dict><key>Label</key><string>so.plane.daemon</string><key>ProgramArguments</key><array><string>" + xmlText(exe) + "</string><string>--config-dir</string><string>" + xmlText(dir) + "</string><string>daemon</string><string>run</string></array><key>RunAtLoad</key><true/><key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict><key>ThrottleInterval</key><integer>5</integer><key>StandardErrorPath</key><string>" + xmlText(filepath.Join(dir, "daemon.log")) + "</string></dict></plist>"
			if e = os.WriteFile(path, []byte(plist), 0600); e != nil {
				return e
			}
			if e = daemonStop(); e != nil {
				return e
			}
			domain := "gui/" + strconv.Itoa(os.Getuid())
			_ = exec.Command("launchctl", "bootout", domain, path).Run()
			if b, err := exec.Command("launchctl", "bootstrap", domain, path).CombinedOutput(); err != nil {
				return fmt.Errorf("%s: %w", b, err)
			}
		default:
			return errors.New("services support macOS and Linux")
		}
		return emit(map[string]bool{"installed": true})
	}}
}
