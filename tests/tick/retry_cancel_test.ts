/**
 * Tests that observe how cancellation interacts with on_error retries.
 *
 * Two scenarios:
 *
 *   1. Cancel issued while a retry is still pending
 *      → advance() sees 'cancelling' on the retry tick and short-circuits before
 *        executing the step — the instance cancels cleanly, retry is suppressed.
 *
 *   2. All retries exhausted before any cancel
 *      → the final failed attempt calls failInstance(); status is 'failed'.
 *        A subsequent cancel has no effect — errors take precedence.
 *
 * Both processes run as root instances (no tree) so the timing is straightforward.
 * The server is started with --immediate-retries so retries are claimable on the
 * very next tick, with no backoff delay to wait for.
 */
import { expect, test, beforeAll, afterAll } from "vitest";
import { startMockService } from "../helpers/client.ts";
import { useTickEnv } from "./helpers.ts";

const PORT = 20016;
const ctx = useTickEnv(PORT);

let failMockPort: number;
let stopMock: (() => Promise<void>) | undefined;
let withRetriesName: string;
let exhaustedName: string;

beforeAll(async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  withRetriesName = `with_retries_${uid}`;
  exhaustedName = `exhausted_${uid}`;

  const failMock = await startMockService(0, { statusCode: 500 });
  failMockPort = failMock.port;
  stopMock = failMock.stop;

  // Process with 2 retries — three total attempts before permanent failure.
  await ctx.env.define(withRetriesName, [
    {
      id: "work",
      call: {
        type: "rest" as const,
        endpoint: `http://localhost:${failMockPort}/action`,
      },
      on_error: [{ code: ["http.%"], retries: 2 }],
      timeout_ms: 5_000,
      switch: [{ goto: "end" }],
    },
  ]);

  // Process with 1 retry — two total attempts, then permanent failure.
  await ctx.env.define(exhaustedName, [
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

afterAll(() => stopMock?.());

test("cancel while retry is pending — retry suppressed, instance cancels", async () => {
  const id = await ctx.env.start(withRetriesName);
  try {
    // tick: attempt 1 fails → retry scheduled (status stays 'running', next_retry_at set)
    await ctx.env.tick();
    expect(await ctx.env.status(id)).toBe("running");

    // Cancel while the retry timer is counting down.
    await ctx.env.cancel(id);
    expect(await ctx.env.status(id)).toBe("cancelling");

    // tick: retry backoff is 0 (--immediate-retries), so the instance is claimable now.
    // advance() sees status='cancelling' → cancelInstance() — the retry never fires.
    await ctx.env.tick();
    expect(await ctx.env.status(id)).toBe("cancelled");
  } finally {
    await ctx.env.tickUntilIdle();
  }
});

test("retries exhausted — process fails; cancel issued after has no effect", async () => {
  const id = await ctx.env.start(exhaustedName);
  try {
    // tick: attempt 1 fails → retry 1 scheduled (retries: 1, so one more attempt allowed)
    await ctx.env.tick();
    expect(await ctx.env.status(id)).toBe("running");

    // tick: no backoff, attempt 2 fires immediately and fails.
    // RetryCount now equals Retries — no more retries available → failInstance.
    await ctx.env.tick();
    expect(await ctx.env.status(id)).toBe("failed");

    // Attempting to cancel an already-failed instance has no effect.
    await ctx.env.cancel(id);
    expect(await ctx.env.status(id)).toBe("failed");
  } finally {
    await ctx.env.tickUntilIdle();
  }
});
