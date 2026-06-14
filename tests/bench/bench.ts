// Performance benchmark: a recursive process that spawns children until its TTL
// runs out, producing a deterministic tree of 2^(ttl+1)-1 instances per root.
// Pure orchestration (no external HTTP calls), so it isolates engine + DB
// throughput and lets us compare SQLite (single writer) vs Postgres (concurrent
// workers).
//
//   make bench                         # SQLite only
//   POSTGRES_DSN=postgres://… make bench   # SQLite + Postgres comparison
//
// Tunables (env): BENCH_TTL, BENCH_ROOTS, BENCH_POLL_MS, BENCH_MAX_CONCURRENT,
// BENCH_RUNS.

import { join } from "node:path";
import { tmpdir } from "node:os";
import {
  buildGentBinary,
  startGent,
  type GentProcess,
} from "../helpers/server.ts";

const TTL = num("BENCH_TTL", 12);
const ROOTS = num("BENCH_ROOTS", 1);
const POLL_MS = num("BENCH_POLL_MS", 10);
const MAX_CONCURRENT = num("BENCH_MAX_CONCURRENT", 200);
const RUNS = num("BENCH_RUNS", 1);
const BENCH_PORT = 8890; // distinct from the test servers (8888 sqlite, 8889 pg)
const BENCH_ENGINES = process.env.BENCH_ENGINES ?? "sqlite,postgres";

// Each node spawns 2 children, so a root with ttl=N expands to 2^(N+1)-1 nodes.
const INSTANCES_PER_ROOT = 2 ** (TTL + 1) - 1;
const TOTAL_INSTANCES = ROOTS * INSTANCES_PER_ROOT;

function num(name: string, def: number): number {
  const v = process.env[name];
  if (v === undefined) return def;
  const n = Number(v);
  if (!Number.isFinite(n))
    throw new Error(`${name} must be a number, got ${v}`);
  return n;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

// The recursive child_parallel definition: spawns `first` and `second` with
// ttl-1 until ttl hits 0, and sums the subtree node count into output.processes.
function recursiveDef(name: string) {
  const childSpec = {
    name,
    input: { ttl: "{{input.ttl - 1}}" },
    output_schema: {
      type: "object",
      properties: { processes: { type: "number" } },
      required: ["processes"],
    },
  };
  return {
    name,
    input_schema: {
      type: "object",
      properties: { ttl: { type: "integer" } },
      required: ["ttl"],
    },
    steps: [
      {
        id: "recursion_condition",
        switch: [
          { case: "input.ttl > 0", goto: "$recursion" },
          { goto: "end" },
        ],
      },
      {
        id: "recursion",
        call: {
          type: "child_parallel" as const,
          children: { first: childSpec, second: childSpec },
        },
        switch: [{ goto: "end" }],
      },
    ],
    output: {
      processes:
        "{{(outputs.recursion.first.processes ?? 0) + (outputs.recursion.second.processes ?? 0) + 1}}",
    },
  };
}

type Client = GentProcess["client"];

// Poll until terminal; returns the final instance status payload.
async function waitDone(client: Client, id: string, timeoutMs: number) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const { data, error } = await client.GET("/instances/{id}", {
      params: { path: { id } },
    });
    if (error) throw new Error(`get_instance failed: ${JSON.stringify(error)}`);
    const status = data?.status;
    if (
      status === "completed" ||
      status === "failed" ||
      status === "cancelled"
    ) {
      return data!;
    }
    await sleep(POLL_MS);
  }
  throw new Error(`instance ${id} did not finish within ${timeoutMs}ms`);
}

