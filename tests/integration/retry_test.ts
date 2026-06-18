import { expect, test, beforeAll } from "vitest";
import { join } from "path";
import { tmpdir } from "os";
import { buildGentBinary, startGent, type GentProcess } from "../helpers/server.ts";
import { client, startMockService, waitForInstance, tick } from "../helpers/client.ts";

const TICK_PORT = 20017;

let gentBin: string;
beforeAll(async () => {
  gentBin = await buildGentBinary();
}, 60_000);

async function getStatus(gent: GentProcess, id: string) {
  const { data, error } = await gent.client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  if (error) throw new Error(`get_instance failed: ${JSON.stringify(error)}`);
  return data!;
}

// failed → retry → completes, without re-executing the task that already succeeded.
test("retry failed instance — resumes from the failed task", async () => {
  const name = `retry_failed_${crypto.randomUUID()}`;
  const step1Mock = await startMockService(0, { response: { ok: true } });
  let step2Mock = await startMockService(0, { statusCode: 500 });
  const step2Port = step2Mock.port;

  try {
    await client.PUT("/definitions", {
      body: {
        name,
        tasks: [
          {
            id: "step1",
            action: { type: "rest" as const, endpoint: `http://localhost:${step1Mock.port}/action` },
            timeout_ms: 2000,
            switch: [{ goto: "next" }],
          },
          {
            id: "step2",
            action: { type: "rest" as const, endpoint: `http://localhost:${step2Port}/action` },
            timeout_ms: 2000,
            switch: [{ goto: "end" }],
          },
        ],
      },
    });

    const { data: startData } = await client.POST("/instances", { body: { process: name } });
    const id = startData!.id;
    expect(await waitForInstance(id, 15_000)).toBe("failed");
    expect(step1Mock.requestCount()).toBe(1);

    // Fix the failing service: restart the mock on the same port with a 200.
    await step2Mock.stop();
    step2Mock = await startMockService(step2Port, { response: { done: true } });

    const { error: retryErr } = await client.POST("/instances/{id}/retry", {
      params: { path: { id } },
    });
    expect(retryErr).toBeUndefined();

    expect(await waitForInstance(id, 15_000)).toBe("completed");
    // step1 was never re-executed — the retry resumed at step2.
    expect(step1Mock.requestCount()).toBe(1);
  } finally {
    await step1Mock.stop();
    await step2Mock.stop();
  }
}, 30_000);

// cancelled → retry → completes; completed tasks are not re-run.
// Manual tick mode makes the cancel land deterministically between tasks.
test("retry cancelled instance — resumes where the cancel interrupted", async () => {
  const name = `retry_cancelled_${crypto.randomUUID()}`;
  const db = join(tmpdir(), `gent_retry_${Date.now()}.db`);
  const gent = await startGent(gentBin, TICK_PORT, db, undefined, 0);

  const step1Mock = await startMockService(0, { response: { ok: true } });
  const step2Mock = await startMockService(0, { response: { done: true } });

  try {
    await gent.client.PUT("/definitions", {
      body: {
        name,
        tasks: [
          {
            id: "step1",
            action: { type: "rest" as const, endpoint: `http://localhost:${step1Mock.port}/action` },
            timeout_ms: 2000,
            switch: [{ goto: "next" }],
          },
          {
            id: "step2",
            action: { type: "rest" as const, endpoint: `http://localhost:${step2Mock.port}/action` },
            timeout_ms: 2000,
            switch: [{ goto: "end" }],
          },
        ],
      },
    });

    const { data: startData } = await gent.client.POST("/instances", { body: { process: name } });
    const id = startData!.id;

    // Tick 1 — step1 executes; cancel lands between tasks.
    expect(await tick(gent.client)).toBe(1);
    expect(step1Mock.requestCount()).toBe(1);
    await gent.client.POST("/instances/{id}/cancel", { params: { path: { id } } });

    // Tick 2 — engine finalises the cancellation.
    await tick(gent.client);
    expect((await getStatus(gent, id)).status).toBe("cancelled");
    expect(step2Mock.requestCount()).toBe(0);

    const { error: retryErr } = await gent.client.POST("/instances/{id}/retry", {
      params: { path: { id } },
    });
    expect(retryErr).toBeUndefined();
    expect((await getStatus(gent, id)).status).toBe("running");

    // Tick 3 — step2 executes; the process completes without re-running step1.
    await tick(gent.client);
    expect((await getStatus(gent, id)).status).toBe("completed");
    expect(step1Mock.requestCount()).toBe(1);
    expect(step2Mock.requestCount()).toBe(1);
  } finally {
    gent.stop();
    await step1Mock.stop();
    await step2Mock.stop();
  }
}, 30_000);

