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
 * buildTree() leaves the tree at:
 *   gp={status:"running", wait_state:"waiting"}, parent={status:"running", wait_state:"waiting"},
 *   a=running, b=running
 *
 * The two interesting moments to observe:
 *   1. Immediately after CancelProcess — the DB state before any tick runs.
 *   2. After tickUntilIdle() — the fully settled final state.
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
//   gp={status:"running", wait_state:"waiting"}, parent={status:"running", wait_state:"waiting"},
//   a=running, b=running
async function buildTree() {
  const gp = await ctx.env.start(gpName);

  // tick: gp spawns parent → gp transitions to running+wait_state=waiting
  await ctx.env.tick();
  const parent = await ctx.env.childOf(gp, "run_parent");

  // tick: parent spawns a and b → parent transitions to running+wait_state=waiting
  await ctx.env.tick();
  const { a, b } = await ctx.env.childrenOf(parent, "run_children");

  expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
    gp: "running",
    parent: "running",
    a: "running",
    b: "running",
  });
  expect(await ctx.env.waitState(gp)).toBe("waiting");
  expect(await ctx.env.waitState(parent)).toBe("waiting");

  return { gp, parent, a, b };
}

test("happy path — tree completes when ticked to completion", async () => {
  const { gp, parent, a, b } = await buildTree();
  try {
    await ctx.env.tickUntilIdle();
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

    // CancelProcess is atomic: the recursive CTE marks all descendants,
    // ancestors (none here) in one transaction.
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "cancelling",
      parent: "cancelling",
      a: "cancelling",
      b: "cancelling",
    });

    await ctx.env.tickUntilIdle();

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

test("cancel parent — subtree + grandparent become cancelling, all settled to cancelled", async () => {
  const { gp, parent, a, b } = await buildTree();
  try {
    await ctx.env.cancel(parent);

    // Descendants: parent, a, b.
    // Ancestors: gp (running+wait_state=waiting → cancelling+wait_state=waiting).
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "cancelling",
      parent: "cancelling",
      a: "cancelling",
      b: "cancelling",
    });

    await ctx.env.tickUntilIdle();

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

test("cancel one child — sibling is unaffected, ancestors cascade to cancelled", async () => {
  const { gp, parent, a, b } = await buildTree();
  try {
    await ctx.env.cancel(a);

    // Only a and its ancestor chain become cancelling.
    // Sibling b remains running — it was not in a's descendant or ancestor set.
    // gp and parent stay running with wait_state=waiting (the cancel only changes status,
    // not wait_state — they still have live children).
    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "cancelling",
      parent: "cancelling",
      a: "cancelling",
      b: "running",
    });

    // tick 1: a or b processed.
    // tick 2: the other leaf processed; last sibling done → parent.wait_state='collecting'
    await ctx.env.tick();
    await ctx.env.tick();

    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "cancelling",
      parent: "cancelling",  // still cancelling, now with wait_state='collecting'
      a: "cancelled",
      b: "completed",
    });

    // tick 3: parent claimed (cancelling+collecting) → cancelInstance → FinishChild
    //         → gp.wait_state='collecting'
    await ctx.env.tick();

    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "cancelling",
      parent: "cancelled",
      a: "cancelled",
      b: "completed",
    });

    // tick 4: gp claimed (cancelling+collecting) → cancelInstance → cancelled
    await ctx.env.tick();

    expect(await ctx.env.statuses({ gp, parent, a, b })).toEqual({
      gp: "cancelled",
      parent: "cancelled",
      a: "cancelled",
      b: "completed",
    });
  } finally {
    await ctx.env.tickUntilIdle();
  }
});
