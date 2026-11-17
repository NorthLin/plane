// Copyright (c) 2023-present Plane Software, Inc. and contributors
// SPDX-License-Identifier: AGPL-3.0-only
// See the LICENSE file for details.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var imagePlaceholder = regexp.MustCompile("\\{\\{image:([0-9]+)\\}\\}")

func imageCommentHTML(body string, ids []string) (string, error) {
	used := make(map[int]bool)
	var invalid bool
	body = imagePlaceholder.ReplaceAllStringFunc(body, func(marker string) string {
		n, err := strconv.Atoi(imagePlaceholder.FindStringSubmatch(marker)[1])
		if err != nil || n < 1 || n > len(ids) {
			invalid = true
			return marker
		}
		used[n-1] = true
		return commentImageNode(ids[n-1])
	})
	if invalid || strings.Contains(body, "{{image:") {
		return "", fmt.Errorf("image placeholder must be {{image:N}} with N between 1 and %d", len(ids))
	}
	for i, id := range ids {
		if !used[i] {
			body += commentImageNode(id)
		}
	}
	return body, nil
}

func commentImageNode(id string) string {
	return fmt.Sprintf("<image-component src=%q status=\"uploaded\" width=\"100%%\" height=\"auto\" alignment=\"left\"></image-component>", id)
}

func addImageComment(c Config, base, project, issue, body string, files []string) error {
	// Validate every file and position before uploading or posting a comment.
	if _, err := imageCommentHTML(body, make([]string, len(files))); err != nil {
		return err
	}
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("image must be a nonempty regular file: %s", file)
		}
		switch attachmentMediaType(file) {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
		default:
			return fmt.Errorf("unsupported comment image: %s (use PNG, JPEG, GIF or WebP)", file)
		}
	}
	// Confirm the task is visible before creating any upload resources.
	taskPath := base + "projects/" + url.PathEscape(project) + "/work-items/" + url.PathEscape(issue) + "/"
	resp, err := apiRequest(context.Background(), c, "GET", taskPath, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	ids := make([]string, 0, len(files))
	for _, file := range files {
		id, err := uploadFile(c, base+"assets/", file, project, true)
		if err != nil {
			return fmt.Errorf("image upload failed; comment was not posted (uploaded asset IDs: %v): %w", ids, err)
		}
		ids = append(ids, id)
	}
	content, err := imageCommentHTML(body, ids)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]interface{}{"comment_html": content, "asset_ids": ids})
	if err := call(c, "POST", taskPath+"comments/", string(data)); err != nil {
		return fmt.Errorf("comment creation failed (uploaded asset IDs: %v); check comments before retrying: %w", ids, err)
	}
	return nil
}
