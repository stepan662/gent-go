/**
 * Tests that observe how cancellation propagates through a 3-level process tree:
 *
 *   grandparent
 *     └─ parent  (child call)
 *          ├─ a  (child_parallel)
 *          └─ b  (child_parallel)
 *
 * The server runs in manual-tick mode (--poll 0, --max-concurrent 1) so every
 * DB state transition is inspectable between ticks.
 *
 * status() returns "status wait_state".trim(), e.g. "running waiting", "cancelling collecting".
 * Instances with no wait_state show just their status, e.g. "running", "cancelled".
 *
 * ClaimInstances uses ORDER BY created_at ASC, and SpawnChildrenAndWait assigns
 * each sibling a strictly increasing created_at (now+0, now+1, …), so parallel
 * siblings are always processed in definition order: a before b.
 *
 * buildTree() leaves the tree at:
 *   gp="running waiting", parent="running waiting", a="running", b="running"
 */
import { expect, test, beforeAll, afterAll } from "vitest";
import { startMockService } from "../helpers/client.ts";
import { useTickEnv } from "./helpers.ts";

const PORT = 20014;
const ctx = useTickEnv(PORT);

let mockPort: number;
let stopMock: () => Promise<void>;
let workerName: string;
let parentName: string;
let gpName: string;

beforeAll(async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  workerName = `worker_${uid}`;
  parentName = `parent_${uid}`;
  gpName = `gp_${uid}`;

  const mock = await startMockService(0, { response: {} });
  mockPort = mock.port;
  stopMock = mock.stop;

  await ctx.env.define(workerName, [
    {
      id: "work",
      call: {
        type: "rest" as const,
        endpoint: `http://localhost:${mockPort}/action`,
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
        children: { a: { name: workerName }, b: { name: workerName } },
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

afterAll(() => stopMock?.());

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

test("happy path — tree completes when ticked to completion", async () => {
  const { gp, parent, a, b } = await buildTree();
  try {
    // tick: a (spawned first) completes; b still running, parent stays waiting
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "running waiting",
      parent: "running waiting",
      a: "completed",
      b: "running",
    });

    // tick: b completes; count = 0 → parent.wait_state = 'collecting'
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "running waiting",
      parent: "running collecting",
      a: "completed",
      b: "completed",
    });

    // tick: parent (running+collecting) collects outputs, advances to end → completed
    //       FinishChild(parent): gp.wait_state = 'collecting'
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "running collecting",
      parent: "completed",
      a: "completed",
      b: "completed",
    });

    // tick: gp (running+collecting) collects output, advances to end → completed
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "completed",
      parent: "completed",
      a: "completed",
      b: "completed",
    });
  } finally {
    await ctx.env.tickUntilIdle();
  }
});

test("cancel grandparent — entire tree becomes cancelling instantly, cancelled after ticks", async () => {
  const { gp, parent, a, b } = await buildTree();
  try {
    await ctx.env.cancel(gp);

    // CancelProcess is atomic. gp and parent keep wait_state='waiting' — they stay
    // suspended until their children settle via FinishChild.
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "cancelling waiting",
      parent: "cancelling waiting",
      a: "cancelling",
      b: "cancelling",
    });

    // tick: a cancelled; b still active, parent not yet woken
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "cancelling waiting",
      parent: "cancelling waiting",
      a: "cancelled",
      b: "cancelling",
    });

    // tick: b cancelled; count = 0 → parent.wait_state = 'collecting'
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "cancelling waiting",
      parent: "cancelling collecting",
      a: "cancelled",
      b: "cancelled",
    });

    // tick: parent (cancelling+collecting) → cancelInstance → cancelled
    //       FinishChild(parent): gp.wait_state = 'collecting'
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "cancelling collecting",
      parent: "cancelled",
      a: "cancelled",
      b: "cancelled",
    });

    // tick: gp (cancelling+collecting) → cancelInstance → cancelled
    await ctx.env.tick();
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "cancelled",
      parent: "cancelled",
      a: "cancelled",
      b: "cancelled",
    });
  } finally {
    await ctx.env.tickUntilIdle();
  }
});

test("cancel non-root — rejected naming the root; tree unaffected", async () => {
  const { gp, parent, a, b } = await buildTree();
  try {
    // Cancellation is a whole-tree decision: only the root is accepted.
    for (const id of [parent, a]) {
      const { error } = await ctx.env.client.POST("/instances/{id}/cancel", {
        params: { path: { id } },
      });
      expect(error).toBeDefined();
      expect(JSON.stringify(error)).toContain(gp);
    }

    // The rejected cancels left the tree untouched.
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "running waiting",
      parent: "running waiting",
      a: "running",
      b: "running",
    });
  } finally {
    await ctx.env.tickUntilIdle();
  }
});
