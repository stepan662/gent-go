// Task handler implementations for the order-pipeline playground.
// HTTP plumbing, routing, and AJV validation live in generated/server.ts.
//
// Usage: bun run playground:server

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

import { startServer, type Handlers } from "./generated/server.ts";
import { PORT } from "./process.ts";

const handlers: Handlers = {};

startServer(handlers, PORT);
