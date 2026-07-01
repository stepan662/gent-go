import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli, writeDefs } from "../helpers/cli.ts";
import { waitForInstance } from "../helpers/client.ts";

// CLI rendering of externalized (object-store) payloads: by default `genctl logs` and
// `genctl get` show a {ref, size} reference in place of a value too big to inline, and
// --resolve fetches and materializes the real object. Exercised through the compiled
// binary so the data_ref → body rendering is covered, not just the HTTP layer.

let bin: string;

beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000); // first build on a cold CI cache can exceed the 10s default

function uid(prefix: string) {
  return `${prefix}_${crypto.randomUUID().replace(/-/g, "").slice(0, 8)}`;
}

// > the 8 KiB threshold, so the input is stored in the object store and shows as a
// reference (not inline) until resolved.
const BIG_BLOB = "B".repeat(20 * 1024);

function blobInputDef(name: string) {
  return {
    name,
    input_schema: {
      type: "object",
      properties: { blob: { type: "string" } },
      required: ["blob"],
    },
    tasks: [{ id: "s1", switch: [{ goto: "end" }] }],
  };
}

test("logs — an externalized payload shows its {ref, size} reference by default", async () => {
  const name = uid("biglogs");
  runCli(bin, ["apply", "-f", writeDefs([blobInputDef(name)])]);
  const id = runCli(bin, ["run", name, "--input", JSON.stringify({ blob: BIG_BLOB }), "-q"]).stdout.trim();
  expect(await waitForInstance(id)).toBe("completed");

  const r = runCli(bin, ["logs", id]);
  expect(r.ok).toBe(true);
  // The inst_created input is too big to inline: the body is the ref + size, not a value.
  expect(r.stdout).toMatch(/input=\{"ref":"[0-9a-f]{32}","size":\d+\}/);
  // The raw blob is never printed by default.
  expect(r.stdout).not.toContain("BBBBBBBBBB");
});

test("logs --resolve — fetches and inlines the full externalized payload", async () => {
  const name = uid("biglogs");
  runCli(bin, ["apply", "-f", writeDefs([blobInputDef(name)])]);
  const id = runCli(bin, ["run", name, "--input", JSON.stringify({ blob: BIG_BLOB }), "-q"]).stdout.trim();
  expect(await waitForInstance(id)).toBe("completed");

  const r = runCli(bin, ["logs", id, "--resolve"]);
  expect(r.ok).toBe(true);
  // The real value is inlined; no reference marker remains.
  expect(r.stdout).toContain("BBBBBBBBBB");
  expect(r.stdout).not.toMatch(/"ref":"[0-9a-f]{32}"/);
});

test("get --resolve — materializes externalized context values", async () => {
  const name = uid("bigctx");
  runCli(bin, ["apply", "-f", writeDefs([blobInputDef(name)])]);
  const id = runCli(bin, ["run", name, "--input", JSON.stringify({ blob: BIG_BLOB }), "-q"]).stdout.trim();
  expect(await waitForInstance(id)).toBe("completed");

  // Default: the context carries a reference, not the blob.
  const lazy = runCli(bin, ["get", id, "--json"]);
  expect(lazy.ok).toBe(true);
  expect(lazy.stdout).toContain(`"ref":`);
  expect(lazy.stdout).not.toContain("BBBBBBBBBB");

  // --resolve: the blob is materialized into the returned context.
  const full = runCli(bin, ["get", id, "--resolve", "--json"]);
  expect(full.ok).toBe(true);
  expect(full.stdout).toContain("BBBBBBBBBB");
});
