#!/usr/bin/env bash
# Generate configuration once; never overwrite an existing deployment's secrets.
set -euo pipefail
cd "$(dirname "$0")"
namespace=${DOCKERHUB_NAMESPACE:-fonzy666}
version=${2:-v1.5.0-cli.2}
url=${1:?Usage: configure.sh PUBLIC_URL [VERSION]}
[[ "$namespace" =~ ^[a-z0-9][a-z0-9_-]+$ ]] || { echo 'Invalid Docker Hub username' >&2; exit 1; }
[[ "$version" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]] || { echo 'Invalid version' >&2; exit 1; }
[[ "$url" =~ ^https?://[a-zA-Z0-9.:-]+$ ]] || { echo 'Use an http(s) origin without a trailing slash' >&2; exit 1; }
test ! -e .env || { echo '.env already exists; edit it to upgrade without replacing secrets.' >&2; exit 1; }
umask 077
set -o noclobber
{
  echo "DOCKERHUB_NAMESPACE=$namespace"
  echo "APP_RELEASE=$version"
  echo "WEB_URL=$url"
  echo "APP_DOMAIN=${url#*://}"
  echo "CORS_ALLOWED_ORIGINS=$url"
  echo 'API_BASE_URL=http://api:8000'
  echo 'SITE_ADDRESS=:80'
  echo 'LISTEN_HTTP_PORT=80'
  echo 'NUM_PROXIES=1'
  echo 'CERT_EMAIL='
  echo 'CERT_ACME_CA=https://acme-v02.api.letsencrypt.org/directory'
  echo 'CERT_ACME_DNS='
  echo 'TRUSTED_PROXIES=private_ranges'
  echo 'POSTGRES_PASSWORD='"$(openssl rand -hex 24)"
  echo 'RABBITMQ_PASSWORD='"$(openssl rand -hex 24)"
  echo 'SECRET_KEY='"$(openssl rand -hex 32)"
  echo 'LIVE_SERVER_SECRET_KEY='"$(openssl rand -hex 32)"
  echo 'AWS_ACCESS_KEY_ID='"$(openssl rand -hex 16)"
  echo 'AWS_SECRET_ACCESS_KEY='"$(openssl rand -hex 32)"
} > .env
echo 'Created .env with random secrets. Review it, then run docker compose up -d.'
