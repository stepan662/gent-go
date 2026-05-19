import { expect, test } from "bun:test";
import { client } from "../helpers/client.ts";

const processName = `test_proc_${crypto.randomUUID()}`;

async function ensureDefinition() {
  await client.PUT("/definitions", {
    body: {
      name: processName,
      version: 1,
      input_schema: {
        type: "object",
        properties: { order_id: { type: "number" } },
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

test("POST /instances — starts a new instance", async () => {
  await ensureDefinition();

  const { data, error } = await client.POST("/instances", {
    body: { process: processName, input: { order_id: 1 } },
  });

  expect(error).toBeUndefined();
  expect(data!.id).toBeDefined();
  expect(data!.status).toBe("running");
});

test("GET /instances/{id} — returns instance status", async () => {
  await ensureDefinition();

  const { data: startData, error: startError } = await client.POST("/instances", {
    body: { process: processName, input: { order_id: 1 } },
  });

  expect(startError).toBeUndefined();
  const id = startData!.id;

  const { data, error } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect(error).toBeUndefined();
  expect(data!.id).toBe(id);
});

test("GET /instances/{id} — returns error for unknown ID", async () => {
  const { data, error } = await client.GET("/instances/{id}", {
    params: { path: { id: "00000000-0000-0000-0000-000000000000" } },
  });
  expect(error).toBeDefined();
  expect(data?.context).toBeUndefined();
});

test("GET /instances — lists instances", async () => {
  const { data, error } = await client.GET("/instances");
  expect(error).toBeUndefined();
  expect(Array.isArray(data)).toBe(true);
});

test("POST /instances — fails when input is invalid", async () => {
  await ensureDefinition();

  const { data, error } = await client.POST("/instances", {
    body: { process: processName, input: { order_id: "hi" } },
  });

  expect(error).toBeDefined();
  expect(data).toBeUndefined();
});
