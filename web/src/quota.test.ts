import { test, expect } from "vitest";
import {
  usedPercent,
  resetLabel,
  buckets,
  planLabel,
  creditsUsed,
} from "./quota";
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

test("套餐名优先取 plan，其次 tierName", () => {
  expect(
    planLabel({ subscription: { plan: "Pro", tierName: "Individual" } }),
  ).toBe("Pro");
  expect(planLabel({ subscription: { tierName: "Individual" } })).toBe(
    "Individual",
  );
});

test("plan 为空串时回退到 tierName", () => {
  expect(planLabel({ subscription: { plan: "", tierName: "Individual" } })).toBe(
    "Individual",
  );
});

test("没有套餐信息时套餐名为 GitHub Copilot", () => {
  expect(planLabel(undefined)).toBe("GitHub Copilot");
  expect(planLabel({})).toBe("GitHub Copilot");
  expect(planLabel({ subscription: {} })).toBe("GitHub Copilot");
  expect(planLabel({ subscription: { plan: "", tierName: "" } })).toBe(
    "GitHub Copilot",
  );
});

test("已用 Credits 取 summary 中 credits_used 的值", () => {
  expect(
    creditsUsed({
      summary: [
        { key: "credits_total", value: 300 },
        { key: "credits_used", value: 12.5 },
      ],
    }),
  ).toBe(12.5);
});

test("已用 Credits 为 0 时返回 0 而不是 undefined", () => {
  expect(creditsUsed({ summary: [{ key: "credits_used", value: 0 }] })).toBe(0);
});

test("多个 credits_used 取第一个有限数值的条目", () => {
  expect(
    creditsUsed({
      summary: [
        { key: "credits_used", value: NaN },
        { key: "credits_used", value: 3 },
        { key: "credits_used", value: 9 },
      ],
    }),
  ).toBe(3);
});

test("额度缺失、summary 缺失或没有 credits_used 时返回 undefined", () => {
  expect(creditsUsed(undefined)).toBeUndefined();
  expect(creditsUsed({})).toBeUndefined();
  expect(creditsUsed({ summary: [] })).toBeUndefined();
  expect(
    creditsUsed({ summary: [{ key: "credits_total", value: 300 }] }),
  ).toBeUndefined();
});

test("credits_used 不是有限数时返回 undefined", () => {
  for (const value of [NaN, Infinity, -Infinity]) {
    expect(
      creditsUsed({ summary: [{ key: "credits_used", value }] }),
    ).toBeUndefined();
  }
});
