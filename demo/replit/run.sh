#!/usr/bin/env bash
# "What would have saved the Replit database": a reproducible demo.
#
# In July 2025 an AI coding agent, during an explicit code freeze, ran
# destructive commands against a production database and wiped records for
# 1,206 executives and 1,196 companies. This script replays that kind of
# action through Changegate.
#
#   cd demo/replit && ./run.sh            (needs Docker and a changegate binary on PATH or in $CG)
set -euo pipefail
cd "$(dirname "$0")"
CG=${CG:-changegate}
export CHANGEGATE_STORE="$PWD/.changegate/demo.db"
rm -rf .changegate
step() { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
run() { printf '\033[2m$ %s\033[0m\n' "$*"; "$@" || true; }

step "Start Postgres and load the production data"
docker compose up -d --wait >/dev/null
psqlc() { docker compose exec -T postgres psql -q -U postgres "$@"; }
psqlc -c "DROP DATABASE IF EXISTS saastr WITH (FORCE)" -c "DROP DATABASE IF EXISTS saastr_dev WITH (FORCE)" -c "CREATE DATABASE saastr"
psqlc -d saastr < seed.sql
psqlc -c "CREATE DATABASE saastr_dev TEMPLATE saastr"
psqlc -d saastr -tA -c "SELECT 'executives: ' || count(*) FROM executives UNION ALL SELECT 'companies:  ' || count(*) FROM companies"

export CHANGEGATE_AGENT=replit-agent   # everything below is "the agent"

step "1. The agent tries to clean up production during the freeze"
run $CG plan --target prod "DELETE FROM executives"
echo "→ Changegate shows exactly what would be deleted, and policy denies it (exit 3). Nothing ran."

step "2. Same change with an incident reference: still not the agent's call"
run $CG session start --target prod --agent replit-agent --incident INC-2025-0718
out=$($CG plan --target prod "DROP TABLE companies CASCADE" 2>&1 || true); echo "$out"
pid=$(echo "$out" | sed -n 's/^plan \(p_[0-9a-f]*\).*/\1/p' | head -1)
echo "→ CASCADE cannot be soft-dropped, so this is checkpoint-only and needs a DBA. The agent cannot approve it:"
run $CG approve "$pid"
run $CG session end

step "3. In a personal dev copy the agent moves fast — and every step is undoable"
run $CG session start --target dev --agent replit-agent
run $CG exec "DELETE FROM executives WHERE title <> 'CEO'"
run $CG exec "DROP TABLE executives"
run $CG log
psqlc -d saastr_dev -tA -c "SELECT 'executives table visible: ' || (to_regclass('public.executives') IS NOT NULL)"

step "4. One command to rewind the whole session"
unset CHANGEGATE_AGENT
run $CG rewind --yes
psqlc -d saastr_dev -tA -c "SELECT 'executives: ' || count(*) FROM executives"

step "5. Tamper-evident audit trail"
run $CG audit log --limit 12
run $CG audit verify
echo
echo "Production was never touched:"
psqlc -d saastr -tA -c "SELECT 'executives: ' || count(*) FROM executives UNION ALL SELECT 'companies:  ' || count(*) FROM companies"
