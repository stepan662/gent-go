import { expect, test } from "vitest";
import {
  client,
  startMockService,
  waitForInstance,
} from "../helpers/client.ts";

test("lifecycle — task step completes when service returns ok", async () => {
  const mock = await startMockService(0, {
    response: { status: "ok", output: { done: true } },
  });

  const name = `lifecycle_ok_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      version: 1,
      steps: [
        {
          type: "task" as const,
          id: "call",
          call: { type: "rest" as const, endpoint: `http://localhost:${mock.port}/action`, output_schema: { type: "object" } },
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

  const { data } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect((data?.context?.outputs as any)?.call?.done).toBe(true);

  mock.stop();
});

test("lifecycle — task step fails and retries then marks failed", async () => {
  const mock = await startMockService(0, { response: { status: "error", error: "boom" } });

  const name = `lifecycle_fail_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      version: 1,
      steps: [
        {
          type: "task" as const,
          id: "call",
          call: { type: "rest" as const, endpoint: `http://localhost:${mock.port}/action` },
          timeout_ms: 500,
          retries: 1,
        },
      ],
    },
  });

  const { data: startData } = await client.POST("/instances", {
    body: { process: name },
  });
  expect(await waitForInstance(startData!.id, 10_000)).toBe("failed");

  mock.stop();
});

test("lifecycle — conditional routes to correct branch", async () => {
  const thenMock = await startMockService(0, {
    response: { status: "ok", output: { branch: "then" } },
  });
  const elseMock = await startMockService(0, {
    response: { status: "ok", output: { branch: "else" } },
  });

  const name = `lifecycle_cond_${crypto.randomUUID()}`;
  const def = await client.PUT("/definitions", {
    body: {
      name,
      version: 1,
      input_schema: {
        type: "object",
        properties: {
          go_then: { type: "boolean" },
        },
        required: ["go_then"],
      },
      steps: [
        {
          id: "start",
          call: { type: "rest" as const, endpoint: `http://localhost:${thenMock.port}/action` },
          switch: [{ when: "{{input.go_then}}", goto: "#then_step" }, { when: "default", goto: "#else_step" }],
        },
        {
          id: "then_step",
          call: { type: "rest" as const, endpoint: `http://localhost:${thenMock.port}/action`, output_schema: { type: "object" } },
          timeout_ms: 1000,
          retries: 0,
          switch: [{ when: "default", goto: "$end" }],
        },
        {
          id: "else_step",
          call: { type: "rest" as const, endpoint: `http://localhost:${elseMock.port}/action`, output_schema: { type: "object" } },
          timeout_ms: 1000,
          retries: 0,
          switch: [{ when: "default", goto: "$end" }],
        },
      ],
    },
  } as const);

  expect(def.error).toBeUndefined();

  let i1Create = await client.POST("/instances", {
    body: { process: name, input: { go_then: true } },
  });
  expect(i1Create.error).toBeUndefined();
  await waitForInstance(i1Create.data!.id);

  const i1 = await client.GET("/instances/{id}", {
    params: { path: { id: i1Create.data!.id } },
  });

  expect(i1.error).toBeUndefined();

  expect((i1.data?.context?.outputs as any)?.then_step?.branch).toBe("then");
  expect((i1.data?.context?.outputs as any)?.else_step?.branch).toBe(undefined);

  const i2Create = await client.POST("/instances", {
    body: { process: name, input: { go_then: false } },
  });
  expect(i2Create.error).toBeUndefined();

  await waitForInstance(i2Create.data!.id);
  const i2 = await client.GET("/instances/{id}", {
    params: { path: { id: i2Create.data!.id } },
  });
  expect((i2.data?.context?.outputs as any)?.else_step?.branch).toBe("else");
  expect((i2.data?.context?.outputs as any)?.then_step?.branch).toBe(undefined);

  thenMock.stop();
  elseMock.stop();
});

test("lifecycle — task fails when output violates output_schema", async () => {
  const mock = await startMockService(0, {
    response: { status: "ok", output: { wrong_field: true } },
  });

  const name = `lifecycle_output_schema_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      version: 1,
      steps: [
        {
          id: "charge",
          call: {
            type: "rest" as const,
            endpoint: `http://localhost:${mock.port}/action`,
            output_schema: {
              type: "object",
              properties: { charged: { type: "boolean" } },
              required: ["charged"],
            },
          },
          timeout_ms: 2000,
          retries: 0,
        },
      ],
    },
  });

  const { data: startData } = await client.POST("/instances", {
    body: { process: name },
  });
  const id = startData!.id;

  expect(await waitForInstance(id, 5000)).toBe("failed");

  const { data } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect(data!.error!).toContain("output validation");

  mock.stop();
});
