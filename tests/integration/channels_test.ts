import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

// Unique names per test to avoid cross-test interference.
function uid(prefix: string) {
  return `${prefix}_${crypto.randomUUID().replace(/-/g, "").slice(0, 8)}`;
}

async function applyBatch(
  defs: object[],
  channel = "latest",
  autoUpdateParents = false,
) {
  const { data, error } = await client.PUT("/definitions/batch", {
    body: {
      channel,
      auto_update_parents: autoUpdateParents,
      definitions: defs,
    } as never,
  });
  if (error) throw new Error(`applyBatch failed: ${JSON.stringify(error)}`);
  return data as Array<{ name: string; version: number; saved: boolean }>;
}

function switchDef(name: string) {
  return {
    name,
    steps: [{ id: "s1", switch: [{ goto: "end" }] }],
  };
}

function restDef(name: string, endpoint = "http://localhost/x") {
  return {
    name,
    steps: [{ id: "s1", call: { type: "rest" as const, endpoint }, switch: [{ goto: "end" }] }],
  };
}

function childDef(name: string, childName: string, childVersion = 0) {
  const call: Record<string, unknown> = { type: "child" as const, name: childName };
  if (childVersion !== 0) call.version = childVersion;
  return {
    name,
    steps: [
      {
        id: "spawn",
        call,
        switch: [{ goto: "end" }],
      },
    ],
  };
}

// ── apply + channel resolution ────────────────────────────────────────────────

test("channels — apply sets channel pointer and deduplicates unchanged content", async () => {
  const name = uid("proc");

  const first = await applyBatch([switchDef(name)], "stable");
  expect(first).toMatchObject([{ name, version: 1, saved: true }]);

  // Same content again — should deduplicate.
  const second = await applyBatch([switchDef(name)], "stable");
  expect(second).toMatchObject([{ name, version: 1, saved: false }]);

  // Verify channel pointer via list_channels.
  const { data: channels } = await client.GET("/channels", {
    params: { query: { name } },
  });
  const stable = (channels as Array<{ channel: string; version: number }>).find(
    (e) => e.channel === "stable",
  );
  expect(stable?.version).toBe(1);
});

test("channels — start_instance resolves version from channel", async () => {
  const name = uid("proc");

  await applyBatch([restDef(name, "http://localhost/v1")], "stable");
  await applyBatch([restDef(name, "http://localhost/v2")], "latest");

  // stable → v1
  const { data: inst1 } = await client.POST("/instances", {
    body: { process: name, channel: "stable" } as never,
  });
  expect((inst1 as { version: number }).version).toBe(1);

  // latest → v2
  const { data: inst2 } = await client.POST("/instances", {
    body: { process: name, channel: "latest" } as never,
  });
  expect((inst2 as { version: number }).version).toBe(2);
});

test("channels — explicit version takes priority over channel", async () => {
  const name = uid("proc");

  await applyBatch([restDef(name, "http://localhost/v1")], "stable");
  await applyBatch([restDef(name, "http://localhost/v2")], "latest");

  const { data: inst } = await client.POST("/instances", {
    body: { process: name, version: 2, channel: "stable" } as never,
  });
  expect((inst as { version: number }).version).toBe(2);
});

// ── auto-update-parents ───────────────────────────────────────────────────────

test("channels — auto-update-parents cascades to dependent process on same channel", async () => {
  const childName = uid("child");
  const parentName = uid("parent");

  await applyBatch(
    [switchDef(childName), childDef(parentName, childName)],
    "stable",
  );

  // Update child on stable; parent should be auto-bumped.
  const results = await applyBatch(
    [
      {
        ...switchDef(childName),
        steps: [{ id: "s2", switch: [{ goto: "end" }] }],
      },
    ],
    "stable",
    true,
  );

  const parentResult = results.find((r) => r.name === parentName);
  expect(parentResult).toBeDefined();
  expect(parentResult!.version).toBeGreaterThanOrEqual(2);

  // New parent instance on stable should run the bumped version.
  const { data: inst } = await client.POST("/instances", {
    body: { process: parentName, channel: "stable" } as never,
  });
  expect((inst as { version: number }).version).toBeGreaterThanOrEqual(2);
});

