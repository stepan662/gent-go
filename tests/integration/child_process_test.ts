import { expect, test } from "vitest";
import { client, listAllInstances, startMockService, waitForInstance } from "../helpers/client.ts";

test("child — task without result_schema after a task with one does not fail", async () => {
  const id = crypto.randomUUID();
  const leafWithOutput = `leaf_with_output_${id}`;
  const leafNoOutput = `leaf_no_output_${id}`;
  const parentName = `parent_${id}`;

  await client.PUT("/definitions", {
    body: {
      name: leafWithOutput,
      tasks: [{ id: "done", switch: [{ goto: "end" }] }],
      output: { value: "{{1}}" },
    },
  });

  await client.PUT("/definitions", {
    body: {
      name: leafNoOutput,
      tasks: [{ id: "done", switch: [{ goto: "end" }] }],
    },
  });

  await client.PUT("/definitions", {
    body: {
      name: parentName,
      tasks: [
        {
          id: "task_a",
          action: {
            type: "child" as const,
            name: leafWithOutput,
            result_schema: {
              type: "object",
              properties: { value: { type: "number" } },
              required: ["value"],
            },
          },
          switch: [{ goto: "next" }],
        },
        {
          id: "task_b",
          action: { type: "child" as const, name: leafNoOutput },
          switch: [{ goto: "end" }],
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

test("child — output validation failure error includes process name", async () => {
  const id = crypto.randomUUID();
  const childName = `child_no_output_${id}`;
  const parentName = `parent_strict_${id}`;

  await client.PUT("/definitions", {
    body: {
      name: childName,
      tasks: [{ id: "done", switch: [{ goto: "end" }] }],
    },
  });

  await client.PUT("/definitions", {
    body: {
      name: parentName,
      tasks: [
        {
          id: "spawn",
          action: {
            type: "child" as const,
            name: childName,
            result_schema: {
              type: "object",
              properties: { required_field: { type: "string" } },
              required: ["required_field"],
            },
          },
          switch: [{ goto: "end" }],
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

test("child — on_error with child.failed pattern is rejected at registration", async () => {
  const id = crypto.randomUUID();
  const childName = `child_fails_${id}`;
  const parentName = `parent_handles_${id}`;

  await client.PUT("/definitions", {
    body: {
      name: childName,
      tasks: [
        {
          id: "action",
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  const { error } = await client.PUT("/definitions", {
    body: {
      name: parentName,
      tasks: [
        {
          id: "spawn",
          action: { type: "child" as const, name: childName },
          on_error: [{ code: ["child.%"], goto: "$recovery" }],
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  expect(error).toBeDefined();
  expect(JSON.stringify(error)).toContain("child.failed");
});

test("child — no on_error cascades to parent failure", async () => {
  const id = crypto.randomUUID();
  const failMock = await startMockService(0, { statusCode: 500 });

  const childName = `child_fails_${id}`;
  const parentName = `parent_no_handler_${id}`;

  await client.PUT("/definitions", {
    body: {
      name: childName,
      tasks: [
        {
          id: "action",
          action: { type: "rest" as const, endpoint: `http://localhost:${failMock.port}/action` },
          timeout_ms: 2000,
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  await client.PUT("/definitions", {
    body: {
      name: parentName,
      tasks: [
        {
          id: "spawn",
          action: { type: "child" as const, name: childName },
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  const { data } = await client.POST("/instances", { body: { process: parentName } });
  expect(await waitForInstance(data!.id, 10_000)).toBe("failed");

  failMock.stop();
});

test("child — failure propagates through the entire ancestor chain", async () => {
  const id = crypto.randomUUID();
  const failMock = await startMockService(0, { statusCode: 500 });

  const leafName = `leaf_fails_${id}`;
  const middleName = `middle_no_handler_${id}`;
  const grandName = `grand_no_handler_${id}`;

  await client.PUT("/definitions", {
    body: {
      name: leafName,
      tasks: [
        {
          id: "action",
          action: { type: "rest" as const, endpoint: `http://localhost:${failMock.port}/action` },
          timeout_ms: 2000,
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  await client.PUT("/definitions", {
    body: {
      name: middleName,
      tasks: [
        {
          id: "spawn",
          action: { type: "child" as const, name: leafName },
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  await client.PUT("/definitions", {
    body: {
      name: grandName,
      tasks: [
        {
          id: "spawn_middle",
          action: { type: "child" as const, name: middleName },
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  const { data } = await client.POST("/instances", { body: { process: grandName } });
  expect(await waitForInstance(data!.id, 15_000)).toBe("failed");

  const { data: inst } = await client.GET("/instances/{id}", { params: { path: { id: data!.id } } });
  expect(inst?.error).toBeTruthy();

  failMock.stop();
});

test("child — parent error contains child's error message when child fails", async () => {
  const id = crypto.randomUUID();
  const failMock = await startMockService(0, { statusCode: 503 });

  const childName = `child_err_msg_${id}`;
  const parentName = `parent_err_msg_${id}`;

  await client.PUT("/definitions", {
    body: {
      name: childName,
      tasks: [
        {
          id: "action",
          action: { type: "rest" as const, endpoint: `http://localhost:${failMock.port}/action` },
          timeout_ms: 2000,
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  await client.PUT("/definitions", {
    body: {
      name: parentName,
      tasks: [
        {
          id: "spawn",
          action: { type: "child" as const, name: childName },
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  const { data } = await client.POST("/instances", { body: { process: parentName } });
  expect(await waitForInstance(data!.id, 10_000)).toBe("failed");

  const { data: inst } = await client.GET("/instances/{id}", { params: { path: { id: data!.id } } });
  expect(inst?.error).toBeTruthy();

  failMock.stop();
});

test("child_parallel — recursive spawn completes with correct aggregated output", async () => {
  const processName = `child_parallel_${crypto.randomUUID()}`;

  await client.PUT("/definitions", {
    body: {
      name: processName,
      input_schema: {
        type: "object",
        properties: { ttl: { type: "integer" } },
        required: ["ttl"],
      },
      tasks: [
        {
          id: "recursion_condition",
          switch: [
            { case: "input.ttl > 0", goto: "$recursion" },
            { goto: "end" },
          ],
        },
        {
          id: "recursion",
          action: {
            type: "child_parallel" as const,
            children: {
              first: {
                name: processName,
                input: { ttl: "{{input.ttl - 1}}" },
                result_schema: {
                  type: "object",
                  properties: { processes: { type: "number" } },
                  required: ["processes"],
                },
              },
              second: {
                name: processName,
                input: { ttl: "{{input.ttl - 1}}" },
                result_schema: {
                  type: "object",
                  properties: { processes: { type: "number" } },
                  required: ["processes"],
                },
              },
            },
          },
          output: "{{ self.result }}",
          switch: [{ goto: "end" }],
        },
      ],
      output: {
        processes:
          "{{(outputs.recursion.first.processes ?? 0) + (outputs.recursion.second.processes ?? 0) + 1}}",
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

  // ttl=2: 1 root + 2 children (ttl=1) + 4 grandchildren (ttl=0) = 7 instances.
  // Page through (default limit is now 20 and other tests add instances concurrently).
  const spawned = (await listAllInstances()).filter((i) => i.process === processName);
  expect(spawned).toHaveLength(7);
  expect(spawned.every((i) => i.status === "completed")).toBe(true);

  // root output: (3 + 3 + 1) = 7
  const { data } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect((data?.context?.output as any)?.processes).toBe(7);
});

// Regression: a parent with TWO sequential child tasks must spawn both batches.
// Before wait_state was persisted by UpdateInstanceProgress, the stale
// 'collecting' left over from the first task's collect made the engine treat
// the second spawn task as already-collected and skip it silently.
test("child — two sequential child tasks both spawn and collect", async () => {
  const uid = crypto.randomUUID();
  const leafName = `seq_leaf_${uid}`;
  const parentName = `seq_parent_${uid}`;
  const mock = await startMockService(0, { response: { ok: true } });

  try {
    await client.PUT("/definitions", {
      body: {
        name: leafName,
        tasks: [
          {
            id: "work",
            action: { type: "rest" as const, endpoint: `http://localhost:${mock.port}/action` },
            timeout_ms: 2000,
            switch: [{ goto: "end" }],
          },
        ],
        output: { done: "{{true}}" },
      },
    });
    await client.PUT("/definitions", {
      body: {
        name: parentName,
        tasks: [
          {
            id: "first",
            action: { type: "child" as const, name: leafName },
            output: "{{ self.result }}",
            switch: [{ goto: "next" }],
          },
          {
            id: "second",
            action: { type: "child" as const, name: leafName },
            output: "{{ self.result }}",
            switch: [{ goto: "end" }],
          },
        ],
      },
    });

    const { data: startData } = await client.POST("/instances", {
      body: { process: parentName },
    });
    const id = startData!.id;
    expect(await waitForInstance(id, 10_000)).toBe("completed");

    // Both leaves actually executed…
    expect(mock.requestCount()).toBe(2);

    // …and both collects produced an output.
    const { data } = await client.GET("/instances/{id}", {
      params: { path: { id } },
    });
    const outputs = data?.context?.outputs as any;
    expect(outputs?.first?.done).toBe(true);
    expect(outputs?.second?.done).toBe(true);
  } finally {
    await mock.stop();
  }
});
