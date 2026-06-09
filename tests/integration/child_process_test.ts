import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

test("child_process — step without child_output_schema after a step with one does not fail", async () => {
  const id = crypto.randomUUID();
  const leafWithOutput = `leaf_with_output_${id}`;
  const leafNoOutput = `leaf_no_output_${id}`;
  const parentName = `parent_${id}`;

  await client.PUT("/definitions", {
    body: {
      name: leafWithOutput,
      steps: [{ id: "done", switch: [{ when: "default", goto: "$end" }] }],
      output: { value: "{{1}}" },
    },
  });

  await client.PUT("/definitions", {
    body: {
      name: leafNoOutput,
      steps: [{ id: "done", switch: [{ when: "default", goto: "$end" }] }],
    },
  });

  await client.PUT("/definitions", {
    body: {
      name: parentName,

      steps: [
        {
          id: "step_a",
          call: {
            type: "child_process" as const,
            processes: [{ name: leafWithOutput }],
            child_output_schema: {
              type: "object",
              properties: { value: { type: "number" } },
              required: ["value"],
            },
          },
        },
        {
          id: "step_b",
          call: {
            type: "child_process" as const,
            processes: [{ name: leafNoOutput }],
          },
        },
      ],
    },
  });

  const { data, error } = await client.POST("/instances", {
    body: { process: parentName },
  });
  expect(error).toBeUndefined();

  const status = await waitForInstance(data!.id, 10_000);
  expect(status).toBe("completed");
});

// Regression: when a child process fails output validation, the error message must include
// the child's process name so the caller can identify which process caused the failure.
test("child_process — output validation failure error includes process name", async () => {
  const id = crypto.randomUUID();
  const childName = `child_no_output_${id}`;
  const parentName = `parent_strict_${id}`;

  // Child produces no output.
  await client.PUT("/definitions", {
    body: {
      name: childName,
      steps: [{ id: "done", switch: [{ when: "default", goto: "$end" }] }],
    },
  });

  // Parent declares a schema the child can never satisfy.
  await client.PUT("/definitions", {
    body: {
      name: parentName,
      steps: [
        {
          id: "spawn",
          call: {
            type: "child_process" as const,
            processes: [{ name: childName }],
            child_output_schema: {
              type: "object",
              properties: { required_field: { type: "string" } },
              required: ["required_field"],
            },
          },
        },
      ],
    },
  });

  const { data, error } = await client.POST("/instances", {
    body: { process: parentName },
  });
  expect(error).toBeUndefined();

  const status = await waitForInstance(data!.id, 10_000);
  expect(status).toBe("failed");

  const { data: inst } = await client.GET("/instances/{id}", {
    params: { path: { id: data!.id } },
  });
  expect(inst?.error).toContain(childName);
});

test("child_process — on_error routes to recovery when child fails", async () => {
  const id = crypto.randomUUID();
  const failMock = await startMockService(0, { statusCode: 500 });
  const recoveryMock = await startMockService(0, { response: { recovered: true } });

  const childName = `child_fails_${id}`;
  const parentName = `parent_handles_${id}`;

  await client.PUT("/definitions", {
    body: {
      name: childName,
      steps: [
        {
          id: "action",
          call: { type: "rest" as const, endpoint: `http://localhost:${failMock.port}/action` },
          timeout_ms: 2000,
        },
      ],
    },
  });

  await client.PUT("/definitions", {
    body: {
      name: parentName,
      steps: [
        {
          id: "spawn",
          call: { type: "child_process" as const, processes: [{ name: childName }] },
          on_error: [{ code: ["child.%"], goto: "#recovery" }],
        },
        {
          id: "recovery",
          call: {
            type: "rest" as const,
            endpoint: `http://localhost:${recoveryMock.port}/action`,
            output_schema: {
              type: "object",
              properties: { recovered: { type: "boolean" } },
              required: ["recovered"],
            },
          },
          timeout_ms: 2000,
        },
      ],
    },
  });

  const { data } = await client.POST("/instances", { body: { process: parentName } });
  expect(await waitForInstance(data!.id, 10_000)).toBe("completed");

  const { data: inst } = await client.GET("/instances/{id}", { params: { path: { id: data!.id } } });
  expect((inst?.context?.outputs as any)?.recovery?.recovered).toBe(true);

  failMock.stop();
  recoveryMock.stop();
});

test("child_process — no on_error on child_process step cascades to parent failure", async () => {
  const id = crypto.randomUUID();
  const failMock = await startMockService(0, { statusCode: 500 });

  const childName = `child_fails_${id}`;
  const parentName = `parent_no_handler_${id}`;

  await client.PUT("/definitions", {
    body: {
      name: childName,
      steps: [
        {
          id: "action",
          call: { type: "rest" as const, endpoint: `http://localhost:${failMock.port}/action` },
          timeout_ms: 2000,
        },
      ],
    },
  });

  await client.PUT("/definitions", {
    body: {
      name: parentName,
      steps: [
        {
          id: "spawn",
          call: { type: "child_process" as const, processes: [{ name: childName }] },
        },
      ],
    },
  });

  const { data } = await client.POST("/instances", { body: { process: parentName } });
  expect(await waitForInstance(data!.id, 10_000)).toBe("failed");

  failMock.stop();
});

