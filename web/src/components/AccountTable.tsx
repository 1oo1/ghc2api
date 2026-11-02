// 账号列表：每行汇总一个账号的状态、额度、统计、优先级与代理，只做展示；
// 点击行打开该账号的卡片进行操作；键盘用户可聚焦账号名按钮，按回车/空格打开。
// 数值列可点击表头排序（降序 → 升序 → 默认顺序），排序状态只保存在本组件中。
import { useState } from "react";
import type { AccountView } from "../api";
import {
  accountStatus,
  accountStatusText,
  coolingLabel,
  formatTime,
  isCooling,
  quotaWarnings,
} from "../account";
import {
  buckets,
  creditsUsed,
  HIGH_USAGE_PERCENT,
  planLabel,
  resetLabel,
  usedPercent,
} from "../quota";
import { proxyLabel } from "../proxy";
import { nextSort, sortAccounts, type SortKey, type SortState } from "../sort";

export function AccountTable({
  accounts,
  onSelect,
}: {
  accounts: AccountView[];
  /** 打开指定账号的卡片。 */
  onSelect: (id: string) => void;
}) {
  const now = Date.now();
  const [sort, setSort] = useState<SortState>(null);
  const sorted = sortAccounts(accounts, sort);
  /** 可排序的表头。 */
  const sortable = (key: SortKey, label: string) => {
    const direction = sort?.key === key ? sort.direction : undefined;
    return (
      <th aria-sort={direction === "desc" ? "descending" : direction === "asc" ? "ascending" : "none"}>
        <button className="sort-header" onClick={() => setSort(nextSort(sort, key))}>
          {label}
          <span aria-hidden="true">{direction === "desc" ? "↓" : direction === "asc" ? "↑" : "↕"}</span>
        </button>
      </th>
    );
  };
  return (
    <section className="panel table-wrap">
      <table className="account-table">
        <thead>
          <tr>
            <th>账号</th>
            <th>状态</th>
            {sortable("quota", "额度")}
            {sortable("requests", "请求")}
            {sortable("errors", "错误")}
            {sortable("lastUsed", "最近使用")}
            {sortable("priority", "优先级")}
            <th>代理</th>
          </tr>
        </thead>
        <tbody>
          {sorted.map((a) => (
            <tr key={a.id} title="点击查看详情与操作" onClick={() => onSelect(a.id)}>
              <td>
                <div className="account-cell">
                  <span className="avatar">{a.user.slice(0, 2).toUpperCase()}</span>
                  <div>
                    {/* 按钮本身不处理点击：键盘激活产生的 click 冒泡到行，由行统一打开卡片。 */}
                    <button className="link account-name" aria-haspopup="dialog">
                      {a.user}
                    </button>
                    <small>{planLabel(a.quota)}</small>
                  </div>
                </div>
              </td>
              <td>
                <StatusCell account={a} now={now} />
              </td>
              <td>
                <QuotaCell account={a} />
              </td>
              <td>{a.stats.requests.toLocaleString("zh-CN")}</td>
              <td>{a.stats.errors.toLocaleString("zh-CN")}</td>
              <td>{formatTime(a.stats.last_used_at)}</td>
              <td>{a.priority}</td>
              <td>{proxyLabel(a.proxy)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}

/** 状态徽标；冷却中时下方附冷却截止时间，禁用且冷却中时另附冷却原因（徽标只显示「禁用中」）。 */
function StatusCell({ account, now }: { account: AccountView; now: number }) {
  const status = accountStatus(account, now);
  const until = formatTime(account.cooldown_until);
  return (
    <>
      <span className={`badge ${status}`}>{accountStatusText(account, now)}</span>
      {isCooling(account, now) && (
        <small className="cell-note">
          {status === "disabled" && `${coolingLabel(account.cooldown_reason)} · `}至 {until}
        </small>
      )}
    </>
  );
}

/** 紧凑的额度：每个窗口一行（名称、进度条、已用百分比、重置时间），以及已用 Credits 与额度提示（过长时省略，悬停看全文）。 */
function QuotaCell({ account }: { account: AccountView }) {
  const windows = account.quota ? buckets(account.quota) : [];
  const credits = creditsUsed(account.quota);
  const warnings = quotaWarnings(account);
  return (
    <div className="mini-quota">
      {windows.map((b, i) => {
        const used = usedPercent(b.remainingFraction);
        return (
          <div className="mini-quota-row" key={`${b.window}-${i}`}>
            <span>{b.description ?? b.window}</span>
            <div className="meter">
              <i className={used >= HIGH_USAGE_PERCENT ? "high" : ""} style={{ width: `${used}%` }} />
            </div>
            <b>{used}%</b>
            <small>{resetLabel(b.resetTime)}</small>
          </div>
        );
      })}
      {credits !== undefined && (
        <small>已用 Credits：{credits.toLocaleString("zh-CN")}</small>
      )}
      {windows.length === 0 && credits === undefined && (
        <span className="muted">暂无额度数据</span>
      )}
      {warnings.map((w) => (
        <small className="quota-warning" key={w} title={w}>
          ⚠ {w}
        </small>
      ))}
    </div>
  );
}
