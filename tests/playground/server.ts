// Task handler implementations for the order-pipeline playground.
// HTTP plumbing, routing, and AJV validation live in generated/server.ts.
//
// Usage: bun run playground:server

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

import { startServer, type Handlers } from "./generated/server.ts";
import { PORT } from "./process.ts";

const handlers: Handlers = {
  async loop(ctx) {
    console.log(ctx);
    console.log(
      `processing task ${ctx.tasks[ctx.task_index]} (${ctx.task_index})`,
    );
    sleep(1000);

    console.log(
      `finished task ${ctx.tasks[ctx.task_index]} (${ctx.task_index})`,
    );
    return {
      finished_index: ctx.task_index,
      done: !(ctx.task_index < ctx.tasks.length),
    };
  },
};

startServer(handlers, PORT);
