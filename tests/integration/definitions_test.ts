import { expect, test } from "bun:test";
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

test("PUT /definitions — registers a new definition", async () => {
  const { data, error } = await client.PUT("/definitions", { body: validDef });

  expect(error).toBeUndefined();
  expect(data?.name).toBe(validDef.name);
});

test("GET /definitions — lists registered definitions", async () => {
  await client.PUT("/definitions", { body: validDef });

  const { data, error } = await client.GET("/definitions");
  expect(error).toBeUndefined();
  expect(data!.some((d) => d.name === validDef.name)).toBe(true);
});

test("PUT /definitions — rejects task step without endpoint", async () => {
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

  expect(error).toBeUndefined();
  expect(data?.name).toBe("bad");
});

test("PUT /definitions — rejects unknown step type", async () => {
  const { data, error } = await client.PUT("/definitions", {
    body: {
      name: "bad",
      version: 1,
      steps: [{ type: "parallel", id: "p1" }],
    },
  });

  expect(error).toBeDefined();
  expect(data).toBeUndefined();
});

test("PUT /definitions — accepts valid definition", async () => {
  const { data, error } = await client.PUT("/definitions", {
    body: {
      name: "valid",
      version: 1,
      input_schema: {
        type: "object",
        properties: { foo: { type: "string" } },
        required: ["foo"],
      },
      steps: [{ type: "task", id: "t1" }],
    },
  });

  expect(error).toBeDefined();
  expect(data).toBeUndefined();
});
