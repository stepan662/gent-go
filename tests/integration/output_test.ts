import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

// The output map remaps an action's result: output_schema validates the full
// response, but only the projection is exported to outputs.<step> (coupling
// reduction), and the switch routes on the remapped self.output.
test("output map remaps an action result — only the projection is exported", async () => {
  const mock = await startMockService(0, {
    response: { job_id: "j-42", queue: "q1", secret: "shh" },
  });

  const name = `output_remap_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      steps: [
        {
          id: "create",
          action: {
            type: "rest" as const,
            endpoint: `http://localhost:${mock.port}/action`,
            output_schema: {
              type: "object",
              properties: {
                job_id: { type: "string" },
                queue: { type: "string" },
                secret: { type: "string" },
              },
              required: ["job_id"],
            },
          },
          output: { id: "{{ self.result.job_id }}" },
          switch: [
            { case: `self.output.id == "j-42"`, goto: "end" },
            { goto: "end" },
          ],
        },
      ],
      output: { id: "{{ outputs.create.id }}" },
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });

  const { data: startData } = await client.POST("/instances", { body: { process: name } });
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  // Only the projected {id} is exported — not the full {job_id, queue, secret} body.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((data?.context?.outputs as any)?.create).toEqual({ id: "j-42" });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((data?.context as any)?.output?.id).toBe("j-42");

  mock.stop();
});
