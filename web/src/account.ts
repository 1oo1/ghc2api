// 账号状态、统计汇总与额度提示的纯函数，账号列表与账号卡片共用。
import type { AccountView } from "./api";

export type AccountStatus = "disabled" | "cooling" | "enabled";

export const statusLabel: Record<AccountStatus, string> = {
  disabled: "禁用中",
  cooling: "冷却中",
  enabled: "启用中",
};

/**
 * 冷却中的状态文字，带触发冷却的原因：状态码直接显示（如 "冷却中 · 403"），
 * "network" 显示为网络错误；没有原因时只显示 "冷却中"。
 */
export function coolingLabel(reason?: string): string {
  if (!reason) return statusLabel.cooling;
  const text = reason === "network" ? "网络错误" : reason;
  return `${statusLabel.cooling} · ${text}`;
}

/** 账号状态的展示文字：冷却中附带原因。 */
export function accountStatusText(account: AccountView, now = Date.now()): string {
  const status = accountStatus(account, now);
  return status === "cooling" ? coolingLabel(account.cooldown_reason) : statusLabel[status];
}

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

/** 额度相关提示：查询失败在前，数据过期（被限流）在后；都没有时返回空数组。 */
export function quotaWarnings(
  account: Pick<AccountView, "quota" | "quota_error">,
): string[] {
  const warnings: string[] = [];
  if (account.quota_error) warnings.push(`额度查询失败：${account.quota_error}`);
  if (account.quota?.stale)
    warnings.push("额度数据可能已过期：GitHub 限流，显示的是上一次成功获取的结果。");
  return warnings;
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

/** 优先级的取值范围（含两端），与服务端 MinPriority、MaxPriority 一致。 */
export const MIN_PRIORITY = -1;
export const MAX_PRIORITY = 100;

/**
 * 解析优先级输入。去除首尾空白后必须是可选负号加十进制数字（不接受正号、小数与科学计数法），
 * 且在 MIN_PRIORITY 到 MAX_PRIORITY 之间；否则返回 null。
 */
export function parsePriority(text: string): number | null {
  const trimmed = text.trim();
  if (!/^-?\d+$/.test(trimmed)) return null;
  const value = Number(trimmed);
  return value >= MIN_PRIORITY && value <= MAX_PRIORITY ? value : null;
}
