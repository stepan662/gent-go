import { expect, test } from "vitest";
import { useTickEnv } from "./helpers.ts";

// Exercises the `delay` action: it parks the instance by stamping next_retry_at
// (releasing the worker) and the normal claim loop resumes it once the server
// clock advances past `ms`. Driven in manual-tick mode with /tick advance_ms.
const ctx = useTickEnv(20019);

test("delay parks the instance until the clock advances past ms", async () => {
  await ctx.env.define("delay_done", [
    { id: "wait", action: { type: "delay", ms: "60000" }, switch: "end" },
  ]);
  const id = await ctx.env.start("delay_done");

  // First tick arms the delay; the instance parks (running, timer in the future).
  expect(await ctx.env.tick()).toBe(1);
  expect(await ctx.env.status(id)).toBe("running");

  // While parked it is not claimable — a plain tick processes nothing.
  expect(await ctx.env.tick()).toBe(0);
  expect(await ctx.env.status(id)).toBe("running");

  // Advancing the clock past ms makes it claimable; it resumes and completes.
  await ctx.env.client.POST("/tick", { body: { advance_ms: 60000 } });
  expect(await ctx.env.status(id)).toBe("completed");
});

test("cancel drains a delayed instance immediately, without waiting out the delay", async () => {
  await ctx.env.client.PUT("/definitions", {
    body: {
      name: "delay_cancel",
      tasks: [{ id: "wait", action: { type: "delay", ms: "3600000" }, switch: "end" }],
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  const id = await ctx.env.start("delay_cancel");

  expect(await ctx.env.tick()).toBe(1); // arm a 1-hour delay
  expect(await ctx.env.status(id)).toBe("running");

  await ctx.env.cancel(id);
  // No clock advance: a cancelling instance is claimed despite its future timer.
  await ctx.env.tickUntilIdle();
  expect(await ctx.env.status(id)).toBe("cancelled");
});

test("delay does not resume before the full ms has elapsed", async () => {
  await ctx.env.define("delay_partial", [
    { id: "wait", action: { type: "delay", ms: "60000" }, switch: "end" },
  ]);
  const id = await ctx.env.start("delay_partial");

  expect(await ctx.env.tick()).toBe(1); // arm

  // Advancing only part of the way leaves it parked.
  await ctx.env.client.POST("/tick", { body: { advance_ms: 30000 } });
  expect(await ctx.env.status(id)).toBe("running");

  // Advancing the remainder resumes and completes it.
  await ctx.env.client.POST("/tick", { body: { advance_ms: 30000 } });
  expect(await ctx.env.status(id)).toBe("completed");
});

test("retry after cancel resumes a delay toward its original deadline", async () => {
  await ctx.env.client.PUT("/definitions", {
    body: {
      name: "delay_resume",
      tasks: [{ id: "wait", action: { type: "delay", ms: "60000" }, switch: "end" }],
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  const id = await ctx.env.start("delay_resume");

  expect(await ctx.env.tick()).toBe(1); // arm delay (deadline = T + 60s)
  await ctx.env.cancel(id);
  await ctx.env.tickUntilIdle(); // drains promptly; cancel preserves the delay timer
  expect(await ctx.env.status(id)).toBe("cancelled");

  await ctx.env.client.POST("/instances/{id}/retry", { params: { path: { id } } });
  // Revived toward the original (still-future) deadline, NOT re-armed: a plain tick
  // claims nothing because the preserved timer has not elapsed. (A from-scratch
  // re-arm would instead claim it once to re-stamp the timer — i.e. tick() === 1.)
  expect(await ctx.env.tick()).toBe(0);
  expect(await ctx.env.status(id)).toBe("running");

  // Reaching the original deadline completes it.
  await ctx.env.client.POST("/tick", { body: { advance_ms: 60000 } });
  expect(await ctx.env.status(id)).toBe("completed");
});

test("retry after cancel runs immediately if the delay's deadline already passed", async () => {
  await ctx.env.client.PUT("/definitions", {
    body: {
      name: "delay_passed",
      tasks: [{ id: "wait", action: { type: "delay", ms: "5000" }, switch: "end" }],
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  const id = await ctx.env.start("delay_passed");

  expect(await ctx.env.tick()).toBe(1); // arm delay (deadline = T + 5s)
  await ctx.env.cancel(id);
  await ctx.env.tickUntilIdle();
  expect(await ctx.env.status(id)).toBe("cancelled");

  // The deadline elapses while the instance is cancelled.
  await ctx.env.client.POST("/tick", { body: { advance_ms: 10000 } });

  await ctx.env.client.POST("/instances/{id}/retry", { params: { path: { id } } });
  // The preserved timer is already in the past, so it runs straight through with
  // no further clock advance. (A from-scratch re-arm would park it 5s into the
  // future and never settle here.)
  await ctx.env.tickUntilIdle();
  expect(await ctx.env.status(id)).toBe("completed");
});
