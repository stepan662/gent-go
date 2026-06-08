import createClient from "openapi-fetch";
import { createServer } from "http";
import type { AddressInfo } from "net";
import type { paths } from "../generated/api.ts";
import { BASE_URL } from "./constants.ts";

export const client = createClient<paths>({ baseUrl: BASE_URL });
export const createClientTyped: typeof createClient<paths> = (options) =>
  createClient<paths>(options);

type ApiClient = Pick<typeof client, "GET">;

export async function waitForInstance(
  id: string,
  timeoutMs = 5000,
  apiClient: ApiClient = client,
): Promise<string> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const { data, error } = await apiClient.GET("/instances/{id}", {
      params: { path: { id } },
    });
    if (error) throw new Error(`get_instance failed: ${JSON.stringify(error)}`);
    const status = data?.status;
    if (status === "completed" || status === "failed") return status!;
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(`instance ${id} did not complete within ${timeoutMs}ms`);
}

interface MockServiceOptions {
  // The JSON body sent for every response. Defaults to {}.
  response?: Record<string, unknown>;
  // HTTP status code to return. Defaults to 200.
  statusCode?: number;
  // How long to delay the very first request before responding.
  // 0 (default) = respond immediately.
  // Infinity     = never respond; use this to simulate a worker hanging mid-step.
  firstRequestDelayMs?: number;
}

export async function startMockService(port: number, options: MockServiceOptions = {}) {
  const { response = {}, statusCode = 200, firstRequestDelayMs = 0 } = options;
  const body = JSON.stringify(response);

  let count = 0;
  let resolveFirst!: () => void;
  const firstRequestReceived = new Promise<void>((r) => {
    resolveFirst = r;
  });

  const server = createServer((req, res) => {
    count++;
    req.socket.on("error", () => {}); // suppress ECONNRESET
    res.on("error", () => {});

    if (count === 1) resolveFirst();

    const send = () => {
      res.writeHead(statusCode, { "Content-Type": "application/json" });
      res.end(body);
    };

    if (count === 1 && firstRequestDelayMs > 0) {
      if (isFinite(firstRequestDelayMs)) setTimeout(send, firstRequestDelayMs);
      // Infinity: hold the connection open until the caller closes it.
    } else {
      send();
    }
  });
  server.on("clientError", () => {});
  await new Promise<void>((r) => server.listen(port, r));
  const boundPort = (server.address() as AddressInfo).port;

  return {
    port: boundPort,
    firstRequestReceived,
    requestCount: () => count,
    stop: () => new Promise<void>((r) => server.close(() => r())),
  };
}
