import { expect, test, beforeAll } from "vitest";
import { join } from "path";
import { tmpdir } from "os";
import { buildGentBinary, startGent } from "../helpers/server.ts";
import { startMockService, waitForInstance } from "../helpers/client.ts";

const GENT1_PORT = 20011;
const GENT2_PORT = 20012;

let gentBin: string;

beforeAll(async () => {
  gentBin = await buildGentBinary();
}, 120_000);

test("crash recovery — new worker re-executes an unconfirmed step after the previous worker crashes", async () => {
  const db = join(tmpdir(), `gent_crash_${Date.now()}.db`);

  // firstRequestDelayMs: Infinity keeps the connection open so the step
  // stays in-flight when we crash the worker.
  const mock = await startMockService(0, {
    response: { done: true },
    firstRequestDelayMs: Infinity,
  });

  const gent1 = await startGent(gentBin, GENT1_PORT, db);
  try {
    const processName = `crash_recovery_${crypto.randomUUID()}`;
    await gent1.client.PUT("/definitions", {
      body: {
        name: processName,
        version: 1,
        steps: [
          {
            id: "work",
            call: { type: "rest" as const, endpoint: `http://localhost:${mock.port}/action` },
            // Long enough that the step never times out before the crash.
            timeout_ms: 120_000,
          },
        ],
      },
    });

    const { data: startData } = await gent1.client.POST("/instances", {
      body: { process: processName },
    });
    const instanceId = startData!.id;

    // Wait until gent1 has claimed the instance and the step is in-flight.
    await Promise.race([
      mock.firstRequestReceived,
      new Promise<never>((_, reject) =>
        setTimeout(
          () => reject(new Error("mock never received first request")),
          15_000,
        ),
      ),
    ]);

    // Crash: SIGKILL leaves the lease in the database without releasing it.
    gent1.crash();

    // The engine lease is 10 s. Waiting 12 s ensures it has expired
    // before the next worker polls.
    await new Promise((r) => setTimeout(r, 12_000));

    const gent2 = await startGent(gentBin, GENT2_PORT, db);
    try {
      const finalStatus = await waitForInstance(
        instanceId,
        15_000,
        gent2.client,
      );

      // gent2 must have re-executed the step and completed the instance.
      expect(finalStatus).toBe("completed");
      // Once by gent1 (abandoned at crash), once by gent2 (confirmed).
      expect(mock.requestCount()).toBe(2);
    } finally {
      gent2.stop();
    }
  } finally {
    gent1.crash(); // no-op if already dead
    await mock.stop();
  }
}, 60_000);
