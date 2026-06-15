// Drain benchmark (Postgres only): pre-load N runnable, call-less instances directly
// into the table, then time how fast the engine drains them to terminal. This isolates
// claim-bound throughput under a backlog — the scenario the partial runnable index
// (migration 010) targets, which the spawn bench (bench.ts) never exercises because it
// keeps the engine fed. Postgres-specific (uses generate_series), and it DROPs/recreates
// the target DB's public schema so migrations (incl. the index set under test) apply
// from scratch.
//
//   POSTGRES_DSN=postgres://gent:gent@localhost:5432/gent_test?sslmode=disable \
//     BENCH_DRAIN_N=50000 bun run bench/drain.ts
//
// Tunables (env): BENCH_DRAIN_N, BENCH_POLL_MS, BENCH_MAX_CONCURRENT, BENCH_PORT,
// BENCH_DRAIN_TIMEOUT, BENCH_JSON.

import { SQL } from "bun";
import { writeFileSync } from "node:fs";
import { buildGentBinary, startGent } from "../helpers/server.ts";

if (!process.env.POSTGRES_DSN) {
  console.error(
    "set POSTGRES_DSN, e.g. postgres://gent:gent@localhost:5432/gent_test?sslmode=disable",
  );
  process.exit(1);
}
const DSN: string = process.env.POSTGRES_DSN;

const N = num("BENCH_DRAIN_N", 50_000);
const POLL_MS = num("BENCH_POLL_MS", 10);
const MAX_CONCURRENT = num("BENCH_MAX_CONCURRENT", 200);
const PORT = num("BENCH_PORT", 8893);
const TIMEOUT_S = num("BENCH_DRAIN_TIMEOUT", 600);

// step_queue of a trivial call-less instance: one noop step routing straight to end,
// so each prefilled instance completes on a single advance.
const STEP_QUEUE = '[{"id":"noop","switch":[{"goto":"end"}]}]';

function num(name: string, def: number): number {
  const v = process.env[name];
  if (v === undefined) return def;
  const n = Number(v);
  if (!Number.isFinite(n)) throw new Error(`${name} must be a number, got ${v}`);
  return n;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

// Count instances still runnable — mirrors the partial runnable index predicate.
async function runnableCount(sql: SQL): Promise<number> {
  const rows = await sql`
    SELECT count(*)::int AS n FROM process_instances
    WHERE status IN ('running', 'failing', 'cancelling') AND wait_state <> 'waiting'
  `;
  return rows[0].n as number;
}

async function main() {
  const sql = new SQL(DSN);

  // Fresh schema so migrations apply from scratch (two statements: DDL can't be
  // batched through the prepared-statement protocol Bun uses).
  await sql`DROP SCHEMA public CASCADE`;
  await sql`CREATE SCHEMA public`;

  const bin = await buildGentBinary();
  const server = await startGent(bin, PORT, "", DSN, POLL_MS, MAX_CONCURRENT);
  try {
    // A trivial call-less definition so prefilled instances complete on one advance.
    const { error } = await server.client.PUT("/definitions", {
      body: {
        name: "drain",
        steps: [{ id: "noop", switch: [{ goto: "end" }] }],
      } as never,
    });
    if (error) throw new Error(`register failed: ${JSON.stringify(error)}`);

    // Prefill N runnable, call-less instances in one statement — bypasses the spawn
    // path so we measure pure claim+advance throughput under a backlog. Spread
    // created_at by g so they don't all collide on the same millisecond.
    await sql`
      INSERT INTO process_instances
        (id, process_name, process_version, step_queue, context_data,
         status, wait_state, created_at, updated_at)
      SELECT gen_random_uuid()::text, 'drain', 1, ${STEP_QUEUE}, '{}',
             'running', '', 1700000000000 + g, 1700000000000
      FROM generate_series(1, ${N}) g
    `;

    const start = Date.now();
    const deadline = start + TIMEOUT_S * 1000;
    let remaining = N;
    while (remaining > 0) {
      if (Date.now() > deadline) {
        throw new Error(
          `drain: TIMEOUT after ${TIMEOUT_S}s with ${remaining} still runnable`,
        );
      }
      await sleep(200);
      remaining = await runnableCount(sql);
    }
    const elapsedS = (Date.now() - start) / 1000;
    const thr = Math.round(N / (elapsedS || 1));

    console.log(
      `\ndrain: N=${N} drained in ${elapsedS.toFixed(1)}s (~${thr} inst/s) ` +
        `[poll=${POLL_MS}ms concurrency=${MAX_CONCURRENT}]`,
    );

    // Optional github-action-benchmark customBiggerIsBetter entry for CI history.
    const jsonPath = process.env.BENCH_JSON;
    if (jsonPath) {
      writeFileSync(
        jsonPath,
        JSON.stringify(
          [{ name: `drain postgres N${N}`, unit: "inst/s", value: thr }],
          null,
          2,
        ),
      );
    }
  } finally {
    server.stop();
    await sql.end();
  }
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
