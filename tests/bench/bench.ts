// Performance benchmark: a recursive process that spawns children until its TTL
// runs out. Pure orchestration (no external HTTP calls), so it isolates engine +
// DB throughput and lets us compare SQLite (single writer) vs Postgres (concurrent
// workers).
//
//   make bench                              # SQLite only
//   POSTGRES_DSN=postgres://… make bench    # SQLite + Postgres comparison
//
// Two workload shapes (BENCH_MODE):
//   simple (default) — full binary tree, every node spawns 2 children until ttl=0
//                      (2^(ttl+1)-1 nodes). WIDE and shallow; the default workload.
//   deep             — mostly-linear tree that splits every BENCH_SPLIT levels, so
//                      depth ≈ ttl with a bounded node count. Probes how nesting
//                      DEPTH (not width) affects per-spawn cost. SQLite needs a
//                      lower BENCH_MAX_CONCURRENT here (e.g. 20) to avoid overwhelm.
//
// Tunables (env): BENCH_MODE, BENCH_TTL, BENCH_SPLIT (deep only), BENCH_ROOTS,
// BENCH_POLL_MS, BENCH_MAX_CONCURRENT, BENCH_RUNS.

import { join } from "node:path";
import { tmpdir } from "node:os";
import { writeFileSync } from "node:fs";
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
const MODE = process.env.BENCH_MODE ?? "simple"; // "simple" (binary) | "deep"
const DEEP = MODE === "deep";
// deep mode only: the tree splits (spawns 2) every SPLIT levels, otherwise descends
// (spawns 1). Larger SPLIT → deeper, narrower trees.
const SPLIT = num("BENCH_SPLIT", 10);
const BENCH_PORT = 8890; // distinct from the test servers (8888 sqlite, 8889 pg)
const BENCH_ENGINES = process.env.BENCH_ENGINES ?? "sqlite,postgres";

// simple mode: full binary tree of 2^(ttl+1)-1 nodes.
const binarySize = (ttl: number) => 2 ** (ttl + 1) - 1;

// deep mode: node count for the countdown pattern, memoised over (ttl, c).
const deepMemo = new Map<string, number>();
function deepSize(ttl: number, c: number): number {
  if (ttl <= 0) return 1;
  const key = `${ttl},${c}`;
  const hit = deepMemo.get(key);
  if (hit !== undefined) return hit;
  const n =
    c <= 1
      ? 1 + 2 * deepSize(ttl - 1, SPLIT) // split, children reset countdown
      : 1 + deepSize(ttl - 1, c - 1); // descend
  deepMemo.set(key, n);
  return n;
}

const INSTANCES_PER_ROOT = DEEP ? deepSize(TTL, SPLIT) : binarySize(TTL);
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

const outputSchema = {
  type: "object",
  properties: { processes: { type: "number" } },
  required: ["processes"],
};

// simple mode: the original full binary tree — every node spawns `first` and
// `second` with ttl-1 until ttl hits 0, summing the subtree node count.
function simpleDef(name: string) {
  const childSpec = {
    name,
    input: { ttl: "{{input.ttl - 1}}" },
    output_schema: outputSchema,
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

// deep mode: a mostly-linear tree. Each node carries a countdown `c`; it splits
// (spawns two children, resetting their countdown to SPLIT) when c reaches 1,
// otherwise it descends (spawns one child with c-1) — so the tree branches every
// SPLIT levels and reaches depth ≈ ttl with a bounded node count. (A countdown,
// not ttl % SPLIT, because `%` needs integer operands and JSON inputs are floats.)
function deepDef(name: string) {
  const splitChild = {
    name,
    input: { ttl: "{{input.ttl - 1}}", c: `{{${SPLIT}}}` }, // children reset countdown
    output_schema: outputSchema,
  };
  const descendChild = {
    name,
    input: { ttl: "{{input.ttl - 1}}", c: "{{input.c - 1}}" },
    output_schema: outputSchema,
  };
  return {
    name,
    input_schema: {
      type: "object",
      properties: { ttl: { type: "integer" }, c: { type: "integer" } },
      required: ["ttl", "c"],
    },
    steps: [
      {
        id: "recursion_condition",
        switch: [
          { case: "input.ttl <= 0", goto: "end" },
          { case: "input.c <= 1", goto: "$split" },
          { goto: "$descend" },
        ],
      },
      {
        id: "split",
        call: {
          type: "child_parallel" as const,
          children: { first: splitChild, second: splitChild },
        },
        switch: [{ goto: "end" }],
      },
      {
        id: "descend",
        call: {
          type: "child" as const,
          name,
          input: descendChild.input,
          output_schema: outputSchema,
        },
        switch: [{ goto: "end" }],
      },
    ],
    output: {
      processes:
        "{{(outputs.split.first.processes ?? 0) + (outputs.split.second.processes ?? 0) + (outputs.descend.processes ?? 0) + 1}}",
    },
  };
}

const buildDef = DEEP ? deepDef : simpleDef;

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
    body: buildDef(name) as never,
  });
  if (defErr) throw new Error(`register failed: ${JSON.stringify(defErr)}`);

  // Generous timeout: scales with tree size and poll interval.
  const timeoutMs = Math.max(30_000, INSTANCES_PER_ROOT * 20);

  const start = Date.now();
  const ids: string[] = [];
  for (let i = 0; i < ROOTS; i++) {
    const { data, error } = await client.POST("/instances", {
      body: {
        process: name,
        input: DEEP ? { ttl: TTL, c: SPLIT } : { ttl: TTL },
      } as never,
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
      `mode=${MODE} ttl=${TTL} ${DEEP ? `split=${SPLIT} ` : ""}` +
      `roots=${ROOTS} instances=${TOTAL_INSTANCES} ` +
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

// When BENCH_JSON is set, write the results as a github-action-benchmark
// customBiggerIsBetter array (so CI can chart throughput per commit over time).
function writeBenchJSON(path: string, results: EngineResult[]) {
  const entries = results.map((r) => {
    const avg = Math.round(
      r.durations.reduce((a, b) => a + b, 0) / r.durations.length,
    );
    return {
      name: `spawn ${MODE} ttl${TTL} ${r.engine}`,
      unit: "inst/s",
      value: throughput(avg),
    };
  });
  writeFileSync(path, JSON.stringify(entries, null, 2));
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
  const jsonPath = process.env.BENCH_JSON;
  if (jsonPath) writeBenchJSON(jsonPath, results);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
