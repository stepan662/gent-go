import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

// Verifies that a cancel arriving while step N is executing takes effect after
// step N completes: step N+1 is never started, and the instance ends up cancelled.
// This exercises the UpdateInstanceProgress path — it preserves the DB status
// (cancelling) rather than overwriting it with 'running', so the next tick
// sees 'cancelling' and stops cleanly.
test("cancel during active step — next step is not executed, instance becomes cancelled", async () => {
  const id = crypto.randomUUID();
  const processName = `cancel_between_steps_${id}`;

  // step1Mock holds the first request until release() is called.
  const step1Mock = await startMockService(0, {
    response: { ok: true },
    firstRequestDelayMs: Infinity,
  });
  // step2Mock should never receive a request if cancel works correctly.
  const step2Mock = await startMockService(0, { response: { done: true } });

  await client.PUT("/definitions", {
    body: {
      name: processName,
      steps: [
        {
          id: "step1",
          call: {
            type: "rest" as const,
            endpoint: `http://localhost:${step1Mock.port}/action`,
          },
          timeout_ms: 10_000,
          switch: [{ goto: "next" }],
        },
        {
          id: "step2",
          call: {
            type: "rest" as const,
            endpoint: `http://localhost:${step2Mock.port}/action`,
          },
          timeout_ms: 5_000,
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  const { data, error } = await client.POST("/instances", {
    body: { process: processName },
  });
  expect(error).toBeUndefined();
  expect(data).toBeDefined();

  // Wait until the worker is mid-execution of step1.
  await step1Mock.firstRequestReceived;

  // Cancel while step1 is still executing. The DB is now 'cancelling'.
  const { error: cancelError } = await client.POST("/instances/{id}/cancel", {
    params: { path: { id: data!.id } },
  });
  expect(cancelError).toBeUndefined();

  // Let step1's HTTP response return. The engine writes the updated queue via
  // UpdateInstanceProgress without touching status — DB stays 'cancelling'.
  step1Mock.release();

  // The instance must end up cancelled and step2 must never have been called.
  const status = await waitForInstance(data!.id, 10_000);
  expect(status).toBe("cancelled");
  expect(step2Mock.requestCount()).toBe(0);

  step1Mock.stop();
  step2Mock.stop();
});
