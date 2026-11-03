import { defineConfig } from "astro/config";
import { siteUrl } from "./src/lib/site.ts";

// A fully static site (dist/), deployed to GitHub Pages by
// .github/workflows/website.yml. See README.md.
export default defineConfig({
  site: siteUrl,
  output: "static",
  // Directory-style URLs (`/docs/` → docs/index.html), as GitHub Pages serves
  // them, so later sections such as /docs can be added as directories.
  trailingSlash: "ignore",
  build: {
    format: "directory",
    // The stylesheet is small: inlining it saves a render-blocking request.
    inlineStylesheets: "always",
  },
  devToolbar: { enabled: false },
  // No Markdown pages yet; Shiki's inline styles would conflict with the CSP.
  markdown: { syntaxHighlight: false },
  security: {
    // A <meta> Content-Security-Policy with hashes of the inline styles and
    // scripts. Nothing is loaded from other origins.
    csp: {
      directives: [
        "default-src 'none'",
        "img-src 'self'",
        "connect-src 'self'",
        "base-uri 'none'",
        "form-action 'none'",
      ],
    },
  },
});
