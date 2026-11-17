Plane CLI v1.5.0-cli.3

Clarifies image and file handling in the bundled README and command help:

- Upload/download task attachments with workspace, project and work item selection.
- Find images in task description_html and comment_html, resolve asset UUIDs and download temporary URLs.
- Explain that comment --body accepts literal HTML; local file paths and @file do not upload images.
- Explain that inline image upload/insertion into descriptions and comments is not currently supported.

Install or upgrade: brew update && brew upgrade plane-cli
First install: brew install NorthLin/tap/plane-cli
Direct downloads: macOS/Linux on Intel and ARM, with SHA256SUMS and install.sh.

This release updates CLI documentation and help. It does not add inline image upload support.
Plane server images remain v1.5.0-cli.2. The included deployment configuration uses fonzy666 by default.
CLI release tags do not automatically rebuild server images; use Publish images manually for server releases.
