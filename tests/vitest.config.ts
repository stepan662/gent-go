import { defineConfig } from "vitest/config";

const pgProject = process.env.POSTGRES_DSN
  ? [
      {
        test: {
          name: "postgres",
          globalSetup: ["./helpers/server-pg.ts"],
          include: ["integration/**/*_test.ts", "cli/**/*_test.ts"],
          testTimeout: 30_000,
          env: {
            GENROC_PORT: "8889",
            POSTGRES_DSN: process.env.POSTGRES_DSN,
          },
        },
      },
    ]
  : [];

export default defineConfig({
  test: {
    projects: [
      {
        test: {
          name: "sqlite",
          globalSetup: ["./helpers/server.ts"],
          include: ["integration/**/*_test.ts", "cli/**/*_test.ts", "tick/**/*_test.ts"],
          testTimeout: 60_000,
          env: { GENROC_PORT: "8888" },
        },
      },
      ...pgProject,
      {
        // Stress tests spawn their own worker fleet, so no shared globalSetup
        // server. Runs the SQLite backend always and Postgres when DSN is set.
        test: {
          name: "stress",
          include: ["stress/**/*_test.ts"],
          testTimeout: 120_000,
          // Run stress files one at a time: each saturates a worker fleet against the
          // single Postgres, so running two concurrently starves the other's workers
          // (connection/CPU contention) and flakes their startup.
          fileParallelism: false,
          env: process.env.POSTGRES_DSN
            ? { POSTGRES_DSN: process.env.POSTGRES_DSN }
            : {},
        },
      },
    ],
  },
});
