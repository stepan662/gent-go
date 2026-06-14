/**
 * End-to-end coverage for the per-instance execution audit trail
 * (GET /instances/{id}/logs).
 *
 * Runs in manual-tick mode with --immediate-retries (via useTickEnv) so retries
 * are claimable on the very next tick with no backoff wait.
 *
 * Covers:
 *   1. A successful run records step_started → step_succeeded → step_completed
 *      → instance_completed, and step_succeeded carries a (capped) response snippet.
 *   2. A failing step with one retry records retry_scheduled (warn) then
 *      instance_failed; the level filter narrows to just the warn entry.
 *   3. Time-based pruning: advancing the clock past the retention window drops
 *      old log rows on the next tick.
 */
import { expect, test, beforeAll, afterAll } from "vitest";
import { startMockService } from "../helpers/client.ts";
import { useTickEnv } from "./helpers.ts";

const PORT = 20018;
const ctx = useTickEnv(PORT);

let okMockPort: number;
let failMockPort: number;
let stopOk: (() => Promise<void>) | undefined;
let stopFail: (() => Promise<void>) | undefined;
let okProc: string;
let failProc: string;

beforeAll(async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  okProc = `logs_ok_${uid}`;
  failProc = `logs_fail_${uid}`;

  const okMock = await startMockService(0, { statusCode: 200, response: { ok: true } });
  okMockPort = okMock.port;
  stopOk = okMock.stop;

  const failMock = await startMockService(0, { statusCode: 500 });
  failMockPort = failMock.port;
  stopFail = failMock.stop;

  // Two-step happy path so step_completed (mid-process routing) also appears.
  await ctx.env.define(okProc, [
    {
      id: "first",
      call: {
        type: "rest" as const,
        endpoint: `http://localhost:${okMockPort}/action`,
        output_schema: {
          type: "object",
          properties: { ok: { type: "boolean" } },
        },
      },
      timeout_ms: 5_000,
      switch: [{ goto: "$second" }],
    },
    {
      id: "second",
      call: {
        type: "rest" as const,
        endpoint: `http://localhost:${okMockPort}/action`,
      },
      timeout_ms: 5_000,
      switch: [{ goto: "end" }],
    },
  ]);

  // One retry → two attempts, then permanent failure.
  await ctx.env.define(failProc, [
    {
      id: "work",
      call: {
        type: "rest" as const,
        endpoint: `http://localhost:${failMockPort}/action`,
      },
      on_error: [{ code: ["http.%"], retries: 1 }],
      timeout_ms: 5_000,
      switch: [{ goto: "end" }],
    },
  ]);
}, 60_000);

afterAll(async () => {
  await stopOk?.();
  await stopFail?.();
});

async function getLogs(
  id: string,
  query?: { level?: "debug" | "info" | "warn" | "error"; tree?: boolean },
) {
  const { data, error } = await ctx.env.client.GET("/instances/{id}/logs", {
    params: { path: { id }, query },
  });
  if (error) throw new Error(`get logs failed: ${JSON.stringify(error)}`);
  return data!;
}

test("successful run records step and completion events with response snippet", async () => {
  const id = await ctx.env.start(okProc);
  await ctx.env.tickUntilIdle();
  expect(await ctx.env.status(id)).toBe("completed");

  const logs = await getLogs(id);
  const events = logs.map((l) => l.event);

  // Oldest-first ordering, full lifecycle of a two-step run.
  expect(events).toEqual([
    "step_started",
    "step_succeeded",
    "step_completed",
    "step_started",
    "step_succeeded",
    "instance_completed",
  ]);

  // step_succeeded for the first step carries a truncated response snippet.
  const firstSucceeded = logs.find((l) => l.event === "step_succeeded");
  expect(firstSucceeded?.step).toBe("first");
  expect(JSON.stringify(firstSucceeded?.detail)).toContain("ok");

  // step_started captures the call type.
  const firstStarted = logs.find((l) => l.event === "step_started");
  expect(JSON.stringify(firstStarted?.detail)).toContain("rest");
});

test("failing step records retry_scheduled then instance_failed; level filter narrows", async () => {
  const id = await ctx.env.start(failProc);
  await ctx.env.tickUntilIdle();
  expect(await ctx.env.status(id)).toBe("failed");

  const logs = await getLogs(id);
  const events = logs.map((l) => l.event);
  expect(events).toContain("retry_scheduled");
  expect(events).toContain("instance_failed");

  const retry = logs.find((l) => l.event === "retry_scheduled");
  expect(retry?.level).toBe("warn");
  expect(retry?.code).toMatch(/^http\./);
  expect(retry?.detail).toMatchObject({ attempt: 1, max: 1 });

  // Level filter returns only the warn-level entry.
  const warns = await getLogs(id, { level: "warn" });
  expect(warns).toHaveLength(1);
  expect(warns[0].event).toBe("retry_scheduled");
});

// Must run last: pruning is global and the clock advance persists for this server.
test("clock advance past retention prunes old logs on the next tick", async () => {
  const id = await ctx.env.start(okProc);
  await ctx.env.tickUntilIdle();
  expect((await getLogs(id)).length).toBeGreaterThan(0);

  // Default retention is 168h; jump well past it, then tick (prune runs first).
  const { error } = await ctx.env.client.POST("/tick", {
    body: { advance_ms: 200 * 60 * 60 * 1000 },
  });
  if (error) throw new Error(`tick failed: ${JSON.stringify(error)}`);

  expect(await getLogs(id)).toHaveLength(0);
});
