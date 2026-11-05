import { type ChildProcess, spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { mkdtempSync, rmSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { test as base, type CDPSession, type Page } from "@playwright/test";

import { loadState, run } from "./env";

export interface Instance {
  /** http://localhost:<port> (http://orglane.localhost:<port> with realAgent) */
  url: string;
  /** A fresh one-time setup token. */
  setupToken(): string;
  /** Runs `orglane cli <args>` against this instance and returns its output. */
  cli(...args: string[]): string;
  /** Runs SQL in this instance's database (test setup only) and returns its output. */
  sql(query: string): string;
  /** The server's log so far, for failures. */
  log(): string;
}

async function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = createServer();
    srv.once("error", reject);
    srv.listen(0, "127.0.0.1", () => {
      const address = srv.address();
      srv.close(() =>
        typeof address === "object" && address
          ? resolve(address.port)
          : reject(new Error("no port")),
      );
    });
  });
}

async function waitReady(url: string, server: ChildProcess): Promise<void> {
  for (let i = 0; i < 120; i++) {
    if (server.exitCode !== null) {
      throw new Error(`orglane server exited with ${server.exitCode}`);
    }
    try {
      if ((await fetch(`${url}/readyz`)).ok) {
        return;
      }
    } catch {
      // not listening yet
    }
    await new Promise((r) => setTimeout(r, 250));
  }
  throw new Error("orglane server did not become ready");
}

const authenticators = new WeakMap<CDPSession, string>();

/** The ID of the virtual authenticator of the webauthn fixture. */
export function authenticator(cdp: CDPSession): string {
  const id = authenticators.get(cdp);
  if (!id) throw new Error("no virtual authenticator");
  return id;
}