// Run the workload once against an already-running server; returns duration in ms.
async function runOnce(client: Client, label: string): Promise<number> {
  const name = `bench_${label}_${crypto.randomUUID().replace(/-/g, "").slice(0, 8)}`;
  const { error: defErr } = await client.PUT("/definitions", {
    body: recursiveDef(name) as never,
  });
  if (defErr) throw new Error(`register failed: ${JSON.stringify(defErr)}`);

  // Generous timeout: scales with tree size and poll interval.
  const timeoutMs = Math.max(30_000, INSTANCES_PER_ROOT * 20);

  const start = Date.now();
  const ids: string[] = [];
  for (let i = 0; i < ROOTS; i++) {
    const { data, error } = await client.POST("/instances", {
      body: { process: name, input: { ttl: TTL } } as never,
    });
    if (error) throw new Error(`start failed: ${JSON.stringify(error)}`);
    ids.push(data!.id);
  }

  const finals = await Promise.all(
    ids.map((id) => waitDone(client, id, timeoutMs)),
  );
  const elapsed = Date.now() - start;

  // Correctness: each root computes its own subtree node count.
  for (const f of finals) {
    if (f.status !== "completed") {
      throw new Error(`root ${f.id} ended ${f.status}: ${f.error ?? ""}`);
    }
    const got = (f.context?.output as { processes?: number } | undefined)
      ?.processes;
    if (got !== INSTANCES_PER_ROOT) {
      throw new Error(
        `root ${f.id}: expected output.processes=${INSTANCES_PER_ROOT}, got ${got}`,
      );
    }
  }
  return elapsed;
}

interface EngineResult {
  engine: string;
  durations: number[];
}

async function benchEngine(
  engine: string,
  dbPath: string,
  dsn?: string,
): Promise<EngineResult> {
  const bin = await buildGentBinary();
  const server = await startGent(
    bin,
    BENCH_PORT,
    dbPath,
    dsn,
    POLL_MS,
    MAX_CONCURRENT,
  );
  try {
    const durations: number[] = [];
    for (let run = 0; run < RUNS; run++) {
      durations.push(await runOnce(server.client, engine));
    }
    return { engine, durations };
  } finally {
    server.stop();
    await sleep(200); // let the process release the port before the next engine
  }
}

function fmt(n: number, w: number) {
  return String(n).padStart(w);
}

function throughput(ms: number) {
  return Math.round((TOTAL_INSTANCES / ms) * 1000);
}

function report(results: EngineResult[]) {
  console.log(
    "\nconfig: " +
      `ttl=${TTL} roots=${ROOTS} instances=${TOTAL_INSTANCES} ` +
      `poll_ms=${POLL_MS} max_concurrent=${MAX_CONCURRENT} runs=${RUNS}\n`,
  );

  console.log(
    "engine".padEnd(10) +
      "runs".padStart(6) +
      "instances".padStart(11) +
      "min_ms".padStart(9) +
      "avg_ms".padStart(9) +
      "max_ms".padStart(9) +
      "thrpt(inst/s)".padStart(15),
  );

  const avgThr: Record<string, number> = {};
  for (const r of results) {
    const min = Math.min(...r.durations);
    const max = Math.max(...r.durations);
    const avg = Math.round(
      r.durations.reduce((a, b) => a + b, 0) / r.durations.length,
    );
    avgThr[r.engine] = throughput(avg);
    console.log(
      r.engine.padEnd(10) +
        fmt(r.durations.length, 6) +
        fmt(TOTAL_INSTANCES, 11) +
        fmt(min, 9) +
        fmt(avg, 9) +
        fmt(max, 9) +
        fmt(throughput(avg), 15),
    );
  }

  if (avgThr.sqlite && avgThr.postgres) {
    const ratio = (avgThr.postgres / avgThr.sqlite).toFixed(2);
    console.log(`\npostgres/sqlite throughput ratio: ${ratio}x`);
  }
}

async function main() {
  const results: EngineResult[] = [];

  // BENCH_ENGINES selects which engines to run (comma-separated), e.g.
  // BENCH_ENGINES=postgres to skip the slow SQLite pass when tuning Postgres.
  const engines = BENCH_ENGINES.split(",")
    .map((s) => s.trim().toLowerCase())
    .filter(Boolean);

  if (engines.includes("sqlite")) {
    const sqliteDb = join(tmpdir(), `gent_bench_${Date.now()}.db`);
    results.push(await benchEngine("sqlite", sqliteDb, undefined));
  }

  if (engines.includes("postgres")) {
    const dsn = process.env.POSTGRES_DSN;
    if (dsn) {
      results.push(await benchEngine("postgres", "", dsn));
    } else {
      console.log(
        "\n(POSTGRES_DSN not set — skipping postgres; set it to compare)",
      );
    }
  }

  report(results);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
