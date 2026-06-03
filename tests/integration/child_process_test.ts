import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

test("child_process — recursive spawn completes with correct aggregated output", async () => {
  const processName = `child_process_${crypto.randomUUID()}`;

  await client.PUT("/definitions", {
    body: {
      name: processName,
      version: 1,
      input_schema: {
        type: "object",
        properties: { ttl: { type: "integer" } },
        required: ["ttl"],
      },
      steps: [
        {
          id: "recursion_condition",
          switch: [{ when: "{{input.ttl > 0}}", goto: "#recursion" }, { when: "default", goto: "$end" }],
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

  const { data: startData, error: startError } = await client.POST("/instances", {
    body: { process: processName, input: { ttl: 2 } },
  });
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
