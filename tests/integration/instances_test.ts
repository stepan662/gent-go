import { assertEquals, assertExists, assertNotEquals } from "jsr:@std/assert";
import { client } from "../helpers/client.ts";

const processName = `test_proc_${crypto.randomUUID()}`;

// Register a definition once for all instance tests.
async function ensureDefinition() {
  await client.PUT("/definitions", {
    body: {
      name: processName,
      version: 1,
      input_schema: {
        type: "object",
        properties: {
          order_id: { type: "number" },
        },
        required: ["order_id"],
      },
      steps: [
        {
          type: "task" as const,
          id: "s1",
          transport: "http" as const,
          endpoint: "http://localhost:19991/action",
          timeout_ms: 500,
          retries: 0,
        },
      ],
    },
  });
}

Deno.test("POST /instances — starts a new instance", async () => {
  await ensureDefinition();

  const { data, error } = await client.POST("/instances", {
    body: { process: processName, input: { order_id: 1 } },
  });

  assertEquals(error, undefined);

  const inst = data!;
  assertExists(inst.id);
  assertEquals(inst.status, "running");
});

Deno.test("GET /instances/{id} — returns instance status", async () => {
  await ensureDefinition();

  const instance = await client.POST("/instances", {
    body: { process: processName, input: { order_id: 1 } },
  });

  assertEquals(instance.error, undefined);

  const id = instance.data!.id;

  const { data, error } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  assertEquals(error, undefined);
  assertEquals(data!.id, id);
});

Deno.test("GET /instances/{id} — returns error for unknown ID", async () => {
  const { data, error } = await client.GET("/instances/{id}", {
    params: { path: { id: "00000000-0000-0000-0000-000000000000" } },
  });
  assertNotEquals(error, undefined);
  assertEquals(data?.context, undefined);
});

Deno.test("GET /instances — lists instances", async () => {
  const { data, error } = await client.GET("/instances");
  assertEquals(error, undefined);
  assertEquals(Array.isArray(data), true);
});

Deno.test("GET /instances/{id} — fails when input is invalid", async () => {
  await ensureDefinition();

  const { data, error } = await client.POST("/instances", {
    body: { process: processName, input: { order_id: "hi" } },
  });

  assertNotEquals(error, undefined);
  assertEquals(data, undefined);
});
