Plane with local agent callbacks.

Download the CLI archive for your OS and CPU, verify it against SHA256SUMS, then place the plane binary on PATH. Includes macOS/Linux on amd64 and arm64.

Quick install: download apps/cli/install.sh from this release tag, inspect it, then run bash install.sh. It verifies the archive checksum and installs to ~/.local/bin.

Run plane --help to get started. Login uses your public Plane URL and an API key; callbacks receive event JSON on stdin.

Docker deployment instructions: deployments/release/README.md in the source tree. Docker images are published separately by the Publish images workflow.
