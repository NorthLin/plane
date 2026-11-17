Plane v1.5.1 — server and CLI release

Server images, CLI archives and deployment configuration now share the stable version v1.5.1. This replaces the ambiguous v1.5.0-cli.4 release name without changing its features. Existing v1.5.0-cli.4 deployments remain compatible.

New: plane comments add --image ./screenshot.png --project PROJECT_UUID --issue ISSUE_UUID

Repeat --image for multiple PNG/JPEG/GIF/WebP files. Add --body HTML for text; images append in order, or use {{image:1}} and {{image:2}} between HTML blocks to choose positions. Image-only comments are supported.

Images upload as comment resources and bind atomically when the comment is created. The CLI checks file paths and positions before uploading. Failed uploads do not post a comment. Error messages report already uploaded asset IDs; check comments before retrying a failed post. Unbound uploads are not automatically removed.

Comment image uploads require Plane server v1.5.1 or the previous v1.5.0-cli.4 images. Earlier servers give an upgrade error. No database migration is needed. This release also includes all six server images on Docker Hub, with amd64/arm64 variants.

Homebrew: brew update && brew upgrade plane-cli
First install: brew install NorthLin/tap/plane-cli
Direct archives cover macOS/Linux on Intel and ARM, with SHA256SUMS and install.sh.

Server upgrade: back up the existing deployment, set APP_RELEASE=v1.5.1 in .env, then docker compose pull && docker compose up -d. Preserve all existing secrets and volumes.

Task description insertion is not added; task attachment upload/download commands continue to work.
