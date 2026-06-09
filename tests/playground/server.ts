// Task handler implementations for the order-pipeline playground.
// HTTP plumbing, routing, and AJV validation live in generated/server.ts.
//
// Usage: bun run playground:server

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

import { startServer, type Handlers } from "./generated/server.ts";

const PORT = 3001;

const handlers: Handlers = {
  start: async (_ctx) => {
    await sleep(50);
    throw new Error("error");
    return { ok: true };
  },
  finale: async ({ error }) => {
    console.log(error);
    return { ok: true };
  },
};

startServer(handlers, PORT);
