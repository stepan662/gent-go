import { spawnSync } from "child_process";
import { join } from "path";
import { tmpdir } from "os";
import { writeFileSync } from "fs";
import { BASE_URL } from "./constants.ts";

const ROOT = new URL("../../", import.meta.url).pathname;

let cachedBin: string | null = null;

export function buildGentctlBinary(): string {
  if (cachedBin) return cachedBin;
  const result = spawnSync("make", ["build"], {
    cwd: ROOT,
    stdio: ["ignore", "ignore", "inherit"],
  });
  if (result.status !== 0) throw new Error("Failed to build gentctl binary");
  cachedBin = join(ROOT, "gentctl");
  return cachedBin;
}

export interface CliResult {
  stdout: string;
  stderr: string;
  exitCode: number;
  ok: boolean;
}

export function runCli(bin: string, args: string[]): CliResult {
  const result = spawnSync(bin, args, {
    env: { ...process.env, GENT_SERVER: BASE_URL },
    encoding: "utf8",
  });
  return {
    stdout: result.stdout ?? "",
    stderr: result.stderr ?? "",
    exitCode: result.status ?? 1,
    ok: result.status === 0,
  };
}

/** Write one or more process definitions to a temp YAML file and return the path. */
export function writeDefs(defs: object[]): string {
  const path = join(
    tmpdir(),
    `gent_cli_test_${Date.now()}_${Math.random().toString(36).slice(2)}.yaml`,
  );
  const yaml = defs.map((d) => jsonToYaml(d)).join("\n---\n");
  writeFileSync(path, yaml, "utf8");
  return path;
}

/** Minimal JSON-to-YAML converter sufficient for process definition objects. */
function jsonToYaml(value: unknown, indent = 0): string {
  const pad = "  ".repeat(indent);
  if (value === null || value === undefined) return "null";
  if (typeof value === "boolean") return String(value);
  if (typeof value === "number") return String(value);
  if (typeof value === "string") {
    if (
      /[:#{}[\],&*?|<>=!%@`]/.test(value) ||
      value === "" ||
      value === "true" ||
      value === "false" ||
      value === "null"
    ) {
      return JSON.stringify(value);
    }
    return value;
  }
  if (Array.isArray(value)) {
    if (value.length === 0) return "[]";
    return value
      .map((v) => {
        const rendered = jsonToYaml(v, indent + 1);
        const lines = rendered.split("\n");
        // First line uses "- " prefix; continuation lines keep their indentation.
        return [`${pad}- ${lines[0].trimStart()}`, ...lines.slice(1)].join(
          "\n",
        );
      })
      .join("\n");
  }
  if (typeof value === "object") {
    const entries = Object.entries(value as Record<string, unknown>);
    if (entries.length === 0) return "{}";
    return entries
      .map(([k, v]) => {
        if (
          v === null ||
          v === undefined ||
          (typeof v !== "object" && !Array.isArray(v))
        ) {
          return `${pad}${k}: ${jsonToYaml(v, indent + 1)}`;
        }
        if (Array.isArray(v) && (v as unknown[]).length === 0)
          return `${pad}${k}: []`;
        // Objects and non-empty arrays always use block (next-line) style.
        return `${pad}${k}:\n${jsonToYaml(v, indent + 1)}`;
      })
      .join("\n");
  }
  return String(value);
}
