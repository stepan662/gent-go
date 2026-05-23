// Task handler implementations for the order-pipeline playground.
// HTTP plumbing, routing, and AJV validation live in generated/server.ts.
//
// Usage: bun run playground:server

import { startServer, type Handlers } from "./generated/server.ts";
import { CheckFraudInput } from "./generated/types.ts";
import { PORT } from "./process.ts";

const handlers: Handlers = {
  async save_order({ data: { amount } }) {
    console.log("validating order, amount:", amount);
    if (amount <= 0) return { valid: false, reason: "amount must be positive" };
    if (amount > 10000)
      return { valid: false, reason: "amount exceeds $10,000 limit" };
    return {};
  },
  check_fraud: function (
    ctx: CheckFraudInput,
  ): Promise<Record<string, unknown>> {
    console.log("checking for fraud:", ctx);
    if (ctx.valid) {
      console.log("no fraud detected");
      return Promise.resolve({ fraud: false });
    } else {
      console.log("fraud detected, rejecting order");
      return Promise.resolve({ fraud: true });
    }
  },
};

startServer(handlers, PORT);
