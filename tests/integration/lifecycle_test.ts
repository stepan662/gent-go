import { expect, test } from "bun:test";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

test("lifecycle — task step completes when service returns ok", async () => {
  const mock = startMockService(19992, { status: "ok", output: { done: true } });

  const name = `lifecycle_ok_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      version: 1,
      steps: [
        {
          type: "task" as const,
          id: "call",
          transport: "http" as const,
          endpoint: "http://localhost:19992/action",
          timeout_ms: 2000,
          retries: 0,
        },
      ],
    },
  });

  const { data: startData } = await client.POST("/instances", {
    body: { process: name, input: { x: 1 } },
  });
  const id = startData!.id;

  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  expect(data?.context?.done).toBe(true);

  mock.stop();
});

test("lifecycle — task step fails and retries then marks failed", async () => {
  const mock = startMockService(19993, { status: "error", error: "boom" });

  const name = `lifecycle_fail_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      version: 1,
      steps: [
        {
          type: "task" as const,
          id: "call",
          transport: "http" as const,
          endpoint: "http://localhost:19993/action",
          timeout_ms: 500,
          retries: 1,
        },
      ],
    },
  });

  const { data: startData } = await client.POST("/instances", { body: { process: name } });
  expect(await waitForInstance(startData!.id, 10_000)).toBe("failed");

  mock.stop();
});

test("lifecycle — conditional routes to correct branch", async () => {
  const thenMock = startMockService(19994, { status: "ok", output: { branch: "then" } });
  const elseMock = startMockService(19995, { status: "ok", output: { branch: "else" } });

  const name = `lifecycle_cond_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      version: 1,
      steps: [
        {
          type: "conditional" as const,
          id: "check",
          condition: "context.input.go_then == true",
          then: [
            {
              type: "task" as const,
              id: "then_step",
              transport: "http" as const,
              endpoint: "http://localhost:19994/action",
              timeout_ms: 1000,
              retries: 0,
            },
          ],
          else: [
            {
              type: "task" as const,
              id: "else_step",
              transport: "http" as const,
              endpoint: "http://localhost:19995/action",
              timeout_ms: 1000,
              retries: 0,
            },
          ],
        },
      ],
    },
  });

  const { data: d1 } = await client.POST("/instances", {
    body: { process: name, input: { go_then: true } },
  });
  await waitForInstance(d1!.id);
  const { data: r1 } = await client.GET("/instances/{id}", { params: { path: { id: d1!.id } } });
  expect(r1?.context?.branch).toBe("then");

  const { data: d2 } = await client.POST("/instances", {
    body: { process: name, input: { go_then: false } },
  });
  await waitForInstance(d2!.id);
  const { data: r2 } = await client.GET("/instances/{id}", { params: { path: { id: d2!.id } } });
  expect(r2?.context?.branch).toBe("else");

  thenMock.stop();
  elseMock.stop();
});

test("lifecycle — task fails when output violates output_schema", async () => {
  const mock = startMockService(19996, { status: "ok", output: { wrong_field: true } });

  const name = `lifecycle_output_schema_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      version: 1,
      steps: [
        {
          type: "task" as const,
          id: "charge",
          transport: "http" as const,
          endpoint: "http://localhost:19996/action",
          timeout_ms: 2000,
          retries: 0,
          output_schema: {
            type: "object",
            properties: { charged: { type: "boolean" } },
            required: ["charged"],
          },
        },
      ],
    },
  });

  const { data: startData } = await client.POST("/instances", { body: { process: name } });
  const id = startData!.id;

  expect(await waitForInstance(id, 5000)).toBe("failed");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  expect(data!.error!).toContain("output validation");

  mock.stop();
});
