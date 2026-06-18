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
    tasks: [{ id: "s1", switch: [{ goto: "end" }] }],
  };
}

function restDef(name: string, endpoint = "http://localhost/x") {
  return {
    name,
    tasks: [{ id: "s1", action: { type: "rest" as const, endpoint }, switch: [{ goto: "end" }] }],
  };
}

function childDef(name: string, childName: string, childVersion = 0) {
  const action: Record<string, unknown> = { type: "child" as const, name: childName };
  if (childVersion !== 0) action.version = childVersion;
  return {
    name,
    tasks: [
      {
        id: "spawn",
        action,
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
        tasks: [{ id: "s2", switch: [{ goto: "end" }] }],
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
        tasks: [{ id: "s2", switch: [{ goto: "end" }] }],
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
        tasks: [{ id: "s2", switch: [{ goto: "end" }] }],
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
      task_id: string;
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

// ── content dedup / versioning ──────────────────────────────────────────────────

type ChannelEntry = { channel: string; version: number };

async function channelVersion(name: string, channel: string) {
  const { data } = await client.GET("/channels", { params: { query: { name } } });
  return (data as ChannelEntry[]).find((e) => e.channel === channel)?.version;
}

// A child whose tasks differ from switchDef so it produces a new content hash.
function switchDefV2(name: string) {
  return { ...switchDef(name), tasks: [{ id: "s2", switch: [{ goto: "end" }] }] };
}

test("channels — identical content on a new channel reuses the existing version", async () => {
  const name = uid("proc");

  await applyBatch([switchDef(name)], "latest"); // v1
  const res = await applyBatch([switchDef(name)], "staging");
  expect(res).toMatchObject([{ name, version: 1, saved: false }]);
  expect(await channelVersion(name, "staging")).toBe(1);
});

test("channels — changed content creates a new version and advances the channel", async () => {
  const name = uid("proc");

  await applyBatch([restDef(name, "http://localhost/v1")], "latest");
  const res = await applyBatch([restDef(name, "http://localhost/changed")], "latest");
  expect(res).toMatchObject([{ name, version: 2, saved: true }]);
  expect(await channelVersion(name, "latest")).toBe(2);
});

test("channels — re-applying a recursive (self-calling) process dedups", async () => {
  const name = uid("recursive");
  const selfRef = {
    name,
    tasks: [
      { id: "recurse", action: { type: "child" as const, name }, switch: [{ goto: "end" }] },
    ],
  };

  await applyBatch([selfRef], "latest");
  const res = await applyBatch([selfRef], "latest");
  expect(res).toMatchObject([{ name, version: 1, saved: false }]);
  expect(await channelVersion(name, "latest")).toBe(1);
});

test("channels — dedup reuses an older version, not just the latest", async () => {
  const child = uid("child");
  const parent = uid("parent");

  // child@v1 + parent@v1 (deps: child@v1).
  await applyBatch([switchDef(child), childDef(parent, child)], "latest");
  // Advance child → v2, cascade parent → v2.
  await applyBatch([switchDefV2(child)], "latest", true);

  // Original content to a fresh channel: child matches v1, parent resolves child@v1 → matches v1.
  const res = await applyBatch([switchDef(child), childDef(parent, child)], "staging");
  for (const r of res) expect(r.saved).toBe(false);
  expect(await channelVersion(child, "staging")).toBe(1);
  expect(await channelVersion(parent, "staging")).toBe(1);
});

test("channels — same parent YAML resolves to different versions per channel", async () => {
  const child = uid("child");
  const parent = uid("parent");

  // ch-a: child@v1, parent@v1.
  await applyBatch([switchDef(child), childDef(parent, child)], "ch-a");
  // ch-b: child@v2 only.
  await applyBatch([switchDefV2(child)], "ch-b");

  // Same parent YAML to ch-b: child resolves v2 → different deps → new parent@v2.
  const resB = await applyBatch([childDef(parent, child)], "ch-b");
  const pB = resB.find((r) => r.name === parent);
  expect(pB?.saved).toBe(true);
  expect(pB?.version).toBe(2);

  // ch-c: child@v1 content, then same parent YAML → child resolves v1 → dedup parent@v1.
  await applyBatch([switchDef(child)], "ch-c");
  const resC = await applyBatch([childDef(parent, child)], "ch-c");
  const pC = resC.find((r) => r.name === parent);
  expect(pC?.saved).toBe(false);
  expect(pC?.version).toBe(1);
  expect(await channelVersion(parent, "ch-c")).toBe(1);
});

test("channels — parent with omitted child version resolves to the child's channel version", async () => {
  const child = uid("child");
  const parent = uid("parent");

  await applyBatch([switchDef(child)], "latest");
  await applyBatch([childDef(parent, child)], "latest");
  expect(await channelVersion(parent, "latest")).toBe(1);
});

// ── batch validation errors ─────────────────────────────────────────────────────

async function rawBatch(defs: object[], channel = "latest", autoUpdateParents = false) {
  return client.PUT("/definitions/batch", {
    body: { channel, auto_update_parents: autoUpdateParents, definitions: defs } as never,
  });
}

test("channels — applying a parent whose child is on no channel is rejected", async () => {
  const parent = uid("parent");
  const missing = uid("missing");

  const { data, error } = await rawBatch([childDef(parent, missing)]);
  expect(error).toBeDefined();
  expect(JSON.stringify(error)).toContain("not on channel");
  expect(data).toBeUndefined();
});

test("channels — a batch listing parent before child applies (topological reorder)", async () => {
  const child = uid("child");
  const parent = uid("parent");

  const res = await applyBatch([childDef(parent, child), switchDef(child)], "latest");
  expect(res.find((r) => r.name === parent)).toBeDefined();
  expect(res.find((r) => r.name === child)).toBeDefined();
});

test("channels — a dependency cycle in a batch is rejected", async () => {
  const a = uid("a");
  const b = uid("b");

  const { data, error } = await rawBatch([childDef(a, b), childDef(b, a)]);
  expect(error).toBeDefined();
  expect(JSON.stringify(error)).toContain("cycle");
  expect(data).toBeUndefined();
});

// ── auto-update-parents cascade variants ────────────────────────────────────────

test("channels — auto-update cascades through multiple levels (leaf→parent→grandparent)", async () => {
  const leaf = uid("leaf");
  const parent = uid("parent");
  const grand = uid("grand");

  await applyBatch(
    [switchDef(leaf), childDef(parent, leaf), childDef(grand, parent)],
    "latest",
  );

  const res = await applyBatch([switchDefV2(leaf)], "latest", true);
  const g = res.find((r) => r.name === grand);
  expect(g).toBeDefined();
  expect(g!.version).toBeGreaterThanOrEqual(2);
});

test("channels — cascade fires for a stale parent even when the child dedups", async () => {
  const child = uid("child");
  const parent = uid("parent");

  // child@v1 + parent@v1.
  await applyBatch([switchDef(child), childDef(parent, child)], "latest");
  // Advance child → v2 without auto-update; parent is now stale.
  await applyBatch([switchDefV2(child)], "latest");

  // Re-apply child@v2 (dedups) WITH auto-update — cascade must still update the parent.
  const res = await applyBatch([switchDefV2(child)], "latest", true);
  const c = res.find((r) => r.name === child);
  expect(c?.saved).toBe(false);
  expect(c?.version).toBe(2);
  const p = res.find((r) => r.name === parent);
  expect(p).toBeDefined();
  expect(p!.version).toBeGreaterThanOrEqual(2);
  expect(await channelVersion(parent, "latest")).toBeGreaterThanOrEqual(2);
});

test("channels — cascade reuses an existing parent version instead of creating a redundant one", async () => {
  const child = uid("child");
  const parent = uid("parent");

  // child@v1 + parent@v1, then child→v2 + parent→v2 (cascade) on latest.
  await applyBatch([switchDef(child), childDef(parent, child)], "latest");
  await applyBatch([switchDefV2(child)], "latest", true);

  // Apply the already-existing content to a new channel — nothing should be saved.
  const res = await applyBatch([switchDefV2(child), childDef(parent, child)], "staging", true);
  for (const r of res) expect(r.saved).toBe(false);
  expect(await channelVersion(parent, "staging")).toBe(2);
});

// ── promote / start_instance edge cases ─────────────────────────────────────────

test("channels — promote preserves the source channel pointer", async () => {
  const name = uid("proc");

  await applyBatch([switchDef(name)], "staging");
  await client.POST("/channels/promote", {
    body: { from: "staging", to: "preserve-target" },
  });

  expect(await channelVersion(name, "staging")).toBe(1);
  expect(await channelVersion(name, "preserve-target")).toBe(1);
});

test("channels — start_instance with a non-existent channel is rejected", async () => {
  const name = uid("proc");

  await applyBatch([switchDef(name)], "stable");
  const { data, error } = await client.POST("/instances", {
    body: { process: name, channel: "nonexistent" } as never,
  });
  expect(error).toBeDefined();
  expect(data).toBeUndefined();
});
