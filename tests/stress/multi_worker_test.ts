import { afterAll, beforeAll, describe, expect, test } from "vitest";
import { buildGentBinary, startGent, type GentProcess } from "../helpers/server.ts";

// Multi-worker collision stress. Several independent `gent` processes — each its
// own OS process and its own connection pool/handle — poll the same database
// while a chaos loop randomly cancels and retries roots. Unlike the in-process
// Go engine stress tests (engines as goroutines sharing one *db.DB), this is the
// real shape of a worker fleet: correctness rests on the claim/lease/child-finish
// locks holding across separate processes.
//
// Workload: a recursive child_parallel process. With ttl=D every instance spawns
// two children with ttl=D-1 until ttl hits 0, so each root grows a binary tree of
// exactly 2^(D+1)-1 instances, and the root's aggregated `output.processes`
// re-counts that subtree bottom-up — a built-in exactly-once checksum.
//
// Collision signals asserted after the chaos settles:
//   1. every instance is terminal — no instance left stuck running/waiting/
//      collecting by a lost update or a cancel racing a spawn;
//   2. every root reaches completed once the chaos stops (driven green by
//      forced retries) — no tree wedged by cross-worker contention;
//   3. each completed root's output.processes == subtree size — no worker
//      double-spawned a child or double-counted an output.
//
// Postgres only. A worker fleet is a Postgres-only deployment: separate processes
// rely on FOR UPDATE SKIP LOCKED claims and per-row FOR UPDATE child-finish locks.
// SQLite is single-writer/single-process — running several gent processes against
// one file wedges under chaos (a cancel-cascade transaction lost to
// SQLITE_BUSY_SNAPSHOT strands a cancelling|waiting parent). The SQLite *supported*
// multi-worker model is multiple engines in ONE process, with the same random
// cancel/retry + exactly-once coverage in the Go test
// TestStress_Chaos_CancelRetryRandomErrors (internal/engine/stress_test.go).

const DSN = process.env.POSTGRES_DSN;

const WORKER_COUNT = 3;
const ROOT_COUNT = 6;
const TTL = 3; // each root → 2^(TTL+1)-1 = 15 instances
const NODES_PER_ROOT = 2 ** (TTL + 1) - 1;
const CHAOS_MS = 4_000;
const SETTLE_MS = 60_000;

interface Backend {
  name: string;
  enabled: boolean;
  basePort: number;
  pollMs: number;
  db: string; // sqlite file path (shared by all workers); "" for postgres
  pgDSN?: string;
  env?: Record<string, string>;
}

// Each worker is an independent process, all opening the same DSN with a small
// pool each (so WORKER_COUNT pools stay well under Postgres' max_connections).
const backends: Backend[] = [
  {
    name: "postgres",
    enabled: !!DSN,
    basePort: 8920,
    pollMs: 5,
    db: "",
    pgDSN: DSN,
    // Small pool per worker (passed to gent as --pg-max-open-conns) so
    // WORKER_COUNT pools stay well under Postgres' max_connections.
    env: { GENT_PG_MAX_OPEN_CONNS: "8" },
  },
];

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const isTerminal = (s?: string) =>
  s === "completed" || s === "failed" || s === "cancelled";

let binPromise: Promise<string> | undefined;
const gentBin = () => (binPromise ??= buildGentBinary());

