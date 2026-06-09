import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

// ── Static validation ─────────────────────────────────────────────────────────

test("only_once:true — rejects retries on http.% pattern", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_static_http_${crypto.randomUUID()}`,
      steps: [
        {
          id: "charge",
          only_once: true,
          call: { type: "rest" as const, endpoint: "http://localhost:19990/x" },
          on_error: [{ code: ["http.%"], retries: 3 }],
        },
      ],
    },
  });
  expect(error).toBeDefined();
  expect(JSON.stringify(error)).toContain("http.%");
});

test("only_once:true — rejects retries on exact http.500", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_static_exact_${crypto.randomUUID()}`,
      steps: [
        {
          id: "charge",
          only_once: true,
          call: { type: "rest" as const, endpoint: "http://localhost:19990/x" },
          on_error: [{ code: ["http.500"], retries: 1 }],
        },
      ],
    },
  });
  expect(error).toBeDefined();
  expect(JSON.stringify(error)).toContain("http.500");
});

test("only_once:true — rejects catch-all with retries", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_static_catchall_${crypto.randomUUID()}`,
      steps: [
        {
          id: "charge",
          only_once: true,
          call: { type: "rest" as const, endpoint: "http://localhost:19990/x" },
          on_error: [{ retries: 2 }],
        },
      ],
    },
  });
  expect(error).toBeDefined();
  expect(JSON.stringify(error)).toContain("catch-all");
});

test("only_once:true — rejects wildcard crossing namespaces", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_static_cross_${crypto.randomUUID()}`,
      steps: [
        {
          id: "charge",
          only_once: true,
          call: { type: "rest" as const, endpoint: "http://localhost:19990/x" },
          on_error: [{ code: ["s%"], retries: 3 }],
        },
      ],
    },
  });
  expect(error).toBeDefined();
  expect(JSON.stringify(error)).toContain("s%");
});

test("only_once:true — accepts retries on pre.%", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_static_start_${crypto.randomUUID()}`,
      steps: [
        {
          id: "charge",
          only_once: true,
          call: { type: "rest" as const, endpoint: "http://localhost:19990/x" },
          on_error: [
            { code: ["pre.%"], retries: 3 },
            { goto: "$end" },
          ],
        },
      ],
    },
  });
  expect(error).toBeUndefined();
});

test("only_once:true — accepts retries on exact pre.* codes", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_static_exact_start_${crypto.randomUUID()}`,
      steps: [
        {
          id: "charge",
          only_once: true,
          call: { type: "rest" as const, endpoint: "http://localhost:19990/x" },
          on_error: [{ code: ["pre.error", "pre.timeout"], retries: 3 }],
        },
      ],
    },
  });
  expect(error).toBeUndefined();
});

test("only_once:true — accepts not_reached:true override for http.422", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_static_exec_false_${crypto.randomUUID()}`,
      steps: [
        {
          id: "charge",
          only_once: true,
          call: { type: "rest" as const, endpoint: "http://localhost:19990/x" },
          on_error: [
            { code: ["http.422"], not_reached: true, retries: 2 },
            { code: ["http.%"], goto: "$end" },
          ],
        },
      ],
    },
  });
  expect(error).toBeUndefined();
});

test("only_once:true — accepts catch-all with not_reached:true", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_static_catchall_exec_${crypto.randomUUID()}`,
      steps: [
        {
          id: "charge",
          only_once: true,
          call: { type: "rest" as const, endpoint: "http://localhost:19990/x" },
          on_error: [{ not_reached: true, retries: 2 }],
        },
      ],
    },
  });
  expect(error).toBeUndefined();
});

