#!/bin/sh
# Run the production data-plane contract against disposable loopback services.
set -eu
cd "$(dirname "$0")/.."
project="agentmoor-test-$$"
if docker compose version >/dev/null 2>&1; then
  compose() { docker compose -p "$project" -f tests/services.compose.yml "$@"; }
else
  compose() { docker-compose -p "$project" -f tests/services.compose.yml "$@"; }
fi
trap 'compose down --volumes --remove-orphans' EXIT
compose up -d --wait
pg_address=$(compose port postgres 5432)
s3_address=$(compose port s3 9000)
AGENTMOOR_TEST_PG="postgres://postgres:disposable-test-password@$pg_address/agentmoor?sslmode=disable" \
AGENTMOOR_TEST_S3="$s3_address" go test -tags integration -count=1 -run '^TestPostgresAndS3$' ./internal/platform
