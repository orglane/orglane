import { createIntl } from "react-intl";
import { describe, expect, it } from "vitest";

import { exactTime, relativeTime } from "./time";

const intl = createIntl({ locale: "en", timeZone: "UTC" });

describe("time formatting", () => {
  it("formats exact timestamps to the minute with the time zone", () => {
    expect(exactTime(intl, "2026-11-03T13:04:59Z")).toBe("Nov 3, 2026, 1:04 PM UTC");
  });

  it("formats relative times", () => {
    const now = new Date("2026-11-03T13:00:00Z");
    expect(relativeTime(intl, "2026-11-03T12:55:00Z", now)).toBe("5 minutes ago");
    expect(relativeTime(intl, "2026-11-02T13:00:00Z", now)).toBe("yesterday");
    expect(relativeTime(intl, "2026-11-03T12:59:50Z", now)).toBe("now");
  });
});