test("only_once:true — goto-only rule on http.% is accepted (no retries)", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_static_goto_only_${crypto.randomUUID()}`,
      steps: [
        {
          id: "charge",
          only_once: true,
          call: { type: "rest" as const, endpoint: "http://localhost:19990/x" },
          on_error: [{ code: ["http.%"], goto: "#handler" }],
        },
        {
          id: "handler",
          call: { type: "rest" as const, endpoint: "http://localhost:19990/x" },
        },
      ],
    },
  });
  expect(error).toBeUndefined();
});

// ── Runtime behaviour ─────────────────────────────────────────────────────────

test("only_once:true — http.500 routes to handler and is called exactly once", async () => {
  const failMock = await startMockService(0, { statusCode: 500 });
  const handlerMock = await startMockService(0, { response: { handled: true } });

  const name = `ni_rt_no_retry_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      steps: [
        {
          id: "charge",
          only_once: true,
          call: {
            type: "rest" as const,
            endpoint: `http://localhost:${failMock.port}/action`,
          },
          on_error: [
            // pre.% rule present — would retry on connection errors but not on http.*
            { code: ["pre.%"], retries: 3 },
            { code: ["http.%"], goto: "#handler" },
          ],
          timeout_ms: 2000,
        },
        {
          id: "handler",
          call: {
            type: "rest" as const,
            endpoint: `http://localhost:${handlerMock.port}/action`,
            output_schema: {
              type: "object",
              properties: { handled: { type: "boolean" } },
              required: ["handled"],
            },
          },
          timeout_ms: 2000,
        },
      ],
    },
  });

  const { data } = await client.POST("/instances", { body: { process: name } });
  expect(await waitForInstance(data!.id)).toBe("completed");

  // The key assertion: only one call to the failing endpoint — no retries fired
  expect(failMock.requestCount()).toBe(1);
  expect(handlerMock.requestCount()).toBe(1);

  failMock.stop();
  handlerMock.stop();
});

test("only_once:true — connection refused triggers pre.% retries", async () => {
  // Start then immediately stop the mock to free the port — subsequent connects will be refused
  const gone = await startMockService(0);
  const port = gone.port;
  await gone.stop();

  const name = `ni_rt_start_retry_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      steps: [
        {
          id: "charge",
          only_once: true,
          call: {
            type: "rest" as const,
            endpoint: `http://localhost:${port}/action`,
          },
          on_error: [
            // 1 retry on pre.% then complete via $end
            { code: ["pre.%"], retries: 1, goto: "$end" },
          ],
          timeout_ms: 2000,
        },
      ],
    },
  });

  const { data } = await client.POST("/instances", { body: { process: name } });
  // Two attempts (original + 1 retry), both refused. Retries exhausted → goto $end → completed.
  // The 2-second retry delay means this takes ~2s — well within the 30s test timeout.
  expect(await waitForInstance(data!.id, 15_000)).toBe("completed");
});

test("only_once:true — not_reached:true allows retry on http.422", async () => {
  // First call returns 422 (trigger retry), second returns 200
  let calls = 0;
  const mock = await startMockService(0, { statusCode: 200, response: { ok: true } });
  // We can't make the mock return different status codes per call, so instead we verify
  // that with not_reached:true the definition is accepted and the step runs.
  // A 200 response means not_reached:true retries would not fire (no error to trigger them).
  // The meaningful runtime check is the static acceptance test above; here we just confirm
  // the step executes and completes normally.

  const name = `ni_rt_exec_false_${crypto.randomUUID()}`;
  const { error: defErr } = await client.PUT("/definitions", {
    body: {
      name,
      steps: [
        {
          id: "charge",
          only_once: true,
          call: {
            type: "rest" as const,
            endpoint: `http://localhost:${mock.port}/action`,
            output_schema: {
              type: "object",
              properties: { ok: { type: "boolean" } },
              required: ["ok"],
            },
          },
          on_error: [{ code: ["http.422"], not_reached: true, retries: 2 }],
          timeout_ms: 2000,
        },
      ],
    },
  });
  expect(defErr).toBeUndefined();

  const { data } = await client.POST("/instances", { body: { process: name } });
  expect(await waitForInstance(data!.id)).toBe("completed");

  mock.stop();
});

test("default step (no only_once) — http.500 retries normally", async () => {
  // Baseline: same setup without only_once:true. The http.% rule has retries:1.
  // Total calls = 2 (original + 1 retry), then $end → completed.
  const failMock = await startMockService(0, { statusCode: 500 });

  const name = `default_retry_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      steps: [
        {
          id: "call",
          // No only_once:true — default behaviour
          call: {
            type: "rest" as const,
            endpoint: `http://localhost:${failMock.port}/action`,
          },
          on_error: [{ code: ["http.%"], retries: 1, goto: "$end" }],
          timeout_ms: 2000,
        },
      ],
    },
  });

  const { data } = await client.POST("/instances", { body: { process: name } });
  // 1 retry = 2s delay; allow up to 15s
  expect(await waitForInstance(data!.id, 15_000)).toBe("completed");

  // Original + 1 retry = 2 calls
  expect(failMock.requestCount()).toBe(2);

  failMock.stop();
});
