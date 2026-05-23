// order-pipeline — a worked example process for the gent playground.
//
// This file is the single source of truth for:
//   • the process definition posted to the gent API
//   • the JSON Schemas used for runtime validation
//   • the schemas that codegen.ts turns into TypeScript types
//
// Edit this file, then re-run `bun run playground:generate` to regenerate types.

import type { paths } from "../generated/api.ts";

type PutDefinitionBody = NonNullable<
  paths["/definitions"]["put"]["requestBody"]
>["content"]["application/json"];

export const PORT = 3001;

// ─── process definition ────────────────────────────────────────────────────

export const processDefinition = {
  name: "order-pipeline",
  version: 1,
  input_schema: {
    type: "object",
    properties: {
      customer_id: { type: "string" },
      amount: { type: "number" },
      card_token: { type: "string" },
    },
    required: ["customer_id", "amount", "card_token"],
  },
  steps: [
    {
      id: "save_order",
      type: "task" as const,
      transport: "http" as const,
      endpoint: `http://localhost:${PORT}/save_order`,
      timeout_ms: 2000,
      retries: 0,
      params: {
        data: "input",
      },
      output_schema: {
        type: "object",
        properties: { valid: { type: "boolean" } },
      },
    },
    {
      id: "check_fraud",
      type: "task" as const,
      transport: "http" as const,
      endpoint: `http://localhost:${PORT}/check_fraud`,
      timeout_ms: 2000,
      retries: 0,
      params: {
        valid: "outputs.save_order.valid",
      },
    },
  ],
} as const satisfies PutDefinitionBody;
