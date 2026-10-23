import { expect, test } from "vitest";
import type { AccountView } from "./api";
import {
  MAX_PRIORITY,
  MIN_PRIORITY,
  accountStatus,
  accountStatusText,
  availableCount,
  coolingLabel,
  isCooling,
  parsePriority,
  statusLabel,
} from "./account";

test("优先级范围为 -1 到 100", () => {
  expect(MIN_PRIORITY).toBe(-1);
  expect(MAX_PRIORITY).toBe(100);
});

test("合法整数（含边界）返回对应数值", () => {
  expect(parsePriority("-1")).toBe(-1);
  expect(parsePriority("0")).toBe(0);
  expect(parsePriority("42")).toBe(42);
  expect(parsePriority("100")).toBe(100);
});

test("去除首尾空白后再解析", () => {
  expect(parsePriority("  7 ")).toBe(7);
  expect(parsePriority("\t-1\n")).toBe(-1);
});

test("空串与纯空白返回 null", () => {
  expect(parsePriority("")).toBeNull();
  expect(parsePriority("   ")).toBeNull();
});

test("非数字返回 null", () => {
  for (const text of ["abc", "1a", "0x10", "-", "--1", "1 2"]) {
    expect(parsePriority(text)).toBeNull();
  }
});

test("小数与科学计数法返回 null", () => {
  for (const text of ["1.5", "5.0", "0.5", "1e1"]) {
    expect(parsePriority(text)).toBeNull();
  }
});

test("带正号返回 null", () => {
  expect(parsePriority("+5")).toBeNull();
});

test("超出 -1 到 100 的范围返回 null", () => {
  for (const text of ["-2", "-100", "101", "1000"]) {
    expect(parsePriority(text)).toBeNull();
  }
});

// 账号状态与冷却展示。新增的导出名：
//   - coolingLabel(reason?: string): string：冷却中的状态文字，带原因；reason 为 "network" 时显示"网络错误"；无原因时只显示"冷却中"。
// 状态判定沿用 accountStatus：禁用 > 冷却 > 启用。

const NOW = Date.parse("2026-10-22T12:00:00Z");

/** 构造一个账号视图，默认未禁用、未冷却。 */
function accountOf(overrides: Partial<AccountView> = {}): AccountView {
  return {
    id: "alice",
    user: "alice",
    host: "",
    disabled: false,
    priority: 0,
    stats: { requests: 0, errors: 0 },
    ...overrides,
  };
}

/** 相对 NOW 的 RFC3339 时间。 */
function isoAt(offsetMs: number): string {
  return new Date(NOW + offsetMs).toISOString();
}

test("状态文字为启用中、禁用中、冷却中", () => {
  expect(statusLabel).toEqual({
    enabled: "启用中",
    disabled: "禁用中",
    cooling: "冷却中",
  });
});

test("状态优先级：禁用 > 冷却 > 启用", () => {
  expect(accountStatus(accountOf(), NOW)).toBe("enabled");
  expect(
    accountStatus(accountOf({ cooldown_until: isoAt(60_000) }), NOW),
  ).toBe("cooling");
  expect(
    accountStatus(
      accountOf({ disabled: true, cooldown_until: isoAt(60_000) }),
      NOW,
    ),
  ).toBe("disabled");
});

test("冷却截止时间已过或无效时不是冷却中", () => {
  expect(accountStatus(accountOf({ cooldown_until: isoAt(-1) }), NOW)).toBe(
    "enabled",
  );
  expect(
    accountStatus(accountOf({ cooldown_until: "not-a-time" }), NOW),
  ).toBe("enabled");
});

test("可用账号数只计未禁用且未冷却的账号", () => {
  const accounts = [
    accountOf({ id: "a" }),
    accountOf({ id: "b", cooldown_until: isoAt(60_000) }),
    accountOf({ id: "c", disabled: true }),
    accountOf({ id: "d", cooldown_until: isoAt(-60_000) }),
  ];
  expect(availableCount(accounts, NOW)).toBe(2);
});

test("冷却文字带原因：状态码直接显示，网络错误显示为网络错误", () => {
  expect(coolingLabel("403")).toBe("冷却中 · 403");
  expect(coolingLabel("401")).toBe("冷却中 · 401");
  expect(coolingLabel("429")).toBe("冷却中 · 429");
  expect(coolingLabel("502")).toBe("冷却中 · 502");
  expect(coolingLabel("network")).toBe("冷却中 · 网络错误");
});

test("没有原因时冷却文字只显示冷却中", () => {
  expect(coolingLabel(undefined)).toBe("冷却中");
  expect(coolingLabel("")).toBe("冷却中");
});

// 账号既禁用又冷却中时，徽标仍为"禁用中"，但卡片仍须显示冷却原因、截止时间与「解除冷却」按钮；
// 按钮是否显示由 isCooling 决定：冷却截止时间晚于 now 即为冷却中，与禁用状态无关。

test("禁用且冷却中：徽标为禁用中，但可以解除冷却", () => {
  const account = accountOf({
    disabled: true,
    cooldown_until: isoAt(60_000),
    cooldown_reason: "403",
  });
  expect(accountStatus(account, NOW)).toBe("disabled");
  expect(accountStatusText(account, NOW)).toBe("禁用中");
  expect(isCooling(account, NOW)).toBe(true);
});

test("未禁用且冷却中：可以解除冷却", () => {
  const account = accountOf({ cooldown_until: isoAt(60_000), cooldown_reason: "network" });
  expect(isCooling(account, NOW)).toBe(true);
});

test("冷却截止时间已过、缺失或无效时没有可解除的冷却", () => {
  expect(isCooling(accountOf({ cooldown_until: isoAt(-1) }), NOW)).toBe(false);
  expect(isCooling(accountOf(), NOW)).toBe(false);
  expect(isCooling(accountOf({ cooldown_until: "not-a-time" }), NOW)).toBe(false);
  expect(isCooling(accountOf({ disabled: true }), NOW)).toBe(false);
});
