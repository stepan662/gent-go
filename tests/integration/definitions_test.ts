import { assertEquals, assertNotEquals } from "jsr:@std/assert";
import { client } from "../helpers/client.ts";

const validDef = {
  name: `test_def_${crypto.randomUUID()}`,
  version: 1,
  steps: [
    {
      type: "task" as const,
      id: "step1",
      transport: "http" as const,
      endpoint: "http://localhost:19990/action",
      timeout_ms: 1000,
      retries: 0,
    },
  ],
};

Deno.test("PUT /definitions — registers a new definition", async () => {
  const { data, error } = await client.PUT("/definitions", { body: validDef });

  assertEquals(error, undefined, `unexpected error: ${JSON.stringify(error)}`);
  assertEquals(data?.name, validDef.name);
});

Deno.test("GET /definitions — lists registered definitions", async () => {
  await client.PUT("/definitions", { body: validDef });

  const { data, error } = await client.GET("/definitions");
  assertEquals(error, undefined);

  const defs = data!;
  const found = defs.some((d) => d.name === validDef.name);
  assertEquals(found, true, `definition ${validDef.name} not found in list`);
});

Deno.test("PUT /definitions — rejects task step without endpoint", async () => {
  const { data, error } = await client.PUT("/definitions", {
    body: {
      name: "bad",
      version: 1,
      steps: [
        {
          type: "task" as const,
          id: "s1",
          transport: "http" as const,
          endpoint: "http://localhost:19990/action",
        },
      ],
    },
  });

  assertEquals(error, undefined);
  assertEquals(data?.name, "bad");
});

Deno.test("PUT /definitions — rejects unknown step type", async () => {
  const { data, error } = await client.PUT("/definitions", {
    body: {
      name: "bad",
      version: 1,
      steps: [{ type: "parallel", id: "p1" }],
    },
  });

  assertNotEquals(error, undefined);
  assertEquals(data, undefined);
});

Deno.test("PUT /definitions — accepts valid definition", async () => {
  const { data, error } = await client.PUT("/definitions", {
    body: {
      name: "valid",
      version: 1,
      input_schema: {
        type: "object",
        properties: {
          foo: { type: "string" },
        },
        required: ["foo"],
      },
      steps: [{ type: "task", id: "t1" }],
    },
  });

  assertNotEquals(error, undefined);
  assertEquals(data, undefined);
});
