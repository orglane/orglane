import "@testing-library/jest-dom/vitest";

// Dates render the same on every machine.
process.env.TZ = "UTC";

import { cleanup } from "@testing-library/react";
import { afterEach } from "vitest";

afterEach(() => {
  cleanup();
  globalThis.localStorage?.clear();
});
