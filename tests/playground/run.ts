// Registers the order-pipeline and starts one instance.
// Requires gent to be running on localhost:8888.
//   Start gent:   go run ./cmd/gent --http :8888
//   Start tasks:  bun run playground:server   (in another terminal)
//
// Usage: bun run playground:run

import { join } from "node:path";
import { spawnSync } from "node:child_process";
import type { ProcessInput } from "./generated/types.ts";
import { createClientTyped, waitForInstance } from "../helpers/client.ts";

const PROCESS_NAME = "order-pipeline";
const repoRoot = join(import.meta.dirname, "../..");
const processYaml = join(import.meta.dirname, "process.yaml");

const client = createClientTyped({ baseUrl: "http://localhost:8888" });

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

// ─── 1. register the process definition ────────────────────────────────────

console.log(`\nRegistering "${PROCESS_NAME}"…`);
const reg = spawnSync(
  "go",
  [
    "run",
    "./cmd/gentctl",
    "apply",
    "--server",
    "http://localhost:8888",
    "-f",
    processYaml,
  ],
  { cwd: repoRoot, encoding: "utf8", stdio: "inherit" },
);
if (reg.status !== 0) throw new Error("gentctl apply failed");

const rounds = 1;
const maxInterval = 100;

for (let i = 0; i < rounds; i++) {
  startInstance();
  const interval = maxInterval * ((rounds - (i + 1)) / rounds);
  console.log(`${i}: ${interval}`);
  await sleep(interval);
}

async function startInstance() {
  // ─── 2. start an instance ──────────────────────────────────────────────────

  const input: ProcessInput = {
    ttl: 8,
  };

  const { data: startData, error: startErr } = await client.POST("/instances", {
    body: { process: PROCESS_NAME, input },
  });
  if (startErr) throw new Error(`start failed: ${JSON.stringify(startErr)}`);

  const id = startData!.id;

  // ─── 3. wait for completion ────────────────────────────────────────────────

  const status = await waitForInstance(id, Infinity);

  const { data } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  if (data?.error) {
    console.log(status, data?.error);
  } else {
    console.log(status, (data?.context as any).output?.processes);
  }
}
