/**
 * Tests that observe how errors interact with cancellation in a 3-level process tree:
 *
 *   grandparent
 *     └─ parent  (child call)
 *          ├─ a  (child_parallel)  ← always calls failWorker → HTTP 500 → fails
 *          └─ b  (child_parallel)  ← calls successWorker → HTTP 200 → completes
 *
 * Key invariant: errors take precedence over cancellation.
 *   - FailAncestors marks ancestors as 'failed' even if they were 'cancelling waiting'.
 *   - FinishChild for a cancelled/completed sibling does NOT overwrite the parent's
 *     'failed' status (parent.wait_state is '' after FailAncestors, so SetParentCollecting
 *     never fires).
 *
 * Same server/tick/ordering conventions as tree_cancel_test.ts; see that file for details.
 *
 * buildTree() leaves the tree at:
 *   gp="running waiting", parent="running waiting", a="running", b="running"
 */
import { expect, test, beforeAll, afterAll } from "vitest";
import { startMockService } from "../helpers/client.ts";
import { useTickEnv } from "./helpers.ts";

const PORT = 20015;
const ctx = useTickEnv(PORT);

let failMockPort: number;
let successMockPort: number;
let stopMocks: (() => Promise<void>) | undefined;
let failWorkerName: string;
let successWorkerName: string;
let parentName: string;
let gpName: string;

beforeAll(async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  failWorkerName = `fail_worker_${uid}`;
  successWorkerName = `success_worker_${uid}`;
  parentName = `parent_${uid}`;
  gpName = `gp_${uid}`;

  const failMock = await startMockService(0, { statusCode: 500 });
  const successMock = await startMockService(0, { response: { ok: true } });
  failMockPort = failMock.port;
  successMockPort = successMock.port;
  stopMocks = async () => {
    await failMock.stop();
    await successMock.stop();
  };

  await ctx.env.define(failWorkerName, [
    {
      id: "work",
      call: {
        type: "rest" as const,
        endpoint: `http://localhost:${failMockPort}/action`,
      },
      timeout_ms: 5_000,
      switch: [{ goto: "end" }],
    },
  ]);

  await ctx.env.define(successWorkerName, [
    {
      id: "work",
      call: {
        type: "rest" as const,
        endpoint: `http://localhost:${successMockPort}/action`,
      },
      timeout_ms: 5_000,
      switch: [{ goto: "end" }],
    },
  ]);

  await ctx.env.define(parentName, [
    {
      id: "run_children",
      call: {
        type: "child_parallel" as const,
        children: {
          a: { name: failWorkerName },
          b: { name: successWorkerName },
        },
      },
      switch: [{ goto: "end" }],
    },
  ]);

  await ctx.env.define(gpName, [
    {
      id: "run_parent",
      call: { type: "child" as const, name: parentName },
      switch: [{ goto: "end" }],
    },
  ]);
}, 60_000);

afterAll(() => stopMocks?.());

// Builds the full tree and leaves it at:
//   gp="running waiting", parent="running waiting", a="running", b="running"
async function buildTree() {
  const gp = await ctx.env.start(gpName);

  // tick: gp spawns parent → gp transitions to running+wait_state=waiting
  await ctx.env.tick();
  const parent = await ctx.env.childOf(gp, "run_parent");

  // tick: parent spawns a and b → parent transitions to running+wait_state=waiting
  await ctx.env.tick();
  const { a, b } = await ctx.env.childrenOf(parent, "run_children");

  expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
    gp: "running waiting",
    parent: "running waiting",
    a: "running",
    b: "running",
  });

  return { gp, parent, a, b };
}

test("a fails — FailAncestors cascades to parent and gp; completed sibling leaves them failed", async () => {
  const { gp, parent, a, b } = await buildTree();
  try {
    // tick: a (smaller created_at) is claimed and executed; its REST call returns 500.
    // failInstance(a) → FailAncestors: parent and gp set to 'failed', wait_state=''.
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "failed",
      parent: "failed",
      a: "failed",
      b: "running",
    });

    // tick: b runs and completes normally.
    // FinishChild(b) reads parent.wait_state='' → no wakeup; parent stays failed.
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "failed",
      parent: "failed",
      a: "failed",
      b: "completed",
    });
  } finally {
    await ctx.env.tickUntilIdle();
  }
});

test("a fails while ancestors are cancelling — FailAncestors overrides 'cancelling'; cancelled sibling leaves parent failed", async () => {
  // A separate tree whose failing worker holds its first request open, so the
  // root can be cancelled while a's call is still in flight. The failure then
  // lands on 'cancelling' ancestors and must override them to 'failed'.
  const uid = crypto.randomUUID().slice(0, 8);
  const holdMock = await startMockService(0, {
    statusCode: 500,
    firstRequestDelayMs: Infinity,
  });
  try {
    const holdWorker = `hold_worker_${uid}`;
    await ctx.env.define(holdWorker, [
      {
        id: "work",
        call: {
          type: "rest" as const,
          endpoint: `http://localhost:${holdMock.port}/action`,
        },
        timeout_ms: 5_000,
        switch: [{ goto: "end" }],
      },
    ]);
    const parent2Name = `parent2_${uid}`;
    await ctx.env.define(parent2Name, [
      {
        id: "run_children",
        call: {
          type: "child_parallel" as const,
          children: {
            a: { name: holdWorker },
            b: { name: successWorkerName },
          },
        },
        switch: [{ goto: "end" }],
      },
    ]);
    const gp2Name = `gp2_${uid}`;
    await ctx.env.define(gp2Name, [
      {
        id: "run_parent",
        call: { type: "child" as const, name: parent2Name },
        switch: [{ goto: "end" }],
      },
    ]);

    const gp = await ctx.env.start(gp2Name);
    await ctx.env.tick();
    const parent = await ctx.env.childOf(gp, "run_parent");
    await ctx.env.tick();
    const { a, b } = await ctx.env.childrenOf(parent, "run_children");

    // Tick 3 claims a (spawned first); its call hangs on the held mock.
    const tickPromise = ctx.env.tick();
    await holdMock.firstRequestReceived;

    // Cancel the root while a's call is in flight — the whole tree (including
    // the claimed a, whose DB row is still 'running') becomes 'cancelling'.
    await ctx.env.cancel(gp);

    // Release the held 500. The engine still holds a as in-memory 'running',
    // so the failure path runs: failInstance(a) → FailAncestors:
    // WHERE status IN ('running', 'cancelling') — the cancelling ancestors are
    // overridden to 'failed', clearing their wait_state.
    holdMock.release();
    await tickPromise;

    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "failed",
      parent: "failed",
      a: "failed",
      b: "cancelling",
    });

    // tick: b (cancelling) is processed → cancelInstance → cancelled.
    // FinishChild(b): parent.wait_state='' (cleared by FailAncestors) → no wakeup.
    // Parent and grandparent remain failed.
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "failed",
      parent: "failed",
      a: "failed",
      b: "cancelled",
    });

    // The error from 'a' is propagated to ancestors via FailAncestors.
    const { data: parentInst } = await ctx.env.client.GET("/instances/{id}", {
      params: { path: { id: parent } },
    });
    expect(parentInst?.error).toBeTruthy();
  } finally {
    await ctx.env.tickUntilIdle();
    await holdMock.stop();
  }
});
