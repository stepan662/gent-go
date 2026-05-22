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

// ─── schemas ───────────────────────────────────────────────────────────────

export const inputSchema = {
  type: "object",
  properties: {
    customer_id: { type: "string" },
    amount: { type: "number" },
    card_token: { type: "string" },
  },
  required: ["customer_id", "amount", "card_token"],
  additionalProperties: false,
} as const;

export const stepSchemas = {
  validate_order: {
    type: "object",
    properties: {
      valid: { type: "boolean" },
      reason: { type: "string" },
    },
    required: ["valid"],
    additionalProperties: false,
  },
  charge_card: {
    type: "object",
    properties: {
      charged: { type: "boolean" },
      transaction_id: { type: "string" },
    },
    required: ["charged", "transaction_id"],
    additionalProperties: false,
  },
  reject_order: {
    type: "object",
    properties: {
      rejected: { type: "boolean" },
      reason: { type: "string" },
    },
    required: ["rejected", "reason"],
    additionalProperties: false,
  },
} as const;

// ─── process definition ────────────────────────────────────────────────────

export const processDefinition = {
  name: "order-pipeline",
  version: 1,
  input_schema: inputSchema,
  steps: [
    {
      id: "validate_order",
      type: "task" as const,
      transport: "http" as const,
      endpoint: `http://localhost:${PORT}/validate_order`,
      timeout_ms: 2000,
      retries: 0,
      output_schema: stepSchemas.validate_order,
      params: { amount: "input.amount" },
    },
    {
      id: "check_valid",
      type: "conditional" as const,
      condition: "outputs.validate_order.valid == true",
      then: [
        {
          id: "charge_card",
          type: "task" as const,
          transport: "http" as const,
          endpoint: `http://localhost:${PORT}/charge_card`,
          timeout_ms: 5000,
          retries: 2,
          output_schema: stepSchemas.charge_card,
          params: { customer_id: "input.customer_id", amount: "input.amount" },
        },
      ],
      else: [
        {
          id: "reject_order",
          type: "task" as const,
          transport: "http" as const,
          endpoint: `http://localhost:${PORT}/reject_order`,
          timeout_ms: 2000,
          retries: 0,
          output_schema: stepSchemas.reject_order,
          params: { reason: "outputs.validate_order.reason" },
        },
      ],
    },
  ],
} satisfies PutDefinitionBody;
