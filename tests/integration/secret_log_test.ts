import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

// A secret config value used to build the endpoint URL must be redacted to "***"
// in the stored audit log — the raw secret must never reach the logs table.
// GENT_GLOBAL_SERVER_URL = http://localhost:14100 (fixture in helpers/server.ts).
test("a secret config value in the endpoint URL is redacted in stored logs", async () => {
  const mock = await startMockService(14100, { response: { slept: 1 } });
  const name = `secret_url_log_${crypto.randomUUID()}`;

  await client.PUT("/definitions/batch", {
    body: {
      definitions: [
        {
          name,
          config_schema: {
            type: "object",
            required: ["server_url"],
            properties: { server_url: { type: "string", secret: true } },
          },
          tasks: [
            {
              id: "call",
              action: {
                type: "rest",
                endpoint: "{{ config.server_url }}/action",
                result_schema: {
                  type: "object",
                  properties: { slept: { type: "number" } },
                  required: ["slept"],
                },
              },
              output: "{{ self.result }}",
              switch: "end",
            },
          ],
        },
      ],
      channel: "latest",
    },
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
  } as any);

  const { data: startData } = await client.POST("/instances", { body: { process: name } });
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data: logs } = await client.GET("/instances/{id}/logs", { params: { path: { id } } });
  const blob = JSON.stringify(logs);
  // The raw secret host must not be stored anywhere in the trail...
  expect(blob).not.toContain("localhost:14100");
  // ...but the redacted URL is kept (action_started meta.url).
  expect(blob).toContain("***/action");

  mock.stop();
});

// The obscuring is not config-specific: an input_schema secret used to build the
// URL is also redacted in the logs (via RedactContext over the eval context).
test("a secret INPUT value in the endpoint URL is redacted in stored logs", async () => {
  // A random port (not the pinned 14100) — the URL comes from input here, so this
  // test needs no fixed fixture, and keeping 14100 to a single test in this file
  // avoids racing its own not-yet-released listener when Vitest runs the cases.
  const mock = await startMockService(0, { response: { slept: 1 } });
  const name = `secret_input_url_${crypto.randomUUID()}`;

  await client.PUT("/definitions/batch", {
    body: {
      definitions: [
        {
          name,
          input_schema: {
            type: "object",
            required: ["base"],
            properties: { base: { type: "string", secret: true } },
          },
          tasks: [
            {
              id: "call",
              action: {
                type: "rest",
                endpoint: "{{ input.base }}/action",
                result_schema: {
                  type: "object",
                  properties: { slept: { type: "number" } },
                  required: ["slept"],
                },
              },
              output: "{{ self.result }}",
              switch: "end",
            },
          ],
        },
      ],
      channel: "latest",
    },
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
  } as any);

  const base = `http://localhost:${mock.port}`;
  const { data: startData } = await client.POST("/instances", {
    body: { process: name, input: { base } },
  });
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data: logs } = await client.GET("/instances/{id}/logs", { params: { path: { id } } });
  const blob = JSON.stringify(logs);
  expect(blob).not.toContain(`localhost:${mock.port}`);
  expect(blob).toContain("***/action");

  mock.stop();
});

// A secret in the URL must not leak via a failed request's transport error either:
// net/http builds the error from the real URL, but the audit sink find/replaces
// every collected secret value out of the message before the log is stored.
test("a secret in a failed request's transport error is obscured in logs", async () => {
  const name = `secret_err_${crypto.randomUUID()}`;
  await client.PUT("/definitions/batch", {
    body: {
      definitions: [
        {
          name,
          input_schema: {
            type: "object",
            required: ["host"],
            properties: { host: { type: "string", secret: true } },
          },
          // No scheme → "unsupported protocol scheme" error built from the real URL.
          tasks: [{ id: "t", action: { type: "rest", endpoint: "{{ input.host }}/x" }, switch: "end" }],
        },
      ],
      channel: "latest",
    },
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
  } as any);

  const { data: startData } = await client.POST("/instances", {
    body: { process: name, input: { host: "SECRET12345HOST" } },
  });
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("failed");

  const { data: logs } = await client.GET("/instances/{id}/logs", { params: { path: { id } } });
  expect(JSON.stringify(logs)).not.toContain("SECRET12345HOST");
});
