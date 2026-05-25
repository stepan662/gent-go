// Task handler implementations for the order-pipeline playground.
// HTTP plumbing, routing, and AJV validation live in generated/server.ts.
//
// Usage: bun run playground:server

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

import { startServer, type Handlers } from "./generated/server.ts";
import { PORT } from "./process.ts";

const handlers: Handlers = {
  async loop(ctx) {
    return {
      finished_index: ctx.task_index,
      done: !(ctx.task_index < ctx.tasks),
    };
  },
  async finish(ctx) {
    console.log(`finished in ${Date.now() - ctx.start_time}`);
    return {};
  },
};

startServer(handlers, PORT);
