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
      ttl: { type: "integer" },
    },
    required: ["ttl"],
  },
  steps: [
    {
      id: "recursion_condition",
      switch: {
        "{{input.ttl > 0}}": "#recursion",
        default: "$end",
      },
    },
    {
      id: "recursion",
      call: {
        type: "child_process",
        processes: [
          {
            name: "order-pipeline",
            input: {
              ttl: "{{input.ttl - 1}}",
            },
          },
        ],
      },
    },
  ],
} as const satisfies PutDefinitionBody;