test("child_process — on_error bubbles to grandparent when parent has no handler", async () => {
  const id = crypto.randomUUID();
  const failMock = await startMockService(0, { statusCode: 500 });
  const recoveryMock = await startMockService(0, { response: { recovered: true } });

  const childName = `leaf_fails_${id}`;
  const middleName = `middle_no_handler_${id}`;
  const grandName = `grand_handles_${id}`;

  await client.PUT("/definitions", {
    body: {
      name: childName,
      steps: [
        {
          id: "action",
          call: { type: "rest" as const, endpoint: `http://localhost:${failMock.port}/action` },
          timeout_ms: 2000,
        },
      ],
    },
  });

  await client.PUT("/definitions", {
    body: {
      name: middleName,
      steps: [
        {
          id: "spawn",
          call: { type: "child_process" as const, processes: [{ name: childName }] },
        },
      ],
    },
  });

  await client.PUT("/definitions", {
    body: {
      name: grandName,
      steps: [
        {
          id: "spawn_middle",
          call: { type: "child_process" as const, processes: [{ name: middleName }] },
          on_error: [{ code: ["child.%"], goto: "#recovery" }],
        },
        {
          id: "recovery",
          call: {
            type: "rest" as const,
            endpoint: `http://localhost:${recoveryMock.port}/action`,
            output_schema: {
              type: "object",
              properties: { recovered: { type: "boolean" } },
              required: ["recovered"],
            },
          },
          timeout_ms: 2000,
        },
      ],
    },
  });

  const { data } = await client.POST("/instances", { body: { process: grandName } });
  expect(await waitForInstance(data!.id, 15_000)).toBe("completed");

  const { data: inst } = await client.GET("/instances/{id}", { params: { path: { id: data!.id } } });
  expect((inst?.context?.outputs as any)?.recovery?.recovered).toBe(true);

  failMock.stop();
  recoveryMock.stop();
});

test("child_process — error context has correct code and step when child fails", async () => {
  const id = crypto.randomUUID();
  const failMock = await startMockService(0, { statusCode: 500 });
  const recoveryMock = await startMockService(0, { response: { ok: true } });

  const childName = `child_err_ctx_${id}`;
  const parentName = `parent_err_ctx_${id}`;

  await client.PUT("/definitions", {
    body: {
      name: childName,
      steps: [
        {
          id: "action",
          call: { type: "rest" as const, endpoint: `http://localhost:${failMock.port}/action` },
          timeout_ms: 2000,
        },
      ],
    },
  });

  await client.PUT("/definitions", {
    body: {
      name: parentName,
      steps: [
        {
          id: "spawn",
          call: { type: "child_process" as const, processes: [{ name: childName }] },
          on_error: [{ code: ["child.%"], goto: "#recovery" }],
        },
        {
          id: "recovery",
          call: {
            type: "rest" as const,
            endpoint: `http://localhost:${recoveryMock.port}/action`,
            output_schema: {
              type: "object",
              properties: { ok: { type: "boolean" } },
              required: ["ok"],
            },
          },
          timeout_ms: 2000,
        },
      ],
    },
  });

  const { data } = await client.POST("/instances", { body: { process: parentName } });
  expect(await waitForInstance(data!.id, 10_000)).toBe("completed");

  const { data: inst } = await client.GET("/instances/{id}", { params: { path: { id: data!.id } } });
  const err = inst?.context?.error as any;
  expect(err?.code).toBe("child.failed");
  expect(err?.step).toBe("spawn");

  failMock.stop();
  recoveryMock.stop();
});

test("child_process — recursive spawn completes with correct aggregated output", async () => {
  const processName = `child_process_${crypto.randomUUID()}`;

  await client.PUT("/definitions", {
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
            { when: "{{input.ttl > 0}}", goto: "#recursion" },
            { when: "default", goto: "$end" },
          ],
        },
        {
          id: "recursion",
          call: {
            type: "child_process" as const,
            processes: [
              { name: processName, input: { ttl: "{{input.ttl - 1}}" } },
              { name: processName, input: { ttl: "{{input.ttl - 1}}" } },
            ],
            child_output_schema: {
              type: "object",
              properties: { processes: { type: "number" } },
              required: ["processes"],
            },
          },
        },
      ],
      output: {
        processes:
          "{{(outputs.recursion[0].output.processes ?? 0) + (outputs.recursion[1].output.processes ?? 0) + 1}}",
      },
    },
  });

  const { data: startData, error: startError } = await client.POST(
    "/instances",
    {
      body: { process: processName, input: { ttl: 2 } },
    },
  );
  expect(startError).toBeUndefined();
  const id = startData!.id;

  expect(await waitForInstance(id, 10_000)).toBe("completed");

  // ttl=2: 1 root + 2 children (ttl=1) + 4 grandchildren (ttl=0) = 7 instances
  const { data: allInstances } = await client.GET("/instances");
  const spawned = (allInstances ?? []).filter((i) => i.process === processName);
  expect(spawned).toHaveLength(7);
  expect(spawned.every((i) => i.status === "completed")).toBe(true);

  // root output: (3 + 3 + 1) = 7
  const { data } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect((data?.context?.output as any)?.processes).toBe(7);
});
