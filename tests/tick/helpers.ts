import { beforeAll, afterAll } from "vitest";
import { join } from "path";
import { tmpdir } from "os";
import {
  buildGentBinary,
  startGent,
  type GentProcess,
} from "../helpers/server.ts";

// Cached binary — built once per Vitest worker process.
let _bin: string | null = null;
async function getBin(): Promise<string> {
  if (!_bin) _bin = await buildGentBinary();
  return _bin;
}

export class TickEnv {
  constructor(private readonly gent: GentProcess) {}

  get client() {
    return this.gent.client;
  }

  // Advance one engine poll cycle. Returns the number of instances processed.
  async tick(): Promise<number> {
    const { data, error } = await this.gent.client.POST("/tick", {});
    if (error) throw new Error(`tick failed: ${JSON.stringify(error)}`);
    return (data as { count: number }).count;
  }

  // Tick until no instances are processed in a cycle (fully settled).
  async tickUntilIdle(maxTicks = 20): Promise<void> {
    for (let i = 0; i < maxTicks; i++) {
      if ((await this.tick()) === 0) return;
    }
    throw new Error(`still active after ${maxTicks} ticks`);
  }

  async status(id: string): Promise<string> {
    const { data, error } = await this.gent.client.GET("/instances/{id}", {
      params: { path: { id } },
    });
    if (error)
      throw new Error(`status(${id}) failed: ${JSON.stringify(error)}`);
    return `${data!.status} ${data!.wait_state ?? ""}`.trim() as string;
  }

  async waitState(id: string): Promise<string> {
    const { data, error } = await this.gent.client.GET("/instances/{id}", {
      params: { path: { id } },
    });
    if (error)
      throw new Error(`waitState(${id}) failed: ${JSON.stringify(error)}`);
    return (data!.wait_state as string) ?? "";
  }

  // Check statuses for a labelled map of instance IDs.
  // Usage: env.statuses({ gp: gpId, parent: parentId, a: aId, b: bId })
  async statuses(
    tree: Record<string, string>,
  ): Promise<Record<string, string>> {
    const entries = await Promise.all(
      Object.entries(tree).map(
        async ([label, id]) => [label, await this.status(id)] as const,
      ),
    );
    return Object.fromEntries(entries);
  }

  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  async define(name: string, steps: object[]): Promise<void> {
    const { error } = await this.gent.client.PUT("/definitions", {
      body: { name, steps } as any,
    });
    if (error)
      throw new Error(`define(${name}) failed: ${JSON.stringify(error)}`);
  }

  async start(process: string): Promise<string> {
    const { data, error } = await this.gent.client.POST("/instances", {
      body: { process },
    });
    if (error)
      throw new Error(`start(${process}) failed: ${JSON.stringify(error)}`);
    return data!.id;
  }

  async cancel(id: string): Promise<void> {
    const { error } = await this.gent.client.POST("/instances/{id}/cancel", {
      params: { path: { id } },
    });
    if (error)
      throw new Error(`cancel(${id}) failed: ${JSON.stringify(error)}`);
  }

  // Returns the child instance ID stored as placeholder in the parent's context
  // after SpawnChildrenAndWait. Valid between spawn and child completion.
  async childOf(parentId: string, stepId: string): Promise<string> {
    const { data } = await this.gent.client.GET("/instances/{id}", {
      params: { path: { id: parentId } },
    });
    const placeholder = (data!.context as Record<string, unknown> | null)
      ?.outputs as Record<string, unknown> | null;
    const val = placeholder?.[stepId];
    if (typeof val !== "string") {
      throw new Error(
        `childOf(${parentId}, ${stepId}): expected string placeholder, got ${JSON.stringify(val)}`,
      );
    }
    return val;
  }

  // Returns the parallel child IDs keyed by child key, stored as placeholder
  // in the parent's context after SpawnChildrenAndWait.
  async childrenOf(
    parentId: string,
    stepId: string,
  ): Promise<Record<string, string>> {
    const { data } = await this.gent.client.GET("/instances/{id}", {
      params: { path: { id: parentId } },
    });
    const outputs = (data!.context as Record<string, unknown> | null)
      ?.outputs as Record<string, unknown> | null;
    const val = outputs?.[stepId];
    if (typeof val !== "object" || val === null) {
      throw new Error(
        `childrenOf(${parentId}, ${stepId}): expected object placeholder, got ${JSON.stringify(val)}`,
      );
    }
    return val as Record<string, string>;
  }

  stop() {
    this.gent.stop();
  }
}

// Registers beforeAll/afterAll to start a fresh tick-mode server on the given port.
// The returned object is populated before tests run.
//
// Usage:
//   const ctx = useTickEnv(20014);
//   test("...", async () => { await ctx.env.tick(); });
export function useTickEnv(port: number) {
  const ctx = {} as { env: TickEnv };

  beforeAll(async () => {
    const bin = await getBin();
    const db = join(tmpdir(), `gent_tick_${Date.now()}.db`);
    // poll=0 → manual tick mode; max-concurrent=1 → one instance per tick (predictable ordering)
    // immediateRetries=true → no backoff, retries are claimable on the very next tick
    const gent = await startGent(bin, port, db, undefined, 0, 1, true);
    ctx.env = new TickEnv(gent);
  }, 60_000);

  afterAll(() => ctx.env?.stop());

  return ctx;
}