for (const backend of backends) {
  describe.runIf(backend.enabled)(`multi-worker chaos — ${backend.name}`, () => {
    let workers: GentProcess[] = [];

    beforeAll(async () => {
      const bin = await gentBin();
      for (const [k, v] of Object.entries(backend.env ?? {})) process.env[k] = v;
      // Spawn sequentially: the first process runs migrations before any other
      // opens the DB, avoiding a concurrent-migration race on the same file.
      for (let i = 0; i < WORKER_COUNT; i++) {
        workers.push(
          await startGent(
            bin,
            backend.basePort + i,
            backend.db,
            backend.pgDSN,
            backend.pollMs,
            5, // max-concurrent
            true, // immediate retries (no backoff) — maximise contention
          ),
        );
      }
    }, 60_000);

    afterAll(() => {
      for (const w of workers) w.stop();
      workers = [];
    });

    test(
      "recursive trees survive random cancel/retry across separate processes",
      async () => {
        const api = workers[0].client;
        const processName = `stress_chaos_${crypto.randomUUID()}`;

        await api.PUT("/definitions", {
          body: {
            name: processName,
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
                  children: {
                    first: {
                      name: processName,
                      input: { ttl: "{{input.ttl - 1}}" },
                      output_schema: {
                        type: "object",
                        properties: { processes: { type: "number" } },
                        required: ["processes"],
                      },
                    },
                    second: {
                      name: processName,
                      input: { ttl: "{{input.ttl - 1}}" },
                      output_schema: {
                        type: "object",
                        properties: { processes: { type: "number" } },
                        required: ["processes"],
                      },
                    },
                  },
                },
                switch: [{ goto: "end" }],
              },
            ],
            output: {
              processes:
                "{{(outputs.recursion.first.processes ?? 0) + (outputs.recursion.second.processes ?? 0) + 1}}",
            },
          },
        });

        const rootIds: string[] = [];
        for (let i = 0; i < ROOT_COUNT; i++) {
          const { data, error } = await api.POST("/instances", {
            body: { process: processName, input: { ttl: TTL } },
          });
          expect(error).toBeUndefined();
          rootIds.push(data!.id);
        }
        const randomRoot = () => rootIds[Math.floor(Math.random() * rootIds.length)];

        // Chaos window: hammer random roots with cancels and retries while the
        // workers race to advance, cancel, and re-spawn the same trees. All
        // errors (cancel of a completed root, retry of a non-settled one) are
        // expected and ignored — they are part of the contention.
        let chaosOn = true;
        const canceller = (async () => {
          while (chaosOn) {
            await api
              .POST("/instances/{id}/cancel", { params: { path: { id: randomRoot() } } })
              .catch(() => {});
            await sleep(30 + Math.random() * 70);
          }
        })();
        const retrier = (async () => {
          while (chaosOn) {
            await api
              .POST("/instances/{id}/retry", {
                params: { path: { id: randomRoot() }, query: { force: Math.random() < 0.5 } },
              })
              .catch(() => {});
            await sleep(30 + Math.random() * 70);
          }
        })();

        await sleep(CHAOS_MS);
        chaosOn = false;
        await Promise.all([canceller, retrier]);

        // Settlement: service is calm now; force-retry any settled (failed/
        // cancelled) root until every tree is completed and nothing is left
        // mid-flight.
        const byProcess = (i: { process?: string }) => i.process === processName;
        const deadline = Date.now() + SETTLE_MS;
        let allDone = false;
        while (Date.now() < deadline) {
          const { data } = await api.GET("/instances");
          const insts = (data ?? []).filter(byProcess);
          const byId = new Map(insts.map((i) => [i.id, i]));

          let rootsCompleted = true;
          for (const id of rootIds) {
            const r = byId.get(id);
            if (r?.status === "completed") continue;
            rootsCompleted = false;
            if (r && (r.status === "failed" || r.status === "cancelled")) {
              await api
                .POST("/instances/{id}/retry", {
                  params: { path: { id }, query: { force: true } },
                })
                .catch(() => {});
            }
          }
          if (rootsCompleted && insts.every((i) => isTerminal(i.status))) {
            allDone = true;
            break;
          }
          await sleep(150);
        }
        expect(allDone, "all roots completed and every instance terminal").toBe(true);

        // Final state: no instance stuck, every root green, every surviving tree
        // aggregated to its exact size (exactly-once under contention).
        const { data: finalAll } = await api.GET("/instances");
        const finalInsts = (finalAll ?? []).filter(byProcess);
        expect(finalInsts.every((i) => isTerminal(i.status))).toBe(true);

        for (const id of rootIds) {
          const { data } = await api.GET("/instances/{id}", { params: { path: { id } } });
          expect(data?.status).toBe("completed");
          expect((data?.context?.output as { processes?: number })?.processes).toBe(
            NODES_PER_ROOT,
          );
        }
      },
      120_000,
    );
  });
}
