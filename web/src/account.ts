// 账号状态与统计汇总的纯函数，概览页与账号页共用。
import type { AccountView } from "./api";

export type AccountStatus = "disabled" | "cooling" | "enabled";

export const statusLabel: Record<AccountStatus, string> = {
  disabled: "已禁用",
  cooling: "冷却中",
  enabled: "已启用",
};

/** 冷却截止时间晚于 now 时视为冷却中。 */
export function isCooling(account: AccountView, now: number): boolean {
  if (!account.cooldown_until) return false;
  const until = Date.parse(account.cooldown_until);
  return Number.isFinite(until) && until > now;
}

/** 状态优先级：已禁用 > 冷却中 > 已启用。 */
export function accountStatus(
  account: AccountView,
  now = Date.now(),
): AccountStatus {
  if (account.disabled) return "disabled";
  return isCooling(account, now) ? "cooling" : "enabled";
}

/** 可用账号数：未禁用且未冷却。 */
export function availableCount(accounts: AccountView[], now = Date.now()) {
  return accounts.filter((a) => accountStatus(a, now) === "enabled").length;
}

/** 汇总所有账号的请求数与错误数。 */
export function sumStats(accounts: AccountView[]) {
  return accounts.reduce(
    (total, a) => ({
      requests: total.requests + a.stats.requests,
      errors: total.errors + a.stats.errors,
    }),
    { requests: 0, errors: 0 },
  );
}

/** 账号地址展示：空 host 即 github.com。 */
export function hostLabel(host: string): string {
  return host || "github.com";
}

/** 时间展示；缺失或无效时返回占位符。 */
export function formatTime(value?: string): string {
  if (!value) return "—";
  const date = new Date(value);
  return Number.isFinite(date.getTime())
    ? date.toLocaleString("zh-CN", { hour12: false })
    : "—";
}