export const test = base.extend<{
  instance: Instance;
  demo: boolean;
  /** Managed services on, with an agent that never answers (its socket does not exist). */
  agent: boolean;
  /**
   * Managed services on with a real `orglane agent` process, without the
   * Compose bundle (so installs fail once the agent needs the bundle's
   * PostgreSQL). The workspace is orglane.localhost, as managed services
   * need in local mode.
   */
  realAgent: boolean;
  webauthn: CDPSession;
  mailpit: { smtpPort: number; api: string };
}>({
  demo: [false, { option: true }],
  agent: [false, { option: true }],
  realAgent: [false, { option: true }],
  // Real SMTP, isolated per test; only starts for tests requesting it.
  // biome-ignore lint/correctness/noEmptyPattern: Playwright fixture dependencies
  mailpit: async ({}, use) => {
    const container = run("docker", [
      "run",
      "-d",
      "--rm",
      "-p",
      "127.0.0.1::1025",
      "-p",
      "127.0.0.1::8025",
      "axllent/mailpit:v1.29.7",
    ]);
    try {
      const port = (p: string) =>
        Number(run("docker", ["port", container, p]).split("\n")[0]?.split(":").pop());
      const api = `http://127.0.0.1:${port("8025/tcp")}`;
      for (let i = 0; ; i++) {
        try {
          if ((await fetch(`${api}/livez`)).ok) break;
        } catch {
          /* starting */
        }
        if (i === 60) throw new Error("Mailpit did not start");
        await new Promise((resolve) => setTimeout(resolve, 200));
      }
      await use({ smtpPort: port("1025/tcp"), api });
    } finally {
      run("docker", ["rm", "-f", container]);
    }
  },
  // A fresh database and `orglane server` for every test, reached on
  // localhost (the plain-HTTP development mode).
  instance: async ({ demo, agent, realAgent }, use, testInfo) => {
    const state = loadState();
    const db = `e2e_${randomBytes(6).toString("hex")}`;
    run("docker", ["exec", state.container, "createdb", "-U", "orglane", db]);
    const port = await freePort();
    const base = realAgent ? "orglane.localhost" : "localhost";
    const url = `http://${base}:${port}`;
    // The agent's socket directory holds nothing else (the agent sets its mode).
    const agentDir = realAgent ? mkdtempSync(join(tmpdir(), "orglane-e2e-agent-")) : "";
    const agentSecret = randomBytes(32).toString("hex");
    const env = {
      ...process.env,
      ORGLANE_ENVIRONMENT: "development",
      ORGLANE_DATABASE_URL: `${state.postgres}/${db}?sslmode=disable`,
      ORGLANE_DOMAIN_BASE: base,
      ORGLANE_DOMAIN_WORKSPACE_HOST: `${base}:${port}`,
      ORGLANE_DOMAIN_SCHEME: "http",
      ORGLANE_SERVER_LISTEN: `127.0.0.1:${port}`,
      ORGLANE_LOG_FORMAT: "text",
      ORGLANE_EMAIL_RETRY_DELAY: "1s",
      ORGLANE_EMAIL_MAX_ATTEMPTS: "2",
      ...(agent
        ? {
            ORGLANE_AGENT_SOCKET: join(tmpdir(), `orglane-e2e-${db}`, "agent.sock"),
            ORGLANE_AGENT_SECRET: randomBytes(32).toString("hex"),
            ORGLANE_AGENT_SYNC_INTERVAL: "10s",
          }
        : {}),
      ...(realAgent
        ? {
            ORGLANE_AGENT_SOCKET: join(agentDir, "run", "agent.sock"),
            ORGLANE_AGENT_SECRET: agentSecret,
            // Local mode, as install.sh --mode demo --managed runs it: no
            // DNS check of *.localhost service hosts.
            ORGLANE_INGRESS_MODE: "local",
          }
        : {}),
    };
    let log = "";
    let agentProcess: ChildProcess | undefined;
    if (realAgent) {
      agentProcess = spawn(state.binary, ["agent"], {
        env: {
          ...process.env,
          ORGLANE_LOG_FORMAT: "text",
          ORGLANE_AGENT_LISTEN: join(agentDir, "run", "agent.sock"),
          ORGLANE_AGENT_SECRET: agentSecret,
          ORGLANE_AGENT_DATA_DIR: join(agentDir, "data"),
          ORGLANE_AGENT_PEER_UID: "-1",
          // No such Compose project: the agent finds no PostgreSQL.
          ORGLANE_AGENT_COMPOSE_PROJECT: `e2e${randomBytes(4).toString("hex")}`,
        },
        stdio: ["ignore", "pipe", "pipe"],
      });
      agentProcess.stdout?.on("data", (d) => (log += `[agent] ${d}`));
      agentProcess.stderr?.on("data", (d) => (log += `[agent] ${d}`));
    }
    const server = spawn(state.binary, demo ? ["server", "--demo"] : ["server"], {
      env,
      stdio: ["ignore", "pipe", "pipe"],
    });
    server.stdout?.on("data", (d) => (log += d));
    server.stderr?.on("data", (d) => (log += d));
    try {
      await waitReady(url, server);
      await use({
        url,
        setupToken: () =>
          /ost_[A-Za-z0-9_-]+/.exec(run(state.binary, ["cli", "setup-token"], { env }))?.[0] ?? "",
        cli: (...args) => run(state.binary, ["cli", ...args], { env }),
        sql: (query) =>
          run("docker", [
            "exec",
            state.container,
            "psql",
            "-U",
            "orglane",
            "-d",
            db,
            "-v",
            "ON_ERROR_STOP=1",
            "-tAc",
            query,
          ]),
        log: () => log,
      });
    } catch (error) {
      await testInfo.attach("server-startup.log", { body: log, contentType: "text/plain" });
      throw error;
    } finally {
      server.kill("SIGTERM");
      agentProcess?.kill("SIGTERM");
      if (agentDir) rmSync(agentDir, { recursive: true, force: true });
      if (testInfo.status !== testInfo.expectedStatus) {
        await testInfo.attach("orglane.log", { body: log, contentType: "text/plain" });
      }
    }
  },
  // Chromium's virtual WebAuthn authenticator (CDP): a platform passkey
  // provider that always verifies the user.
  webauthn: async ({ page }, use) => {
    const cdp = await page.context().newCDPSession(page);
    await cdp.send("WebAuthn.enable");
    const { authenticatorId } = await cdp.send("WebAuthn.addVirtualAuthenticator", {
      options: {
        protocol: "ctap2",
        transport: "internal",
        hasResidentKey: true,
        hasUserVerification: true,
        isUserVerified: true,
        automaticPresenceSimulation: true,
      },
    });
    authenticators.set(cdp, authenticatorId);
    await use(cdp);
  },
});

export const expect = test.expect;

export const owner = {
  name: "Alex Morgan",
  email: "alex@acme.test",
  username: "alex",
  password: "correct horse battery staple",
};

/** First-run setup through the UI. */
export async function setUp(page: Page, instance: Instance): Promise<void> {
  await page.goto(instance.url);
  await expect(page).toHaveURL(/\/setup$/);
  await page.getByLabel("Setup token").fill(instance.setupToken());
  await page.getByLabel("Full name").fill(owner.name);
  await page.getByLabel("Work email").fill(owner.email);
  await expect(page.getByLabel("Username")).toHaveValue(owner.username);
  await page.getByLabel("Password", { exact: true }).fill(owner.password);
  await page.getByRole("button", { name: "Create owner account" }).click();
}

export async function signOut(page: Page): Promise<void> {
  await page.getByRole("button", { name: `Account menu for ${owner.name}` }).click();
  await page.getByRole("menuitem", { name: "Sign out" }).click();
  await expect(page.getByText("You signed out.")).toBeVisible();
}
