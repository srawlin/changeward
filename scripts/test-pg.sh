#!/usr/bin/env bash
# Start the Postgres servers used by the integration tests:
#   PG18 on :55418 (RETURNING OLD/NEW path) and PG16 on :55416 (pre-select fallback).
# Tests read CG_TEST_PG18 / CG_TEST_PG16 and skip servers that are not reachable.
set -euo pipefail
start() {
  local name=$1 image=$2 port=$3
  if docker ps --format '{{.Names}}' | grep -qx "$name"; then return; fi
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker run -d --name "$name" -e POSTGRES_PASSWORD=pg -p "$port:5432" "$image" >/dev/null
}
start cg-pg18 postgres:18-alpine 55418
start cg-pg16 postgres:16-alpine 55416
for port in 55418 55416; do
  for _ in $(seq 1 30); do
    if docker run --rm --network host postgres:18-alpine pg_isready -h localhost -p "$port" >/dev/null 2>&1 \
       || (command -v pg_isready >/dev/null && pg_isready -h localhost -p "$port" >/dev/null 2>&1); then break; fi
    sleep 1
  done
done
echo "export CG_TEST_PG18=postgres://postgres:pg@localhost:55418/postgres?sslmode=disable"
echo "export CG_TEST_PG16=postgres://postgres:pg@localhost:55416/postgres?sslmode=disable"
