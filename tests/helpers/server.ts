import { spawnSync, spawn, type ChildProcess } from "child_process";
import { join } from "path";
import { tmpdir } from "os";
import { BASE_URL, PORT } from "./constants.ts";
import { createClientTyped } from "./client.ts";

const ROOT = new URL("../../", import.meta.url).pathname;

export async function buildGentBinary(): Promise<string> {
  const bin = join(tmpdir(), `gent_${Date.now()}`);
  const result = spawnSync("go", ["build", "-o", bin, "./cmd/gent"], {
    cwd: ROOT,
    env: { ...process.env, CGO_ENABLED: "1" },
    stdio: ["ignore", "ignore", "inherit"],
  });
  if (result.status !== 0) throw new Error("Failed to build gent binary");
  return bin;
}

function spawnProc(
  bin: string,
  port: number,
  db: string,
  pgDSN?: string,
  pollMs?: number,
  maxConcurrent?: number,
  immediateRetries?: boolean,
): ChildProcess {
  const dbArgs = pgDSN ? ["--pg", pgDSN] : ["--db", db];
  const pollArgs = pollMs !== undefined ? ["--poll", String(pollMs)] : [];
  const concArgs = maxConcurrent !== undefined ? ["--max-concurrent", String(maxConcurrent)] : [];
  const retryArgs = immediateRetries ? ["--immediate-retries"] : [];
  return spawn(bin, [...dbArgs, "--http", `:${port}`, "--log", "error", ...pollArgs, ...concArgs, ...retryArgs], {
    stdio: "ignore",
  });
}

async function waitUntilReady(port: number, timeoutMs = 10_000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const r = await fetch(`http://localhost:${port}/openapi.json`);
      await r.body?.cancel();
      if (r.ok) return;
    } catch {}
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(`gent on port ${port} did not become ready within ${timeoutMs}ms`);
}

export interface GentProcess {
  client: ReturnType<typeof createClientTyped>;
  stop: () => void;  // SIGTERM — clean shutdown
  crash: () => void; // SIGKILL — simulate a hard crash, lease stays in DB
}

export async function startGent(
  bin: string,
  port: number,
  db: string,
  pgDSN?: string,
  pollMs?: number,
  maxConcurrent?: number,
  immediateRetries?: boolean,
): Promise<GentProcess> {
  const proc = spawnProc(bin, port, db, pgDSN, pollMs, maxConcurrent, immediateRetries);
  await waitUntilReady(port);
  return {
    client: createClientTyped({ baseUrl: `http://localhost:${port}` }),
    stop: () => proc.kill("SIGTERM"),
    crash: () => proc.kill("SIGKILL"),
  };
}

// ── Global shared server for vitest's globalSetup ─────────────────────────────

let sharedServer: GentProcess | null = null;

async function ping(): Promise<boolean> {
  try {
    const r = await fetch(`${BASE_URL}/openapi.json`);
    await r.body?.cancel();
    return r.ok;
  } catch {
    return false;
  }
}

export async function setup() {
  if (await ping()) return;
  console.log("\nBuilding test server…");
  const bin = await buildGentBinary();
  const db = join(tmpdir(), `gent_${Date.now()}.db`);
  sharedServer = await startGent(bin, PORT as number, db);
}

export function teardown() {
  sharedServer?.stop();
}
