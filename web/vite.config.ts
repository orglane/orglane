import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defaultClientConditions, type Plugin } from "vite";
import { defineConfig } from "vitest/config";

const src = fileURLToPath(new URL("./src", import.meta.url));

// The core embeds web/dist (go:embed), which must exist even before the
// first build: dist/.gitkeep is committed. Vite empties dist on every build,
// so put it back.
function keepDist(): Plugin {
  return {
    name: "orglane-keep-dist",
    apply: "build",
    closeBundle() {
      writeFileSync(fileURLToPath(new URL("./dist/.gitkeep", import.meta.url)), "");
    },
  };
}

const escapeHtml = (text: string) =>
  text.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");

// A basic sign-in form for browsers that cannot run the app (no JavaScript
// modules), such as the OpenID Conformance Suite's HtmlUnit. It sits hidden
// in #root, where React replaces it, and only public/basic-sign-in.js
// (<script nomodule>) reveals it, on /sign-in?authRequest=... (OIDC). Its
// text comes from the English catalog.
function basicSignIn(): Plugin {
  return {
    name: "orglane-basic-sign-in",
    transformIndexHtml(html) {
      const catalog = JSON.parse(
        readFileSync(new URL("./src/i18n/locales/en.json", import.meta.url), "utf8"),
      ) as Record<string, string>;
      const t = (id: string) => {
        const text = catalog[id];
        if (text === undefined) {
          throw new Error(`basic sign-in: message ${id} is not in the catalog`);
        }
        return escapeHtml(text);
      };
      const errors = {
        credentials: "signIn.wrongPassword",
        limited: "signIn.limited",
        failed: "error.server",
      };
      const form = [
        '<form id="basic-sign-in" method="post" action="/oidc/login" hidden>',
        `<h1>${t("signIn.title")}</h1>`,
        ...Object.entries(errors).map(
          ([code, id]) => `<p role="alert" data-error="${code}" hidden>${t(id)}</p>`,
        ),
        '<input type="hidden" name="authRequest">',
        `<p><label for="email">${t("signIn.email")}</label><br><input id="email" name="email" type="email" autocomplete="username" required></p>`,
        `<p><label for="password">${t("signIn.password")}</label><br><input id="password" name="password" type="password" autocomplete="current-password" required></p>`,
        `<p><button id="submit" type="submit">${t("signIn.submit")}</button></p>`,
        "</form>",
      ].join("");
      return html
        .replace('<div id="root"></div>', `<div id="root">${form}</div>`)
        .replace("</body>", '  <script nomodule src="/basic-sign-in.js"></script>\n  </body>');
    },
  };
}

// The API the dev server proxies to: run the core with
// ORGLANE_DOMAIN_WORKSPACE_HOST=localhost:5173 so that cookies, the Origin
// check and passkeys see the dev server's origin (see web/README.md).
const core = process.env.ORGLANE_DEV_API ?? "http://127.0.0.1:8080";

export default defineConfig({
  plugins: [react(), tailwindcss(), basicSignIn(), keepDist()],
  resolve: {
    conditions: ["source", ...defaultClientConditions],
    alias: { "@": src },
  },
  build: {
    target: "es2022",
    // The Content-Security-Policy allows only same-origin resources: no
    // data: URLs, so never inline assets.
    assetsInlineLimit: 0,
    chunkSizeWarningLimit: 800,
  },
  server: {
    port: 5173,
    strictPort: true,
    proxy: Object.fromEntries(
      ["/api", "/oidc", "/.well-known", "/healthz", "/readyz"].map((p) => [p, { target: core }]),
    ),
  },
  test: {
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
    include: ["src/**/*.test.{ts,tsx}", "scripts/**/*.test.ts"],
    css: false,
  },
});
