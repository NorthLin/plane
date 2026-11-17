// Copyright (c) 2023-present Plane Software, Inc. and contributors
// SPDX-License-Identifier: AGPL-3.0-only
// See the LICENSE file for details.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func uploadCommand() *cobra.Command {
	var project, issue string
	cmd := &cobra.Command{Use: "upload FILE", Short: "Upload a task attachment and confirm completion", Long: "Upload FILE into the task attachment area and return asset_id after confirmation.\nRequires --project and --issue UUIDs. Does not insert the image into a task\ndescription or comment; inline image upload/insertion is not supported.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, e := loadConfig()
		if e != nil {
			return e
		}
		base, e := workspacePath(c)
		if e != nil {
			return e
		}
		if project == "" || issue == "" {
			return fmt.Errorf("--project and --issue are required")
		}
		f, e := os.Open(args[0])
		if e != nil {
			return e
		}
		defer f.Close()
		info, e := f.Stat()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("upload requires a regular file")
		}
		kind := attachmentMediaType(args[0])
		metadata, _ := json.Marshal(map[string]interface{}{"name": filepath.Base(args[0]), "type": kind, "size": info.Size()})
		path := base + "projects/" + url.PathEscape(project) + "/work-items/" + url.PathEscape(issue) + "/attachments/"
		resp, e := apiRequest(context.Background(), c, "POST", path, bytes.NewReader(metadata))
		if e != nil {
			return e
		}
		var result struct {
			AssetID string `json:"asset_id"`
			Upload  struct {
				URL    string            `json:"url"`
				Fields map[string]string `json:"fields"`
			} `json:"upload_data"`
		}
		e = json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if e != nil {
			return e
		}
		storageURL, e := url.Parse(result.Upload.URL)
		if e != nil || storageURL.Host == "" || (storageURL.Scheme != "http" && storageURL.Scheme != "https") {
			return fmt.Errorf("invalid upload URL")
		}
		reader, writer := io.Pipe()
		form := multipart.NewWriter(writer)
		go func() {
			for k, v := range result.Upload.Fields {
				if err := form.WriteField(k, v); err != nil {
					writer.CloseWithError(err)
					return
				}
			}
			part, err := form.CreateFormFile("file", filepath.Base(args[0]))
			if err == nil {
				_, err = io.Copy(part, f)
			}
			if err == nil {
				err = form.Close()
			}
			writer.CloseWithError(err)
		}()
		defer reader.Close()
		req, e := http.NewRequest("POST", storageURL.String(), reader)
		if e != nil {
			return e
		}
		req.Header.Set("Content-Type", form.FormDataContentType())
		req.ContentLength = info.Size() + int64(multipartOverhead(result.Upload.Fields, filepath.Base(args[0]), form.Boundary()))
		// Storage uploads are separate requests, with no Plane authorization headers.
		uploadClient := &http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
		uploaded, e := uploadClient.Do(req)
		if e != nil {
			return e
		}
		io.Copy(io.Discard, io.LimitReader(uploaded.Body, 8192))
		uploaded.Body.Close()
		if uploaded.StatusCode < 200 || uploaded.StatusCode >= 300 {
			return fmt.Errorf("storage HTTP %d", uploaded.StatusCode)
		}
		confirmed, e := apiRequest(context.Background(), c, "PATCH", path+url.PathEscape(result.AssetID)+"/", strings.NewReader("{}"))
		if e != nil {
			return e
		}
		confirmed.Body.Close()
		return emit(map[string]string{"asset_id": result.AssetID, "status": "uploaded"})
	}}
	cmd.Flags().StringVar(&project, "project", "", "Project UUID")
	cmd.Flags().StringVar(&issue, "issue", "", "Work item UUID")
	return cmd
}

func attachmentMediaType(path string) string {
	// Plane's allowed-type list contains bare MIME types, without charset parameters.
	kind, _, err := mime.ParseMediaType(mime.TypeByExtension(filepath.Ext(path)))
	if err != nil || kind == "" {
		return "application/octet-stream"
	}
	return kind
}

// Presigned POST storage rejects chunked uploads; compute the exact multipart size.
func multipartOverhead(fields map[string]string, name, boundary string) int {
	var b bytes.Buffer
	writer := multipart.NewWriter(&b)
	_ = writer.SetBoundary(boundary)
	for k, v := range fields {
		_ = writer.WriteField(k, v)
	}
	_, _ = writer.CreateFormFile("file", name)
	_ = writer.Close()
	return b.Len()
}
