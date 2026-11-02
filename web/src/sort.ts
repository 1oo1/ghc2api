// 账号列表的排序：点击表头在 降序 → 升序 → 默认顺序 之间切换。
import type { AccountView } from "./api";
import { buckets } from "./quota";

export type SortKey = "quota" | "requests" | "errors" | "lastUsed" | "priority";
export type SortDirection = "asc" | "desc";
/** null 表示默认顺序（服务端返回的顺序）。 */
export type SortState = { key: SortKey; direction: SortDirection } | null;

/** 点击某列表头后的下一个排序状态：未按该列排序 → 降序 → 升序 → 默认顺序。 */
export function nextSort(current: SortState, key: SortKey): SortState {
  if (current?.key !== key) return { key, direction: "desc" };
  return current.direction === "desc" ? { key, direction: "asc" } : null;
}

/** 账号在某列上的排序值；undefined 表示缺失（无最近使用时间、无额度数据）。 */
function sortValue(account: AccountView, key: SortKey): number | undefined {
  switch (key) {
    case "requests":
      return account.stats.requests;
    case "errors":
      return account.stats.errors;
    case "priority":
      return account.priority;
    case "lastUsed": {
      const time = Date.parse(account.stats.last_used_at ?? "");
      return Number.isFinite(time) ? time : undefined;
    }
    case "quota": {
      // 取已用比例最高的窗口，已用越多值越大。
      const windows = account.quota ? buckets(account.quota) : [];
      if (windows.length === 0) return undefined;
      return 1 - Math.min(...windows.map((b) => b.remainingFraction));
    }
  }
}

/** 返回排序后的新数组，不修改入参。缺失值总排在最后；值相等时保持原顺序。 */
export function sortAccounts(accounts: AccountView[], sort: SortState): AccountView[] {
  if (!sort) return [...accounts];
  const sign = sort.direction === "desc" ? -1 : 1;
  // Array.prototype.sort 是稳定排序，比较结果为 0 时保持原顺序。
  return [...accounts].sort((a, b) => {
    const x = sortValue(a, sort.key);
    const y = sortValue(b, sort.key);
    if (x === undefined || y === undefined)
      return (x === undefined ? 1 : 0) - (y === undefined ? 1 : 0);
    return (x - y) * sign;
  });
}
