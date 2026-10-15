// 额度数据的类型与展示辅助函数，改编自 CPA Management Center 的 Copilot 额度适配器（MIT 许可，见 LICENSES/Management-Center.txt）。
// 字段与服务端 internal/copilot/quota.go 的 JSON 结构一致。

/** 单个额度窗口（Credits、Premium requests、Additional Budget 等）。 */
export type Bucket = {
  window: string;
  description?: string;
  /** 剩余比例，0~1；展示时换算为已用百分比。 */
  remainingFraction: number;
  /** 重置时间：RFC3339 时间或 YYYY-MM-DD。 */
  resetTime?: string;
};
export type Quota = {
  subscription?: { plan?: string; tierName?: string };
  groups?: { displayName?: string; buckets?: Bucket[] }[];
  summary?: { key: string; label?: string; value: number; format?: string }[];
  /** 最近一次刷新被限流，数据为旧值。 */
  stale?: boolean;
  observedAt?: string;
};

/** 将剩余比例换算为已用百分比（整数，限定在 0~100）。 */
export function usedPercent(remaining: number): number {
  return Math.floor(Math.max(0, Math.min(100, 100 - remaining * 100)));
}

/**
 * 生成中文重置时间文案：日期型（YYYY-MM-DD）为“X月X日重置”，
 * 时间型为“X月X日 HH:mm 重置”（24 小时制）；无效时间返回空字符串。
 */
export function resetLabel(
  value?: string,
  locale = "zh-CN",
  timeZone?: string,
): string {
  if (!value) return "";
  const date = new Date(value);
  if (!Number.isFinite(date.getTime()) || date.getUTCFullYear() < 2000)
    return "";
  const dateOnly = /^\d{4}-\d{2}-\d{2}$/.test(value);
  const day = new Intl.DateTimeFormat(locale, {
    month: "long",
    day: "numeric",
    timeZone: dateOnly ? "UTC" : timeZone,
  }).format(date);
  if (dateOnly) return `${day}重置`;
  const time = new Intl.DateTimeFormat(locale, {
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
    timeZone,
  }).format(date);
  return `${day} ${time} 重置`;
}
/** 展开所有分组的额度窗口，忽略缺失或非有限数值的项（未知额度不视为 0）。 */
export function buckets(quota: Quota): Bucket[] {
  return (quota.groups ?? [])
    .flatMap((g) => g.buckets ?? [])
    .filter(
      (b) =>
        typeof b.remainingFraction === "number" &&
        Number.isFinite(b.remainingFraction),
    );
}
