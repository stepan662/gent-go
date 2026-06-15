#!/usr/bin/env bash
# Drain benchmark (Postgres only): pre-load N runnable, call-less instances directly
# into the table, then time how fast the engine drains them to terminal. This
# isolates claim-bound throughput under a backlog — the scenario the partial
# runnable index (migration 010) targets, which the spawn bench (`make bench`)
# never exercises because it keeps the engine fed. Uses generate_series, so it's
# Postgres-specific, and it DROPs/recreates the target DB's public schema.
#
#   POSTGRES_DSN=postgres://gent:gent@localhost:5432/gent_test?sslmode=disable \
#     BENCH_DRAIN_N=50000 scripts/bench-drain.sh
set -euo pipefail

: "${POSTGRES_DSN:?set POSTGRES_DSN, e.g. postgres://gent:gent@localhost:5432/gent_test?sslmode=disable}"
N="${BENCH_DRAIN_N:-50000}"
POLL_MS="${BENCH_POLL_MS:-10}"
MAX_CONCURRENT="${BENCH_MAX_CONCURRENT:-200}"
PORT="${BENCH_PORT:-8893}"
TIMEOUT="${BENCH_DRAIN_TIMEOUT:-600}"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
BIN="$(mktemp -t gent-drainbench.XXXXXX)"

cleanup() { [ -n "${SRV:-}" ] && kill "$SRV" 2>/dev/null || true; rm -f "$BIN"; }
trap cleanup EXIT

# Fresh schema so migrations (incl. the index set under test) apply from scratch.
psql "$POSTGRES_DSN" -q -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;" >/dev/null
CGO_ENABLED=1 go build -tags "sqlite_omit_load_extension" -o "$BIN" ./cmd/gent

"$BIN" --pg "$POSTGRES_DSN" --http ":$PORT" --poll "$POLL_MS" --max-concurrent "$MAX_CONCURRENT" --log error &
SRV=$!
for _ in $(seq 1 100); do curl -sf "http://localhost:$PORT/openapi.json" >/dev/null 2>&1 && break; sleep 0.2; done

# A trivial call-less definition so prefilled instances complete on a single advance.
curl -s -X PUT "http://localhost:$PORT/definitions" -H 'content-type: application/json' \
  -d '{"name":"drain","steps":[{"id":"noop","switch":[{"goto":"end"}]}]}' >/dev/null

psql "$POSTGRES_DSN" -q -c "INSERT INTO process_instances (id,process_name,process_version,step_queue,context_data,status,wait_state,created_at,updated_at) SELECT gen_random_uuid()::text,'drain',1,'[{\"id\":\"noop\",\"switch\":[{\"goto\":\"end\"}]}]','{}','running','',1700000000000+g,1700000000000 FROM generate_series(1,$N) g;"

SECONDS=0
while :; do
  n=$(psql "$POSTGRES_DSN" -tAc "SELECT count(*) FROM process_instances WHERE status IN ('running','failing','cancelling') AND wait_state <> 'waiting';")
  [ "$n" -eq 0 ] && break
  if [ "$SECONDS" -gt "$TIMEOUT" ]; then echo "drain: TIMEOUT after ${SECONDS}s with $n still runnable" >&2; exit 1; fi
  sleep 0.2
done
THR=$(( N / (SECONDS == 0 ? 1 : SECONDS) ))
echo "drain: N=$N drained in ${SECONDS}s (~${THR} inst/s) [poll=${POLL_MS}ms concurrency=${MAX_CONCURRENT}]"

# Optional github-action-benchmark customBiggerIsBetter entry for CI history.
if [ -n "${BENCH_JSON:-}" ]; then
  printf '[{"name":"drain postgres N%s","unit":"inst/s","value":%s}]\n' "$N" "$THR" > "$BENCH_JSON"
fi
