import type { GenrocProcess } from "./server.ts";
import { buildGenrocBinary, startGenroc } from "./server.ts";

// globalSetup runs in the main vitest process, not in a project worker,
// so project-level env vars (GENROC_PORT) are not available here.
const PG_PORT = 8889;

let server: GenrocProcess | null = null;

export async function setup() {
  const dsn = process.env.POSTGRES_DSN;
  if (!dsn)
    throw new Error("POSTGRES_DSN must be set for the postgres test project");

  const bin = await buildGenrocBinary();
  server = await startGenroc(bin, PG_PORT, "", dsn);
}

export function teardown() {
  server?.stop();
}
