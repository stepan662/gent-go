// Registers the order-pipeline and starts one instance.
// Requires gent to be running on localhost:8080.
//   Start gent: go run ./cmd/gent --http :8080
//   Start tasks: bun run playground:server   (in another terminal)
//
// Usage: bun run playground:run

import { client, waitForInstance } from "../helpers/client.ts";
import { processDefinition } from "./process.ts";
import type { ProcessInput } from "./generated/types.ts";

// ─── 1. register the process definition ────────────────────────────────────

console.log(
  `\nRegistering "${processDefinition.name}" v${processDefinition.version}…`,
);
const { error: defErr } = await client.PUT("/definitions", {
  body: processDefinition,
});
if (defErr) throw new Error(`registration failed: ${JSON.stringify(defErr)}`);
console.log("  registered");

// ─── 2. start an instance ──────────────────────────────────────────────────

const input: ProcessInput = {
  tasks: ["first", "second", "third", "fourth"],
};

console.log("\nStarting instance with input:", input);
const { data: startData, error: startErr } = await client.POST("/instances", {
  body: { process: processDefinition.name, input },
});
if (startErr) throw new Error(`start failed: ${JSON.stringify(startErr)}`);

const id = startData!.id;
console.log(`  instance id: ${id}`);

// ─── 3. wait for completion ────────────────────────────────────────────────

console.log("\nWaiting for completion…");
const status = await waitForInstance(id, 15_000);

const { data } = await client.GET("/instances/{id}", {
  params: { path: { id } },
});

console.log(`\nStatus:  ${status}`);
console.log("Outputs:", JSON.stringify(data?.context?.outputs, null, 2));
if (data?.error) console.log("Error:  ", data.error);
