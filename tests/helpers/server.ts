import { afterAll, beforeAll } from "bun:test";
import { join } from "path";
import { tmpdir } from "os";
import { BASE_URL } from "./constants.ts";

async function ping(): Promise<boolean> {
  try {
    const r = await fetch(`${BASE_URL}/openapi.json`);
    await r.body?.cancel();
    return r.ok;
  } catch {
    return false;
  }
}

let proc: ReturnType<typeof Bun.spawn> | null = null;

beforeAll(async () => {
  if (await ping()) return;

  const root = new URL("../../", import.meta.url).pathname;
  const bin = join(tmpdir(), `gent_${Date.now()}`);
  const db = join(tmpdir(), `gent_${Date.now()}.db`);

  console.error("Building test server…");
  const build = Bun.spawnSync(["go", "build", "-o", bin, "./cmd/gent"], {
    cwd: root,
    env: { ...process.env, CGO_ENABLED: "1" },
    stderr: "inherit",
  });

  if (build.exitCode !== 0) throw new Error("Failed to build test server");

  proc = Bun.spawn([bin, "--db", db, "--http", ":8080", "--log", "error"], {
    stdout: "ignore",
    stderr: "ignore",
  });

  let ready = false;
  for (let i = 0; i < 50; i++) {
    if (await ping()) {
      ready = true;
      break;
    }
    await Bun.sleep(200);
  }
  if (!ready) throw new Error("Test server did not start within 10 s");
});

afterAll(() => {
  proc?.kill();
});