// retry while the tree is still draining ('cancelling') → rejected;
// once it settles to 'cancelled' the same retry succeeds.
test("retry during cancelling — rejected until the tree settles", async () => {
  const name = `retry_cancelling_${crypto.randomUUID()}`;
  const db = join(tmpdir(), `gent_retry_cancelling_${Date.now()}.db`);
  const gent = await startGent(gentBin, TICK_PORT + 1, db, undefined, 0);

  const step1Mock = await startMockService(0, { response: { ok: true } });
  const step2Mock = await startMockService(0, { response: { done: true } });

  try {
    await gent.client.PUT("/definitions", {
      body: {
        name,
        tasks: [
          {
            id: "step1",
            action: { type: "rest" as const, endpoint: `http://localhost:${step1Mock.port}/action` },
            timeout_ms: 2000,
            switch: [{ goto: "next" }],
          },
          {
            id: "step2",
            action: { type: "rest" as const, endpoint: `http://localhost:${step2Mock.port}/action` },
            timeout_ms: 2000,
            switch: [{ goto: "end" }],
          },
        ],
      },
    });

    const { data: startData } = await gent.client.POST("/instances", { body: { process: name } });
    const id = startData!.id;

    // Tick 1 — step1 executes; cancel lands between tasks → 'cancelling'.
    await tick(gent.client);
    await gent.client.POST("/instances/{id}/cancel", { params: { path: { id } } });
    expect((await getStatus(gent, id)).status).toBe("cancelling");

    // Retry while still draining is rejected; status is untouched.
    const { error: earlyErr } = await gent.client.POST("/instances/{id}/retry", {
      params: { path: { id } },
    });
    expect(earlyErr).toBeDefined();
    expect(JSON.stringify(earlyErr)).toContain("not retryable");
    expect((await getStatus(gent, id)).status).toBe("cancelling");

    // Tick 2 — the engine settles the instance to 'cancelled'; now retry works.
    await tick(gent.client);
    expect((await getStatus(gent, id)).status).toBe("cancelled");

    const { error: retryErr } = await gent.client.POST("/instances/{id}/retry", {
      params: { path: { id } },
    });
    expect(retryErr).toBeUndefined();

    // Tick 3 — step2 executes and the process completes.
    await tick(gent.client);
    expect((await getStatus(gent, id)).status).toBe("completed");
    expect(step1Mock.requestCount()).toBe(1);
  } finally {
    gent.stop();
    await step1Mock.stop();
    await step2Mock.stop();
  }
}, 30_000);

// only_once → plain retry rejected, force retry succeeds.
test("retry only_once task — rejected without force, allowed with force", async () => {
  const name = `retry_only_once_${crypto.randomUUID()}`;
  let chargeMock = await startMockService(0, { statusCode: 500 });
  const chargePort = chargeMock.port;

  try {
    await client.PUT("/definitions", {
      body: {
        name,
        tasks: [
          {
            id: "charge",
            only_once: true,
            action: { type: "rest" as const, endpoint: `http://localhost:${chargePort}/action` },
            timeout_ms: 2000,
            switch: [{ goto: "end" }],
          },
        ],
      },
    });

    const { data: startData } = await client.POST("/instances", { body: { process: name } });
    const id = startData!.id;
    expect(await waitForInstance(id, 15_000)).toBe("failed");

    // Plain retry is rejected: the pending task is only_once.
    const { error: plainErr } = await client.POST("/instances/{id}/retry", {
      params: { path: { id } },
    });
    expect(plainErr).toBeDefined();
    expect(JSON.stringify(plainErr)).toContain("only_once");

    // Fix the service, then force the retry.
    await chargeMock.stop();
    chargeMock = await startMockService(chargePort, { response: { ok: true } });

    const { error: forceErr } = await client.POST("/instances/{id}/retry", {
      params: { path: { id }, query: { force: true } },
    });
    expect(forceErr).toBeUndefined();
    expect(await waitForInstance(id, 15_000)).toBe("completed");
  } finally {
    await chargeMock.stop();
  }
}, 30_000);

