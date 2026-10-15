import { test, expect } from "vitest";
import { usedPercent, resetLabel, buckets } from "./quota";
test("credits and budget percentages", () => {
  expect(usedPercent(0.96)).toBe(4);
  expect(usedPercent(1)).toBe(0);
  expect(usedPercent(0.0099)).toBe(99);
});
test("reset timezone and date-only precision", () => {
  expect(
    resetLabel("2026-10-01T00:00:00Z", "zh-CN", "Asia/Shanghai"),
  ).toBe("10月1日 08:00 重置");
  expect(
    resetLabel("2026-10-01T16:30:00Z", "zh-CN", "America/Los_Angeles"),
  ).toBe("10月1日 09:30 重置");
  expect(resetLabel("2026-10-01", "zh-CN", "America/Los_Angeles")).toBe(
    "10月1日重置",
  );
  expect(resetLabel("invalid")).toBe("");
});
test("默认文案为中文", () => {
  expect(resetLabel("2026-10-01")).toBe("10月1日重置");
});
test("unknown quota is not zero", () => {
  expect(buckets({})).toEqual([]);
  expect(
    buckets({
      groups: [{ buckets: [{ window: "credits", remainingFraction: NaN }] }],
    }),
  ).toEqual([]);
});
