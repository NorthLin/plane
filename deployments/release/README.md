# Deploy the callback-enabled Plane fork

Requires Docker Engine with Compose v2 on Linux amd64 or arm64. Download this
directory from the same release tag as the images; keep docker-compose.yml and
configure.sh together. All six Plane images use your personal Docker Hub account.

    bash configure.sh YOUR_DOCKERHUB_USERNAME v1.5.0-cli.2 https://plane.example.com
    docker compose pull
    docker compose up -d
    docker compose logs -f migrator api

The generator creates a mode-0600 .env with independent random database, queue,
application and storage secrets. It refuses to overwrite existing configuration.
Back up .env and Docker volumes together. The compose stack includes PostgreSQL,
Valkey, RabbitMQ and S3-compatible storage. It publishes only proxy ports 80/443.

For a local HTTP trial use http://localhost. For HTTPS either set SITE_ADDRESS to
your domain and CERT_EMAIL to your email (Caddy obtains certificates on ports
80/443), or put your own TLS proxy in front and set TRUSTED_PROXIES to its network.
WEB_URL and CORS_ALLOWED_ORIGINS must match the URL users and the CLI actually use.

Open /god-mode/ to configure the instance. Then log the CLI in with the same public
origin, e.g. plane auth login --server https://plane.example.com --workspace personal.
No API or daemon port needs to be exposed separately.

To upgrade, back up first, change APP_RELEASE in .env to a published version, then
run docker compose pull && docker compose up -d. The migrator applies the database
migrations, including the CLI event table. Keep all six images on the same version.

Images: plane-backend, plane-frontend, plane-admin, plane-space, plane-live and
plane-proxy. Each published tag has linux/amd64 and linux/arm64 variants.
GitHub Actions publishes version tags, then starts this isolated stack and runs the
CLI callback/upload/download integration test through the bundled proxy.
