// Task handler implementations for the order-pipeline playground.
// HTTP plumbing, routing, and AJV validation live in generated/server.ts.
//
// Usage: bun run playground:server

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

import { startServer, type Handlers } from "./generated/server.ts";

const PORT = 3001;

const handlers: Handlers = {
  success: async (_ctx) => {
    await sleep(50);
    return { ok: true };
  },
  failure: async ({ error }) => {
    console.log(error);
    return {};
  },
};

startServer(handlers, PORT);