// retry/cancel on a child instance → rejected with the root's id.
test("retry and cancel on non-root instance — rejected naming the root", async () => {
  const id = crypto.randomUUID();
  const leafName = `nonroot_leaf_${id}`;
  const rootName = `nonroot_root_${id}`;
  const failMock = await startMockService(0, { statusCode: 500 });

  try {
    await client.PUT("/definitions", {
      body: {
        name: leafName,
        tasks: [
          {
            id: "work",
            action: { type: "rest" as const, endpoint: `http://localhost:${failMock.port}/action` },
            timeout_ms: 2000,
            switch: [{ goto: "end" }],
          },
        ],
      },
    });
    await client.PUT("/definitions", {
      body: {
        name: rootName,
        tasks: [
          {
            id: "spawn",
            action: { type: "child" as const, name: leafName },
            switch: [{ goto: "end" }],
          },
        ],
      },
    });

    const { data: startData } = await client.POST("/instances", { body: { process: rootName } });
    const rootId = startData!.id;
    expect(await waitForInstance(rootId, 15_000)).toBe("failed");

    // The spawn placeholder in the root's context (_children) holds the child id.
    const { data: rootData } = await client.GET("/instances/{id}", {
      params: { path: { id: rootId } },
    });
    const childId = (rootData?.context as any)?._children?.spawn as string;
    expect(childId).toBeTruthy();

    const { error: retryErr } = await client.POST("/instances/{id}/retry", {
      params: { path: { id: childId } },
    });
    expect(retryErr).toBeDefined();
    expect(JSON.stringify(retryErr)).toContain(rootId);

    const { error: cancelErr } = await client.POST("/instances/{id}/cancel", {
      params: { path: { id: childId } },
    });
    expect(cancelErr).toBeDefined();
    expect(JSON.stringify(cancelErr)).toContain(rootId);
  } finally {
    await failMock.stop();
  }
}, 30_000);

// parallel children, one failed → root retry re-runs only the failed child.
test("retry with parallel children — only the failed child re-runs", async () => {
  const id = crypto.randomUUID();
  const goodName = `par_good_${id}`;
  const badName = `par_bad_${id}`;
  const rootName = `par_root_${id}`;

  const goodMock = await startMockService(0, { response: { ok: true } });
  let badMock = await startMockService(0, { statusCode: 500 });
  const badPort = badMock.port;

  try {
    await client.PUT("/definitions", {
      body: {
        name: goodName,
        tasks: [
          {
            id: "work",
            action: { type: "rest" as const, endpoint: `http://localhost:${goodMock.port}/action` },
            timeout_ms: 2000,
            switch: [{ goto: "end" }],
          },
        ],
      },
    });
    await client.PUT("/definitions", {
      body: {
        name: badName,
        tasks: [
          {
            id: "work",
            action: { type: "rest" as const, endpoint: `http://localhost:${badPort}/action` },
            timeout_ms: 2000,
            switch: [{ goto: "end" }],
          },
        ],
      },
    });
    await client.PUT("/definitions", {
      body: {
        name: rootName,
        tasks: [
          {
            id: "fanout",
            action: {
              type: "child_parallel" as const,
              children: {
                good: { name: goodName },
                bad: { name: badName },
              },
            },
            switch: [{ goto: "end" }],
          },
        ],
      },
    });

    const { data: startData } = await client.POST("/instances", { body: { process: rootName } });
    const rootId = startData!.id;
    expect(await waitForInstance(rootId, 15_000)).toBe("failed");
    expect(goodMock.requestCount()).toBe(1);

    // Fix the failing service and retry the root.
    await badMock.stop();
    badMock = await startMockService(badPort, { response: { ok: true } });

    const { error: retryErr } = await client.POST("/instances/{id}/retry", {
      params: { path: { id: rootId } },
    });
    expect(retryErr).toBeUndefined();

    expect(await waitForInstance(rootId, 15_000)).toBe("completed");
    // The completed child was never re-executed.
    expect(goodMock.requestCount()).toBe(1);
  } finally {
    await goodMock.stop();
    await badMock.stop();
  }
}, 30_000);

// /tick is a manual-mode tool: when the continuous pump is running (poll > 0),
// an out-of-band tick would race it, so the endpoint refuses.
test("tick is rejected when the engine runs the continuous pump", async () => {
  const db = join(tmpdir(), `gent_tick_guard_${Date.now()}.db`);
  // No poll arg → server uses its default poll interval (continuous mode).
  const gent = await startGent(gentBin, TICK_PORT + 2, db);
  try {
    const { error } = await gent.client.POST("/tick", { body: { advance_ms: 0 } });
    expect(error).toBeDefined();
    expect(JSON.stringify(error)).toContain("manual mode");
  } finally {
    gent.stop();
  }
}, 30_000);
