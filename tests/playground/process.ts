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
      tasks: { type: "array", item: { type: "string" } },
    },
    required: ["tasks"],
  },
  steps: [
    {
      id: "loop",
      transport: "http" as const,
      endpoint: `http://localhost:${PORT}/loop`,
      params: {
        tasks: "{{input.tasks}}",
        task_index:
          "{{outputs.loop.finished_index != nil ? outputs.loop.finished_index + 1 : 0}}",
      },
      output_schema: {
        type: "object",
        properties: {
          finished_index: { type: "number" },
          done: { type: "boolean" },
        },
        required: ["finished_index", "done"],
      },
      switch: {
        "!self.done": "#loop",
        default: "$end",
      },
    },
  ],
} as const satisfies PutDefinitionBody;
