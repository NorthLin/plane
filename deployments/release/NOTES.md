Plane v1.5.3 — live work item updates

Fixes a cold-load failure in v1.5.2: versioned JavaScript assets no longer consume the page-request rate limit, preventing HTTP 429 errors during normal browser startup.

Web pages now receive project events over an authenticated WebSocket and update task details, comments, activity and lists without a manual refresh. Changes from CLI/API clients using the same account are included. Comment images continue to use the existing upload and asset endpoints.

Based on community PR makeplane/plane#10244 by Austin Norgaard, with fixes for short disconnects, offline comment edits/deletions, subscription recovery, paginated list totals and graceful shutdown. Project membership and session checks protect subscriptions. Guest subscriptions are excluded in this version.

Root docker-compose.yml enables LIVE_EVENTS_ENABLED=1 for the API, worker and live service. Upgrade all images together and ensure your reverse proxy forwards WebSocket upgrades on /live/events. No new database migration is required. Existing CLI callbacks are unchanged.

Back up your deployment, set APP_RELEASE=v1.5.3 in the existing .env, then run docker compose pull && docker compose up -d. Keep existing secrets, Compose project name and volumes. See DEPLOYMENT.md.

All six Docker Hub images include amd64/arm64 variants. CLI archives cover macOS/Linux on Intel and ARM. Install with brew install NorthLin/tap/plane-cli or use install.sh from this release; existing users can brew update && brew upgrade plane-cli.
