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
            GENT_PORT: "8889",
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
          include: ["integration/**/*_test.ts", "cli/**/*_test.ts"],
          testTimeout: 30_000,
          env: { GENT_PORT: "8888" },
        },
      },
      ...pgProject,
    ],
  },
});
