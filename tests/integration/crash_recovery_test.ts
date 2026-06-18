import { expect, test, beforeAll, afterAll } from "vitest";
import { join } from "path";
import { tmpdir } from "os";
import { spawnSync } from "child_process";
import { buildGentBinary, startGent } from "../helpers/server.ts";
import { startMockService, waitForInstance } from "../helpers/client.ts";

// The sqlite and postgres vitest projects run this file in parallel, and both read
// the global POSTGRES_DSN, so offset the (otherwise fixed) gent ports per project
// to keep their own gent1/gent2 processes from colliding.
const PORT_OFFSET = (Number(process.env.GENT_PORT ?? 8888) - 8888) * 4;
const GENT1_PORT = 20011 + PORT_OFFSET;
const GENT2_PORT = 20012 + PORT_OFFSET;

let gentBin: string;
let crashPgDSN: string | undefined;
let tempDbName: string | undefined;

function replaceDbName(dsn: string, dbName: string): string {
  const url = new URL(dsn);
  url.pathname = `/${dbName}`;
  return url.toString();
}

beforeAll(async () => {
  gentBin = await buildGentBinary();

  const rawDsn = process.env.POSTGRES_DSN;
  if (rawDsn) {
    tempDbName = `gent_crash_${Date.now()}`;
    const adminDsn = replaceDbName(rawDsn, "postgres");
    const result = spawnSync(
      "psql",
      [adminDsn, "-c", `CREATE DATABASE ${tempDbName}`],
      {
        stdio: "pipe",
      },
    );
    if (result.status !== 0) {
      throw new Error(
        `Failed to create crash recovery database: ${result.stderr.toString()}`,
      );
    }
    crashPgDSN = replaceDbName(rawDsn, tempDbName);
  }
}, 120_000);

afterAll(() => {
  if (tempDbName) {
    const adminDsn = replaceDbName(process.env.POSTGRES_DSN!, "postgres");
    spawnSync(
      "psql",
      [adminDsn, "-c", `DROP DATABASE ${tempDbName} WITH (FORCE)`],
      { stdio: "pipe" },
    );
  }
});

test("crash recovery — new worker re-executes an unconfirmed task after the previous worker crashes", async () => {
  const db = crashPgDSN ? "" : join(tmpdir(), `gent_crash_${Date.now()}.db`);

  // firstRequestDelayMs: Infinity keeps the connection open so the task
  // stays in-flight when we crash the worker.
  const mock = await startMockService(0, {
    response: { done: true },
    firstRequestDelayMs: Infinity,
  });

  const gent1 = await startGent(gentBin, GENT1_PORT, db, crashPgDSN);
  try {
    const processName = `crash_recovery_${crypto.randomUUID()}`;
    await gent1.client.PUT("/definitions", {
      body: {
        name: processName,

        tasks: [
          {
            id: "work",
            action: {
              type: "rest" as const,
              endpoint: `http://localhost:${mock.port}/action`,
            },
            // Long enough that the task never times out before the crash.
            timeout_ms: 120_000,
            switch: [{ goto: "end" }],
          },
        ],
      },
    });

    const { data: startData } = await gent1.client.POST("/instances", {
      body: { process: processName },
    });
    const instanceId = startData!.id;

    // Wait until gent1 has claimed the instance and the task is in-flight.
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

    // Manual-tick mode (--poll 0): /tick is only available when the continuous
    // pump is off, and it lets us drive reclaim deterministically.
    const gent2 = await startGent(gentBin, GENT2_PORT, db, crashPgDSN, 0);
    // The engine lease is 10 s. Instead of waiting it out, shift gent2's
    // clock forward so gent1's lease is already expired from its view,
    // and tick immediately so it reclaims the instance.
    await gent2.client.POST("/tick", { body: { advance_ms: 12_000 } });
    try {
      const finalStatus = await waitForInstance(
        instanceId,
        15_000,
        gent2.client,
      );

      // gent2 must have re-executed the task and completed the instance.
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

test("crash recovery — an only_once task is failed (not re-executed) after a lease takeover", async () => {
  const db = crashPgDSN ? "" : join(tmpdir(), `gent_crash_once_${Date.now()}.db`);

  // The first request hangs so the task is in-flight when we crash the worker.
  const mock = await startMockService(0, {
    response: { done: true },
    firstRequestDelayMs: Infinity,
  });

  const gent1 = await startGent(gentBin, GENT1_PORT, db, crashPgDSN);
  try {
    const processName = `crash_only_once_${crypto.randomUUID()}`;
    await gent1.client.PUT("/definitions", {
      body: {
        name: processName,
        tasks: [
          {
            id: "work",
            action: {
              type: "rest" as const,
              endpoint: `http://localhost:${mock.port}/action`,
            },
            // only_once: the engine must not re-run this on a lease takeover, since
            // the call may already have happened on the crashed worker.
            only_once: true,
            timeout_ms: 120_000,
            switch: [{ goto: "end" }],
          },
        ],
      },
    });

    const { data: startData } = await gent1.client.POST("/instances", {
      body: { process: processName },
    });
    const instanceId = startData!.id;

    // Wait until gent1 has claimed the instance and the task is in-flight.
    await Promise.race([
      mock.firstRequestReceived,
      new Promise<never>((_, reject) =>
        setTimeout(
          () => reject(new Error("mock never received first request")),
          15_000,
        ),
      ),
    ]);

    gent1.crash();

    const gent2 = await startGent(gentBin, GENT2_PORT, db, crashPgDSN, 0);
    await gent2.client.POST("/tick", { body: { advance_ms: 12_000 } });
    try {
      const finalStatus = await waitForInstance(
        instanceId,
        15_000,
        gent2.client,
      );

      // gent2 detected the takeover and refused to re-execute the only_once task.
      expect(finalStatus).toBe("failed");
      const { data } = await gent2.client.GET("/instances/{id}", {
        params: { path: { id: instanceId } },
      });
      expect(data!.error).toContain("only_once");
      // Only gent1's abandoned attempt — gent2 never sent the request.
      expect(mock.requestCount()).toBe(1);
    } finally {
      gent2.stop();
    }
  } finally {
    gent1.crash();
    await mock.stop();
  }
}, 60_000);
