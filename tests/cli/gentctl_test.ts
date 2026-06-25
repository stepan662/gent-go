import { mkdtempSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { beforeAll, expect, test } from "vitest";
import { buildGentctlBinary, runCli, writeDefs } from "../helpers/cli.ts";
import { client } from "../helpers/client.ts";

let bin: string;

beforeAll(() => {
  bin = buildGentctlBinary();
}, 60_000); // first build on a cold CI cache can exceed the 10s default

// ── helpers ───────────────────────────────────────────────────────────────────

function uid(prefix: string) {
  return `${prefix}_${crypto.randomUUID().replace(/-/g, "").slice(0, 8)}`;
}

function switchDef(name: string) {
  return {
    name,
    tasks: [{ id: "s1", switch: [{ goto: "end" }] }],
  };
}

function restDef(name: string, endpoint = "http://localhost/x") {
  return { name, tasks: [{ id: "s1", action: { type: "rest", endpoint }, switch: [{ goto: "end" }] }] };
}

function childDef(name: string, childName: string) {
  return {
    name,
    tasks: [{ id: "spawn", action: { type: "child", name: childName }, switch: [{ goto: "end" }] }],
  };
}

function inputDef(name: string) {
  return {
    name,
    input_schema: {
      type: "object",
      properties: { count: { type: "number" }, name: { type: "string" } },
      required: ["count"],
    },
    tasks: [{ id: "s1", switch: [{ goto: "end" }] }],
  };
}

// Pull the instance id out of a `started: <id>  proc@vN  (status)` line.
function startedID(stdout: string): string {
  const m = stdout.match(/started:\s+(\S+)/);
  if (!m) throw new Error(`no started id in: ${stdout}`);
  return m[1];
}

// ── apply ─────────────────────────────────────────────────────────────────────

test("apply — saves definition and prints saved line", () => {
  const name = uid("proc");
  const file = writeDefs([switchDef(name)]);

  const r = runCli(bin, ["apply", "-f", file]);

  expect(r.ok).toBe(true);
  expect(r.stdout).toContain(`saved: ${name}@v1`);
});

test("apply — unchanged content prints unchanged line", () => {
  const name = uid("proc");
  const file = writeDefs([switchDef(name)]);

  runCli(bin, ["apply", "-f", file]);
  const r = runCli(bin, ["apply", "-f", file]);

  expect(r.ok).toBe(true);
  expect(r.stdout).toContain(`unchanged: ${name}@v1`);
});

test("apply --channel sets the named channel", async () => {
  const name = uid("proc");
  const file = writeDefs([switchDef(name)]);

  const r = runCli(bin, ["apply", "-f", file, "--channel", "stable"]);

  expect(r.ok).toBe(true);
  expect(r.stdout).toContain(`saved: ${name}@v1`);

  const { data } = await client.GET("/channels", { params: { query: { name } } });
  const entry = (data?.items ?? []).find((e) => e.channel === "stable");
  expect(entry?.version).toBe(1);
});

test("apply — multi-document YAML applies all definitions", () => {
  const a = uid("a");
  const b = uid("b");
  const file = writeDefs([switchDef(a), switchDef(b)]);

  const r = runCli(bin, ["apply", "-f", file]);

  expect(r.ok).toBe(true);
  expect(r.stdout).toContain(`saved: ${a}@v1`);
  expect(r.stdout).toContain(`saved: ${b}@v1`);
});

test("apply --auto-update-parents cascades to parent on same channel", () => {
  const childName = uid("child");
  const parentName = uid("parent");

  // Apply child + parent on "stable".
  runCli(bin, ["apply", "-f", writeDefs([switchDef(childName), childDef(parentName, childName)]), "--channel", "stable"]);

  // Change child content and apply with --auto-update-parents.
  const child2 = { ...switchDef(childName), tasks: [{ id: "s2", switch: [{ goto: "end" }] }] };
  const r = runCli(bin, ["apply", "-f", writeDefs([child2]), "--channel", "stable", "--auto-update-parents"]);

  expect(r.ok).toBe(true);
  expect(r.stdout).toContain(`saved: ${childName}@v2`);
  // Parent should appear in output too (auto-created new version).
  expect(r.stdout).toContain(parentName);
});

test("apply — accepts self-referential (recursive) process", () => {
  const name = uid("recursive");
  const file = writeDefs([childDef(name, name)]);

  const r = runCli(bin, ["apply", "-f", file]);

  expect(r.ok).toBe(true);
  expect(r.stdout).toContain(`saved: ${name}@v1`);
});

test("apply — exits non-zero and prints error for invalid definition", () => {
  const file = writeDefs([{ name: "bad", tasks: [] }]); // tasks must not be empty

  const r = runCli(bin, ["apply", "-f", file]);

  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("gentctl:");
});

// ── validate ──────────────────────────────────────────────────────────────────

test("validate — exits 0 and prints schema for valid definition", () => {
  const name = uid("proc");
  const file = writeDefs([switchDef(name)]);

  const r = runCli(bin, ["validate", "-f", file]);

  expect(r.ok).toBe(true);
  expect(r.stdout).toContain(name);
});

test("validate — exits non-zero for invalid definition", () => {
  const file = writeDefs([{ name: "bad", tasks: [] }]);

  const r = runCli(bin, ["validate", "-f", file]);

  expect(r.ok).toBe(false);
});

// ── channel ───────────────────────────────────────────────────────────────────

test("channel set / list / delete", () => {
  const name = uid("proc");

  // Create definition first.
  runCli(bin, ["apply", "-f", writeDefs([restDef(name)])]);

  // set
  const setR = runCli(bin, ["channel", "set", name, "stable", "1"]);
  expect(setR.ok).toBe(true);
  expect(setR.stdout).toContain(`${name}@stable`);

  // list
  const listR = runCli(bin, ["channel", "list", name]);
  expect(listR.ok).toBe(true);
  expect(listR.stdout).toContain("stable");
  expect(listR.stdout).toContain("v1");

  // delete
  const delR = runCli(bin, ["channel", "delete", name, "stable"]);
  expect(delR.ok).toBe(true);

  const listAfter = runCli(bin, ["channel", "list", name]);
  expect(listAfter.stdout).not.toContain("stable");
});

test("channel set — fails for non-existent process", () => {
  const r = runCli(bin, ["channel", "set", "no-such-process", "stable", "1"]);
  expect(r.ok).toBe(false);
});

test("channel set — fails for invalid version", () => {
  const r = runCli(bin, ["channel", "set", "p", "stable", "notanumber"]);
  expect(r.ok).toBe(false);
});

// ── promote ───────────────────────────────────────────────────────────────────

test("promote — copies all channel pointers from source to target", () => {
  const a = uid("a");
  const b = uid("b");

  runCli(bin, ["apply", "-f", writeDefs([switchDef(a), switchDef(b)]), "--channel", "staging"]);

  const r = runCli(bin, ["promote", "--from", "staging", "--to", "prod"]);
  expect(r.ok).toBe(true);
  expect(r.stdout).toContain(a);
  expect(r.stdout).toContain(b);

  const listA = runCli(bin, ["channel", "list", a]);
  expect(listA.stdout).toContain("prod");

  const listB = runCli(bin, ["channel", "list", b]);
  expect(listB.stdout).toContain("prod");
});

test("promote --process — only touches the named process subtree", () => {
  const childName = uid("child");
  const parentName = uid("parent");
  const unrelated = uid("unrelated");
  const track = uid("track");

  runCli(bin, [
    "apply", "-f",
    writeDefs([switchDef(childName), childDef(parentName, childName), switchDef(unrelated)]),
    "--channel", track,
  ]);

  const r = runCli(bin, ["promote", "--from", track, "--to", `${track}_out`, "--process", parentName]);
  expect(r.ok).toBe(true);

  const parentList = runCli(bin, ["channel", "list", parentName]);
  expect(parentList.stdout).toContain(`${track}_out`);

  const unrelatedList = runCli(bin, ["channel", "list", unrelated]);
  expect(unrelatedList.stdout).not.toContain(`${track}_out`);
});

test("promote — fails when --from or --to is missing", () => {
  const r = runCli(bin, ["promote", "--from", "staging"]);
  expect(r.ok).toBe(false);
});

// ── run / get / instances ──────────────────────────────────────────────────────

test("run --set — starts an instance and prints its id", () => {
  const name = uid("proc");
  runCli(bin, ["apply", "-f", writeDefs([inputDef(name)])]);

  const r = runCli(bin, ["run", name, "--set", "count=3", "--set", "name=Sam"]);

  expect(r.ok).toBe(true);
  expect(r.stdout).toContain(`started:`);
  expect(r.stdout).toContain(`${name}@v1`);
  expect(startedID(r.stdout)).toMatch(/[0-9a-f-]{36}/);
});

test("run --input — accepts relaxed JSON", () => {
  const name = uid("proc");
  runCli(bin, ["apply", "-f", writeDefs([inputDef(name)])]);

  const r = runCli(bin, ["run", name, "--input", "{count: 7, name: Sam}"]);

  expect(r.ok).toBe(true);
  expect(r.stdout).toContain(`${name}@v1`);
});

test("run — prints a friendly error when input does not match the schema", () => {
  const name = uid("proc");
  runCli(bin, ["apply", "-f", writeDefs([inputDef(name)])]);

  const r = runCli(bin, ["run", name, "--set", "count=not-a-number"]);

  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("input is not valid for");
});

test("get — shows a started instance's details and context", () => {
  const name = uid("proc");
  runCli(bin, ["apply", "-f", writeDefs([inputDef(name)])]);
  const id = startedID(runCli(bin, ["run", name, "--set", "count=42"]).stdout);

  const r = runCli(bin, ["get", id]);

  expect(r.ok).toBe(true);
  expect(r.stdout).toContain(id);
  expect(r.stdout).toContain(`${name}@v1`);
  expect(r.stdout).toContain("Context:");
  expect(r.stdout).toContain("42"); // the input value lives in the context

  // --json dumps the raw response, which carries the context object.
  const j = runCli(bin, ["get", id, "--json"]);
  expect(j.ok).toBe(true);
  expect(j.stdout).toContain(`"context"`);
});

test("instances — lists a freshly started instance", () => {
  const name = uid("proc");
  runCli(bin, ["apply", "-f", writeDefs([inputDef(name)])]);
  const id = startedID(runCli(bin, ["run", name, "--set", "count=1"]).stdout);

  const r = runCli(bin, ["instances"]);

  expect(r.ok).toBe(true);
  expect(r.stdout).toContain("STATUS");
  expect(r.stdout).toContain(id);

  // --sort updated is accepted too.
  const u = runCli(bin, ["instances", "--sort", "updated"]);
  expect(u.ok).toBe(true);
});

// ── run -q / @last / last ───────────────────────────────────────────────────────

test("run -q — prints only the bare instance id", () => {
  const name = uid("proc");
  runCli(bin, ["apply", "-f", writeDefs([inputDef(name)])]);

  const r = runCli(bin, ["run", name, "--set", "count=1", "-q"]);

  expect(r.ok).toBe(true);
  // stdout is exactly the id — nothing else — so id=$(gentctl run … -q) is clean.
  expect(r.stdout.trim()).toMatch(/^[0-9a-f-]{36}$/);
  expect(r.stdout).not.toContain("started:");
});

test("get / logs / last — @last resolves the most recently started instance", () => {
  const name = uid("proc");
  runCli(bin, ["apply", "-f", writeDefs([inputDef(name)])]);
  const id = runCli(bin, ["run", name, "--set", "count=7", "-q"]).stdout.trim();

  // `last` echoes the recorded id.
  expect(runCli(bin, ["last"]).stdout.trim()).toBe(id);

  // @last is the explicit handle, including alongside a flag.
  const gj = runCli(bin, ["get", "@last", "--json"]);
  expect(gj.ok).toBe(true);
  expect(gj.stdout).toContain(`"${id}"`);

  // logs accept @last too.
  const l = runCli(bin, ["logs", "@last"]);
  expect(l.ok).toBe(true);
});

test("get — requires an explicit id (a bare command never implies @last)", () => {
  const name = uid("proc");
  runCli(bin, ["apply", "-f", writeDefs([inputDef(name)])]);
  // Even immediately after a run, a bare `get` must not silently reuse the last id.
  runCli(bin, ["run", name, "--set", "count=1", "-q"]);

  const r = runCli(bin, ["get"]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("instance id is required");
});

test("@last — errors when no instance has been started yet", () => {
  // A pristine config home so no prior `run` leaks a last id into this case.
  const home = mkdtempSync(join(tmpdir(), "gent_nolast_"));
  const r = runCli(bin, ["get", "@last"], {
    HOME: home,
    XDG_CONFIG_HOME: join(home, ".config"),
  });

  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("no instance recorded");
});

// ── status ────────────────────────────────────────────────────────────────────

test("status -- reports coherent when channel is up to date", () => {
  const childName = uid("child");
  const parentName = uid("parent");
  const track = uid("track");

  runCli(bin, ["apply", "-f", writeDefs([switchDef(childName), childDef(parentName, childName)]), "--channel", track]);

  const r = runCli(bin, ["status", "--channel", track]);
  expect(r.ok).toBe(true);
  expect(r.stdout).toContain("coherent");
});

test("status -- reports stale ref after child is advanced without updating parent", () => {
  const childName = uid("child");
  const parentName = uid("parent");
  const track = uid("track");

  runCli(bin, ["apply", "-f", writeDefs([switchDef(childName), childDef(parentName, childName)]), "--channel", track]);

  // Advance child only.
  const child2 = { ...switchDef(childName), tasks: [{ id: "s2", switch: [{ goto: "end" }] }] };
  runCli(bin, ["apply", "-f", writeDefs([child2]), "--channel", track]);

  const r = runCli(bin, ["status", "--channel", track]);
  expect(r.ok).toBe(true);
  expect(r.stdout).toContain("STALE");
  expect(r.stdout).toContain(parentName);
  expect(r.stdout).toContain(childName);
});
