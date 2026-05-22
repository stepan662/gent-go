// Task handler implementations for the order-pipeline playground.
// HTTP plumbing, routing, and AJV validation live in generated/server.ts.
//
// Usage: bun run playground:server

import { startServer, type Handlers } from "./generated/server.ts";
import { PORT } from "./process.ts";

const handlers: Handlers = {
  async validate_order({ amount }) {
    console.log("validating order, amount:", amount);
    if (amount <= 0) return { valid: false, reason: "amount must be positive" };
    if (amount > 10_000)
      return { valid: false, reason: "amount exceeds $10,000 limit" };
    return { valid: true };
  },

  async charge_card({ customer_id, amount }) {
    console.log(`charging ${customer_id} $${amount.toFixed(2)}`);
    return { charged: true, transaction_id: `txn_${Date.now()}` };
  },

  async reject_order({ reason }) {
    const msg = reason ?? "validation failed";
    console.log(`rejecting order: ${msg}`);
    return { rejected: true, reason: msg };
  },
};

startServer(handlers, PORT);
