import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

// Config vars resolve from GENT_<PROCESS>_<NAME>, falling back to
// GENT_GLOBAL_<NAME>, and are exposed to expressions under the "config"
// namespace. These tests use the global tier (process names are random).
// Fixtures set on the test server (see helpers/server.ts):
//   GENT_GLOBAL_E2E_URL   = https://config.example.test
//   GENT_GLOBAL_E2E_PORT  = 8080
//   GENT_GLOBAL_E2E_TOKEN = supersecret-token-value

// Resolution + coercion + default end-to-end: a string passes through, an
// integer is coerced to a number, and an unset optional var falls back to its
// default — all reachable in expressions as config.<NAME>.
test("config resolves from the environment and is usable in expressions", async () => {
  const name = `config_resolve_${crypto.randomUUID()}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name,
      config_schema: {
        type: "object",
        required: ["e2e_url"],
        properties: {
          e2e_url: { type: "string" },
          e2e_port: { type: "integer" },
          e2e_region: { type: "string", default: "us" },
        },
      },
      tasks: [{ id: "route", switch: "end" }],
      output: {
        url: "{{ config.e2e_url }}",
        port: "{{ config.e2e_port }}",
        region: "{{ config.e2e_region }}",
      },
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(putErr).toBeUndefined();

  const { data: startData } = await client.POST("/instances", {
    body: { process: name },
  });
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const output = (data?.context as any)?.output;
  expect(output.url).toBe("https://config.example.test");
  expect(output.port).toBe(8080); // coerced to a number, not the string "8080"
  expect(output.region).toBe("us"); // default applied (e2e_region unset)
});

// A required config var with no corresponding environment variable rejects the
// start request up front rather than producing an instance that fails on tick.
test("starting an instance fails when a required config var is unset", async () => {
  const name = `config_missing_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      config_schema: {
        type: "object",
        required: ["e2e_not_set"],
        properties: { e2e_not_set: { type: "string" } },
      },
      tasks: [{ id: "route", switch: "end" }],
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });

  const { data, error } = await client.POST("/instances", {
    body: { process: name },
  });
  expect(data).toBeUndefined();
  expect(error).toBeDefined();
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect(JSON.stringify(error)).toContain("e2e_not_set");
});
