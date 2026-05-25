// Registers the order-pipeline and starts one instance.
// Requires gent to be running on localhost:8080.
//   Start gent: go run ./cmd/gent --http :8080
//   Start tasks: bun run playground:server   (in another terminal)
//
// Usage: bun run playground:run

import { client } from "../helpers/client.ts";
import { processDefinition } from "./process.ts";
import type { ProcessInput } from "./generated/types.ts";

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

// ─── 1. register the process definition ────────────────────────────────────

console.log(
  `\nRegistering "${processDefinition.name}" v${processDefinition.version}…`,
);
const { error: defErr } = await client.PUT("/definitions", {
  body: processDefinition,
});
if (defErr) throw new Error(`registration failed: ${JSON.stringify(defErr)}`);
console.log("  registered");

const rounds = 100_000;
const maxInterval = 0;

for (let i = 0; i < rounds; i++) {
  startInstance();
  const interval = maxInterval * ((rounds - (i + 1)) / rounds);
  console.log(`${i}: ${interval}`);
  await sleep(interval);
}

async function startInstance() {
  // ─── 2. start an instance ──────────────────────────────────────────────────

  const start_time = Date.now();
  const input: ProcessInput = {
    tasks: 100,
    start_time,
  };

  const { data: startData, error: startErr } = await client.POST("/instances", {
    body: { process: processDefinition.name, input },
  });
  if (startErr) throw new Error(`start failed: ${JSON.stringify(startErr)}`);

  // const id = startData!.id;

  // ─── 3. wait for completion ────────────────────────────────────────────────

  // const status = await waitForInstance(id, 15_000);

  // const { data } = await client.GET("/instances/{id}", {
  //   params: { path: { id } },
  // });
  // if (data?.error) {
  //   console.log("Error:  ", data.error);
  // } else {
  //   console.log(`finished in ${Date.now() - start_time}`);
  // }
}
