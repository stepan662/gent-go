import { expect, test } from "vitest";
import { useTickEnv } from "./helpers.ts";

// Exercises the `assign` action: it computes values from expressions and stores
// them as outputs.<id>, readable by later steps and by its own switch as `self`.
// The counter loop is the bounded poll pattern — assign + a switch on the
// accumulated count terminates the loop without any external call.
const ctx = useTickEnv(20020);

const intObj = (field: string) => ({
  type: "object",
  properties: { [field]: { type: "integer" } },
  required: [field],
});

test("assign drives a bounded counter loop that terminates", async () => {
  // A single self-looping step: assign increments the counter and the switch
  // ends the loop once it reaches the bound. The process output reads the
  // final accumulated value.
  await ctx.env.client.PUT("/definitions", {
    body: {
      name: "counter",
      steps: [
        {
          id: "count",
          action: {
            type: "assign",
            values: { n: "{{ (outputs.count.n ?? 0) + 1 }}" },
            output_schema: intObj("n"),
          },
          switch: [
            { case: "self.n >= 3", goto: "end" },
            { goto: "$count" }, // loop back to itself
          ],
        },
      ],
      output: { n: "{{ outputs.count.n }}" },
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });

  const id = await ctx.env.start("counter");
  await ctx.env.tickUntilIdle();

  expect(await ctx.env.status(id)).toBe("completed");
  const { data } = await ctx.env.client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((data!.context as any)?.output?.n).toBe(3);
});

test("a later step can read an assign output", async () => {
  await ctx.env.define("chain", [
    {
      id: "set",
      action: {
        type: "assign",
        values: { base: "{{ 21 }}" },
      },
      switch: "next",
    },
    {
      id: "derive",
      action: {
        type: "assign",
        values: { doubled: "{{ outputs.set.base + outputs.set.base }}" },
        output_schema: intObj("doubled"),
      },
      switch: "end",
    },
  ]);

  const id = await ctx.env.start("chain");
  await ctx.env.tickUntilIdle();

  expect(await ctx.env.status(id)).toBe("completed");
  const { data } = await ctx.env.client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((data!.context as any)?.outputs?.derive?.doubled).toBe(42);
});
