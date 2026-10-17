import { describe, expect, it } from "vitest";
import { fmtDuration, fmtRise } from "./format";

describe("fmtRise", () => {
  it("renders a rise as whole percent, none as a dash", () => {
    expect(fmtRise(0.03)).toBe("+3%");
    expect(fmtRise(0.010000000000000009)).toBe("+1%");
    expect(fmtRise(0)).toBe("—");
  });
});

describe("fmtDuration", () => {
  it("renders minutes, then hours and padded minutes", () => {
    expect(fmtDuration(45 * 60)).toBe("45m");
    expect(fmtDuration(70 * 60)).toBe("1h10m");
    expect(fmtDuration(125 * 60 + 20)).toBe("2h05m");
  });
});