test("channels — auto-update-parents does not touch other channels", async () => {
  const childName = uid("child");
  const parentName = uid("parent");

  // Parent on "stable", child on both.
  await applyBatch([switchDef(childName)], "latest");
  await applyBatch(
    [switchDef(childName), childDef(parentName, childName)],
    "stable",
  );

  // Update child on "latest" only.
  await applyBatch(
    [
      {
        ...switchDef(childName),
        steps: [{ id: "s2", switch: [{ goto: "end" }] }],
      },
    ],
    "latest",
    true,
  );

  // stable/parent should still be v1.
  const { data: channels } = await client.GET("/channels", {
    params: { query: { name: parentName } },
  });
  const stable = (channels as Array<{ channel: string; version: number }>).find(
    (e) => e.channel === "stable",
  );
  expect(stable?.version).toBe(1);
});

// ── channel_status ────────────────────────────────────────────────────────────

test("channels — channel_status reports stale refs after child is advanced", async () => {
  const childName = uid("child");
  const parentName = uid("parent");
  const track = uid("track");

  await applyBatch(
    [switchDef(childName), childDef(parentName, childName)],
    track,
  );

  // Advance child without updating parent.
  await applyBatch(
    [
      {
        ...switchDef(childName),
        steps: [{ id: "s2", switch: [{ goto: "end" }] }],
      },
    ],
    track,
  );

  const { data: statusData } = await client.POST("/channels/status", {
    body: { channel: track },
  });
  const items = statusData as Array<{
    name: string;
    version: number;
    stale_refs: Array<{
      step_id: string;
      child_name: string;
      baked_version: number;
      channel_version: number;
    }>;
  }>;

  const parentItem = items.find((i) => i.name === parentName);
  expect(parentItem).toBeDefined();
  expect(parentItem!.stale_refs).toHaveLength(1);
  expect(parentItem!.stale_refs[0].child_name).toBe(childName);
  expect(parentItem!.stale_refs[0].baked_version).toBe(1);
  expect(parentItem!.stale_refs[0].channel_version).toBe(2);
});

test("channels — channel_status is clean when everything is coherent", async () => {
  const childName = uid("child");
  const parentName = uid("parent");
  const track = uid("track");

  await applyBatch(
    [switchDef(childName), childDef(parentName, childName)],
    track,
  );

  const { data: statusData } = await client.POST("/channels/status", {
    body: { channel: track },
  });
  const items = statusData as Array<{ name: string; stale_refs: unknown[] }>;
  for (const item of items) {
    expect(item.stale_refs ?? []).toHaveLength(0);
  }
});

// ── promote_channel ───────────────────────────────────────────────────────────

test("channels — promote copies all pointers from source to target channel", async () => {
  const a = uid("a");
  const b = uid("b");

  await applyBatch([switchDef(a), switchDef(b)], "staging");

  const { data: promoteData, error } = await client.POST("/channels/promote", {
    body: { from: "staging", to: "promoted" },
  });
  expect(error).toBeUndefined();

  const promoted = (promoteData as { promoted: Array<{ name: string }> })
    .promoted;
  const names = promoted.map((p) => p.name);
  expect(names).toContain(a);
  expect(names).toContain(b);

  for (const name of [a, b]) {
    const { data: channels } = await client.GET("/channels", {
      params: { query: { name } },
    });
    const entry = (
      channels as Array<{ channel: string; version: number }>
    ).find((e) => e.channel === "promoted");
    expect(entry?.version).toBe(1);
  }
});

test("channels — promote subtree only touches the process and its dependencies", async () => {
  const childName = uid("child");
  const parentName = uid("parent");
  const unrelated = uid("unrelated");

  await applyBatch(
    [
      switchDef(childName),
      childDef(parentName, childName),
      switchDef(unrelated),
    ],
    "staging",
  );

  await client.POST("/channels/promote", {
    body: { from: "staging", to: "subtree-target", process: parentName },
  });

  const parentChannels = (
    await client.GET("/channels", { params: { query: { name: parentName } } })
  ).data as Array<{ channel: string }>;
  expect(parentChannels.map((e) => e.channel)).toContain("subtree-target");

  const unrelatedChannels = (
    await client.GET("/channels", { params: { query: { name: unrelated } } })
  ).data as Array<{ channel: string }>;
  expect(unrelatedChannels.map((e) => e.channel)).not.toContain(
    "subtree-target",
  );
});

// ── end-to-end: apply → complete via channel ──────────────────────────────────

test("channels — process started by channel runs and completes", async () => {
  const name = uid("proc");

  await applyBatch([switchDef(name)], "stable");

  const { data: inst } = await client.POST("/instances", {
    body: { process: name, channel: "stable" } as never,
  });
  expect(inst).toBeDefined();

  const status = await waitForInstance((inst as { id: string }).id, 10_000);
  expect(status).toBe("completed");
});
