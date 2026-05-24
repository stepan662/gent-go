import { spawnSync, spawn, type ChildProcess } from "child_process";
import { join } from "path";
import { tmpdir } from "os";
import { BASE_URL, PORT } from "./constants.ts";

async function ping(): Promise<boolean> {
  try {
    const r = await fetch(`${BASE_URL}/openapi.json`);
    await r.body?.cancel();
    return r.ok;
  } catch {
    return false;
  }
}

let proc: ChildProcess | null = null;

export async function setup() {
  if (await ping()) return;

  const root = new URL("../../", import.meta.url).pathname;
  const bin = join(tmpdir(), `gent_${Date.now()}`);
  const db = join(tmpdir(), `gent_${Date.now()}.db`);

  console.log("\nBuilding test server…");
  const build = spawnSync("go", ["build", "-o", bin, "./cmd/gent"], {
    cwd: root,
    env: { ...process.env, CGO_ENABLED: "1" },
    stdio: ["ignore", "ignore", "inherit"],
  });

  if (build.status !== 0) throw new Error("Failed to build test server");

  proc = spawn(bin, ["--db", db, "--http", `:${PORT}`, "--log", "error"], {
    stdio: "ignore",
  });

  let ready = false;
  for (let i = 0; i < 50; i++) {
    if (await ping()) {
      ready = true;
      break;
    }
    await new Promise((r) => setTimeout(r, 200));
  }
  if (!ready) throw new Error("Test server did not start within 10 s");
}

export function teardown() {
  proc?.kill("SIGTERM");
}
