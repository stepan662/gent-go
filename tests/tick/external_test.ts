import { expect, test } from "vitest";
import { useTickEnv } from "./helpers.ts";

// Exercises the `external` action: the engine parks the instance (wait_state='external',
// no worker held), an outside caller discovers it via GET /external-tasks and submits a
// result to POST /external-tasks/resolve, and the process resumes. An optional timeout_ms
// raises a catchable external.timeout. Driven in manual-tick mode.
const ctx = useTickEnv(20031);

// eslint-disable-next-line @typescript-eslint/no-explicit-any
const approvedSchema: any = {
  type: "object",
  properties: { approved: { type: "boolean" } },
  required: ["approved"],
};

// Find the single queue entry for an instance id (the token is `<id>.<nonce>`).
// eslint-disable-next-line @typescript-eslint/no-explicit-any
async function queueEntryFor(id: string): Promise<any | undefined> {
  const { data, error } = await ctx.env.client.GET("/external-tasks", {});
  if (error) throw new Error(`list external tasks failed: ${JSON.stringify(error)}`);
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  return ((data?.items ?? []) as any[]).find(
    (t) => typeof t.token === "string" && t.token.startsWith(`${id}.`),
  );
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
async function resolve(token: string, result: unknown) {
  return ctx.env.client.POST("/external-tasks/resolve", {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    body: { token, result } as any,
  });
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
async function contextOf(id: string): Promise<any> {
  const { data } = await ctx.env.client.GET("/instances/{id}", { params: { path: { id } } });
  return data!.context;
}

test("external parks, is queued, and resumes when resolved", async () => {
  await ctx.env.define("ext_happy", [
    {
      id: "approval",
      action: { type: "external", input: { msg: "approve me" }, result_schema: approvedSchema },
      output: "{{ self.result }}",
      switch: "end",
    },
  ]);
  const id = await ctx.env.start("ext_happy");

  // First tick arms the wait; the instance parks (running, wait_state='external').
  expect(await ctx.env.tick()).toBe(1);
  expect(await ctx.env.status(id)).toBe("running external");

  // While parked it is not claimable — a plain tick processes nothing.
  expect(await ctx.env.tick()).toBe(0);

  // It appears on the queue with its input + result_schema + a token, no context.
  const entry = await queueEntryFor(id);
  expect(entry).toBeDefined();
  expect(entry.task_id).toBe("approval");
  expect(entry.process).toBe("ext_happy");
  expect(entry.input).toEqual({ msg: "approve me" });
  expect(entry.result_schema).toBeTruthy();
  expect(entry).not.toHaveProperty("context");

  // Submitting a valid result un-parks it; the next tick runs it to completion.
  const { error } = await resolve(entry.token, { approved: true });
  expect(error).toBeUndefined();

  expect(await ctx.env.tick()).toBe(1);
  expect(await ctx.env.status(id)).toBe("completed");
  // The submitted result flowed through self.result into the task output.
  expect((await contextOf(id)).outputs.approval).toEqual({ approved: true });
});

test("resolve validates the result against result_schema", async () => {
  await ctx.env.define("ext_validate", [
    { id: "approval", action: { type: "external", result_schema: approvedSchema }, switch: "end" },
  ]);
  const id = await ctx.env.start("ext_validate");
  expect(await ctx.env.tick()).toBe(1);

  const entry = await queueEntryFor(id);
  // approved must be a boolean — a string is rejected and the task stays parked.
  const { error } = await resolve(entry.token, { approved: "yes" });
  expect(error).toBeDefined();
  expect(await ctx.env.status(id)).toBe("running external");

  // A valid result still works afterwards.
  const ok = await resolve(entry.token, { approved: false });
  expect(ok.error).toBeUndefined();
  expect(await ctx.env.tick()).toBe(1);
  expect(await ctx.env.status(id)).toBe("completed");
});

test("a stale/double resolve is rejected", async () => {
  await ctx.env.define("ext_double", [
    { id: "approval", action: { type: "external", result_schema: approvedSchema }, switch: "end" },
  ]);
  const id = await ctx.env.start("ext_double");
  expect(await ctx.env.tick()).toBe(1);

  const entry = await queueEntryFor(id);
  expect((await resolve(entry.token, { approved: true })).error).toBeUndefined();
  // Second submit with the same token: the task is no longer waiting.
  expect((await resolve(entry.token, { approved: false })).error).toBeDefined();
  await ctx.env.tickUntilIdle(); // drain the resolved instance so it does not bleed into later tests
});

test("timeout raises external.timeout, catchable in on_error", async () => {
  await ctx.env.define("ext_timeout", [
    {
      id: "approval",
      action: { type: "external", result_schema: approvedSchema },
      timeout_ms: 60000,
      on_error: [{ code: ["external.timeout"], goto: "$handler" }],
      switch: "end",
    },
    { id: "handler", switch: "end" },
  ]);
  const id = await ctx.env.start("ext_timeout");

  expect(await ctx.env.tick()).toBe(1); // arm (deadline = T + 60s)
  expect(await ctx.env.status(id)).toBe("running external");

  // Not due yet: a plain tick claims nothing.
  expect(await ctx.env.tick()).toBe(0);
  expect(await ctx.env.status(id)).toBe("running external");

  // Advancing past the deadline fires the timeout, which routes to the handler.
  await ctx.env.client.POST("/tick", { body: { advance_ms: 60000 } });
  await ctx.env.tickUntilIdle();
  expect(await ctx.env.status(id)).toBe("completed");
});

test("a no-timeout external wait is never self-claimed", async () => {
  await ctx.env.define("ext_wait", [
    { id: "approval", action: { type: "external", result_schema: approvedSchema }, switch: "end" },
  ]);
  const id = await ctx.env.start("ext_wait");
  expect(await ctx.env.tick()).toBe(1); // arm, no timer

  // Advancing the clock far forward does not make it claimable (no timeout).
  await ctx.env.client.POST("/tick", { body: { advance_ms: 3600000 } });
  expect(await ctx.env.status(id)).toBe("running external");

  // Only a submitted result resumes it.
  const entry = await queueEntryFor(id);
  expect((await resolve(entry.token, { approved: true })).error).toBeUndefined();
  expect(await ctx.env.tick()).toBe(1);
  expect(await ctx.env.status(id)).toBe("completed");
});

test("cancel drains an externally-waiting instance immediately", async () => {
  await ctx.env.define("ext_cancel", [
    { id: "approval", action: { type: "external", result_schema: approvedSchema }, switch: "end" },
  ]);
  const id = await ctx.env.start("ext_cancel");
  expect(await ctx.env.tick()).toBe(1);
  expect(await ctx.env.status(id)).toBe("running external");

  await ctx.env.cancel(id);
  // No clock advance: a cancelling instance is claimed despite waiting on an external result.
  await ctx.env.tickUntilIdle();
  expect(await ctx.env.status(id)).toBe("cancelled");

  // Resolving a cancelled task is rejected.
  const { data, error } = await ctx.env.client.POST("/external-tasks/resolve", {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    body: { token: `${id}.whatever`, result: { approved: true } } as any,
  });
  expect(error ?? (data as { error?: string })?.error).toBeTruthy();
});
