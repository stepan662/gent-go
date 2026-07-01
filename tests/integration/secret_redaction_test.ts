import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

// A config var marked secret:true, interpolated into the process output, must be
// redacted ("***") when the context is returned over the API — even though it is
// stored plainly in the DB. The non-secret sibling is returned as-is.
// Fixture: GENROC_GLOBAL_API_KEY = "supersecret-api-key" (see helpers/server.ts).
test("a secret config value is redacted from the API context", async () => {
  const name = `secret_redact_${crypto.randomUUID()}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name,
      config_schema: {
        type: "object",
        required: ["api_key"],
        properties: { api_key: { type: "string", secret: true } },
      },
      tasks: [{ id: "route", switch: "end" }],
      output: {
        // Even concatenated / transformed, the secret must not leak.
        auth: "Bearer {{ config.api_key }}",
        note: "public value",
      },
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(putErr).toBeUndefined();

  const { data: startData } = await client.POST("/instances", { body: { process: name } });
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const output = (data?.context as any)?.output;
  expect(output.note).toBe("public value");
  expect(output.auth).toBe("***");
  expect(JSON.stringify(data)).not.toContain("supersecret-api-key");
});
